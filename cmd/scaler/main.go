// Package main is the composition root for gh-runner-scaler.
// This is the ONLY file that imports concrete provider packages.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Cordtus/gh-runner-scaler/internal/config"
	"github.com/Cordtus/gh-runner-scaler/internal/daemon"
	"github.com/Cordtus/gh-runner-scaler/internal/engine"
	"github.com/Cordtus/gh-runner-scaler/internal/iface"
	"github.com/Cordtus/gh-runner-scaler/provider/fsstate"
	ghprovider "github.com/Cordtus/gh-runner-scaler/provider/github"
	"github.com/Cordtus/gh-runner-scaler/provider/loki"
	lxdprovider "github.com/Cordtus/gh-runner-scaler/provider/lxd"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		os.Args = append(os.Args, "daemon")
	}

	switch os.Args[1] {
	case "daemon":
		runDaemon(os.Args[2:])
	case "reconcile":
		runReconcile(os.Args[2:])
	case "version":
		fmt.Println("gh-runner-scaler", version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\nUsage: gh-runner-scaler [daemon|reconcile|version]\n", os.Args[1])
		os.Exit(1)
	}
}

func runDaemon(args []string) {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	configPath := fs.String("config", "config.toml", "path to TOML config file")
	fs.Parse(args)

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	runtime, cache, ci, metrics, state, err := wireProviders(cfg, log)
	if err != nil {
		log.Error("failed to initialize providers", "error", err)
		os.Exit(1)
	}

	reconciler := engine.NewReconciler(
		engine.ReconcilerConfig{
			Prefix:         cfg.Scaler.Prefix,
			MaxAutoRunners: cfg.Scaler.MaxAutoRunners,
			IdleTimeout:    cfg.Scaler.IdleTimeout.Duration,
			Labels:         cfg.Scaler.Labels,
			RunnerWorkDir:  cfg.Scaler.RunnerWorkDir,
			CacheEnabled:   cfg.Cache.Enabled,
		},
		runtime, cache, ci, state, log,
	)

	d := daemon.New(
		daemon.Config{
			Prefix:           cfg.Scaler.Prefix,
			PollInterval:     cfg.Scaler.PollInterval.Duration,
			WebhookEnabled:   cfg.Webhook.Enabled,
			WebhookPort:      cfg.Webhook.Port,
			WebhookDebounce:  cfg.Webhook.Debounce.Duration,
			MetricsEnabled:   cfg.Metrics.Enabled,
			MetricsInterval:  cfg.Metrics.Interval.Duration,
			CollectWorkflows: cfg.Metrics.CollectWorkflows,
			CollectHost:      cfg.Metrics.CollectHost,
			CachePool:        cfg.Cache.Pool,
			SyncRepos:        cfg.Webhook.SyncRepos,
		},
		reconciler, ci, metrics, runtime, log,
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := d.Run(ctx); err != nil {
		log.Error("daemon exited with error", "error", err)
		os.Exit(1)
	}
}

func runReconcile(args []string) {
	fs := flag.NewFlagSet("reconcile", flag.ExitOnError)
	configPath := fs.String("config", "config.toml", "path to TOML config file")
	fs.Parse(args)

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	runtime, cache, ci, _, state, err := wireProviders(cfg, log)
	if err != nil {
		log.Error("failed to initialize providers", "error", err)
		os.Exit(1)
	}

	reconciler := engine.NewReconciler(
		engine.ReconcilerConfig{
			Prefix:         cfg.Scaler.Prefix,
			MaxAutoRunners: cfg.Scaler.MaxAutoRunners,
			IdleTimeout:    cfg.Scaler.IdleTimeout.Duration,
			Labels:         cfg.Scaler.Labels,
			RunnerWorkDir:  cfg.Scaler.RunnerWorkDir,
			CacheEnabled:   cfg.Cache.Enabled,
		},
		runtime, cache, ci, state, log,
	)

	ctx := context.Background()
	if err := reconciler.Reconcile(ctx); err != nil {
		log.Error("reconcile failed", "error", err)
		os.Exit(1)
	}
}

// wireProviders constructs concrete provider implementations based on config.
// This is the only place in the codebase that knows about concrete types.
func wireProviders(cfg *config.Config, log *slog.Logger) (
	iface.ContainerRuntime,
	iface.CacheManager,
	iface.CIProvider,
	iface.MetricsBackend,
	iface.StateStore,
	error,
) {
	// Container runtime.
	var runtime iface.ContainerRuntime
	switch cfg.Container.Provider {
	case "lxd":
		r, err := lxdprovider.New(
			cfg.Container.LXD.Socket,
			cfg.Container.LXD.Remote,
			cfg.Container.LXD.RemoteURL,
			cfg.Container.LXD.RemoteCert,
			cfg.Container.LXD.RemoteKey,
			cfg.Container.Template,
		)
		if err != nil {
			return nil, nil, nil, nil, nil, fmt.Errorf("lxd runtime: %w", err)
		}
		runtime = r
	default:
		return nil, nil, nil, nil, nil, fmt.Errorf("unsupported container provider: %s", cfg.Container.Provider)
	}

	// Cache manager (optional).
	var cache iface.CacheManager
	if cfg.Cache.Enabled {
		switch cfg.Container.Provider {
		case "lxd":
			// The LXD cache manager needs the concrete runtime for API access.
			lxdRT, ok := runtime.(*lxdprovider.Runtime)
			if !ok {
				return nil, nil, nil, nil, nil, fmt.Errorf("cache requires lxd runtime")
			}
			cache = lxdprovider.NewCacheManager(lxdRT, cfg.Cache.Pool, cfg.Cache.Volume, cfg.Cache.Symlinks)
		}
	}

	// CI provider.
	var ci iface.CIProvider
	switch cfg.CI.Provider {
	case "github":
		p := ghprovider.New(cfg.CI.GitHub.Token, cfg.CI.Org, cfg.Scaler.Prefix)
		p.SetValidator(cfg.CI.GitHub.WebhookSecret)
		p.SetWorkflowRepoBatchSize(cfg.Metrics.WorkflowRepoBatchSize)
		ci = p
	default:
		return nil, nil, nil, nil, nil, fmt.Errorf("unsupported CI provider: %s", cfg.CI.Provider)
	}

	// Metrics backend (optional).
	var metrics iface.MetricsBackend
	if cfg.Metrics.Enabled {
		metrics = loki.New(
			cfg.Metrics.Loki.PushURL,
			cfg.Metrics.Loki.Username,
			cfg.Metrics.Loki.APIKey,
			cfg.CI.Org,
		)
	}

	// State store.
	var state iface.StateStore
	switch cfg.State.Provider {
	case "filesystem":
		s, err := fsstate.New(cfg.State.Filesystem.Dir)
		if err != nil {
			return nil, nil, nil, nil, nil, fmt.Errorf("filesystem state: %w", err)
		}
		state = s
	default:
		return nil, nil, nil, nil, nil, fmt.Errorf("unsupported state provider: %s", cfg.State.Provider)
	}

	_ = log // available for provider-level logging in the future
	return runtime, cache, ci, metrics, state, nil
}
