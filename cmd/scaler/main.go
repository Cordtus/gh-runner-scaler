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
	"path/filepath"
	"strings"
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

const multiTargetMetricsLabel = "multi-target"

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

	logStore, err := daemon.NewLogStore(cfg.State.Filesystem.Dir)
	if err != nil {
		log.Error("failed to initialize log store", "error", err)
		os.Exit(1)
	}
	log = slog.New(daemon.NewLogHandler(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}), logStore))

	groups, metrics, err := wireRunnerGroups(cfg, log)
	if err != nil {
		log.Error("failed to initialize providers", "error", err)
		os.Exit(1)
	}

	d := daemon.NewWithRunnerGroups(daemonConfigFrom(cfg), groups, metrics, logStore, log)

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

	logStore, err := daemon.NewLogStore(cfg.State.Filesystem.Dir)
	if err != nil {
		log.Error("failed to initialize log store", "error", err)
		os.Exit(1)
	}
	log = slog.New(daemon.NewLogHandler(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}), logStore))

	groups, _, err := wireRunnerGroups(cfg, log)
	if err != nil {
		log.Error("failed to initialize providers", "error", err)
		os.Exit(1)
	}

	ctx := context.Background()
	for _, group := range groups {
		if err := group.Reconciler.Reconcile(ctx); err != nil {
			log.Error("reconcile failed", "runner_group", group.ID, "error", err)
			os.Exit(1)
		}
	}
}

func daemonConfigFrom(cfg *config.Config) daemon.Config {
	return daemon.Config{
		Prefix:           cfg.Scaler.Prefix,
		PollInterval:     cfg.Scaler.PollInterval.Duration,
		WebhookEnabled:   cfg.Webhook.Enabled,
		WebhookPort:      cfg.Webhook.Port,
		WebhookDebounce:  cfg.Webhook.Debounce.Duration,
		LogsToken:        cfg.Webhook.LogsToken,
		MetricsEnabled:   cfg.Metrics.Enabled,
		MetricsInterval:  cfg.Metrics.Interval.Duration,
		CollectWorkflows: cfg.Metrics.CollectWorkflows,
		CollectHost:      cfg.Metrics.CollectHost,
		CachePool:        cfg.Cache.Pool,
		StateDir:         cfg.State.Filesystem.Dir,
		SyncRepos:        cfg.Webhook.SyncRepos,
	}
}

func wireRunnerGroups(cfg *config.Config, log *slog.Logger) ([]daemon.RunnerGroup, iface.MetricsBackend, error) {
	classes, err := cfg.RunnerClassConfigs()
	if err != nil {
		return nil, nil, err
	}

	groups := make([]daemon.RunnerGroup, 0, len(classes))
	legacyStateDir := len(cfg.RunnerClasses) == 0
	githubProvidersByOrg := make(map[string]*ghprovider.Provider)
	for _, class := range classes {
		runtime, cache, err := wireRuntimeAndCache(cfg, class)
		if err != nil {
			return nil, nil, fmt.Errorf("runner class %s: %w", class.ID, err)
		}

		ci, err := wireCIProvider(cfg, class, githubProvidersByOrg)
		if err != nil {
			return nil, nil, fmt.Errorf("runner class %s: %w", class.ID, err)
		}

		stateDir := cfg.State.Filesystem.Dir
		if !legacyStateDir {
			stateDir = filepath.Join(stateDir, "runner-groups", class.ID)
		}
		state, err := wireState(cfg, stateDir)
		if err != nil {
			return nil, nil, fmt.Errorf("runner class %s: %w", class.ID, err)
		}

		classMetrics := wireMetricsBackend(cfg, class)
		reconciler := engine.NewReconciler(
			engine.ReconcilerConfig{
				Prefix:         class.Prefix,
				MaxAutoRunners: class.MaxAutoRunners,
				IdleTimeout:    class.IdleTimeout,
				Labels:         class.Labels,
				RunnerWorkDir:  class.RunnerWorkDir,
				CacheEnabled:   class.Cache.Enabled,
				CachePrune:     config.CachePrunePolicyFor(class.Cache),
			},
			runtime, cache, ci, state, log.With("runner_group", class.ID),
		)

		groups = append(groups, daemon.RunnerGroup{
			ID:          class.ID,
			Target:      class.TargetName(),
			RepoScoped:  class.RepoScoped(),
			Prefix:      class.Prefix,
			MatchLabels: class.MatchLabels,
			CachePool:   class.Cache.Pool,
			Reconciler:  reconciler,
			CI:          ci,
			Runtime:     runtime,
			Metrics:     classMetrics,
		})
	}

	return groups, wireMetricsBackendForTarget(cfg, daemonMetricsTarget(classes)), nil
}

func wireRuntimeAndCache(cfg *config.Config, class config.RunnerClass) (iface.ContainerRuntime, iface.CacheManager, error) {
	var runtime iface.ContainerRuntime
	switch cfg.Container.Provider {
	case "lxd":
		r, err := lxdprovider.New(
			cfg.Container.LXD.Socket,
			cfg.Container.LXD.Remote,
			cfg.Container.LXD.RemoteURL,
			cfg.Container.LXD.RemoteCert,
			cfg.Container.LXD.RemoteKey,
			class.Template,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("lxd runtime: %w", err)
		}
		runtime = r
	default:
		return nil, nil, fmt.Errorf("unsupported container provider: %s", cfg.Container.Provider)
	}

	var cache iface.CacheManager
	if class.Cache.Enabled {
		switch cfg.Container.Provider {
		case "lxd":
			lxdRT, ok := runtime.(*lxdprovider.Runtime)
			if !ok {
				return nil, nil, fmt.Errorf("cache requires lxd runtime")
			}
			cache = lxdprovider.NewCacheManager(lxdRT, class.Cache.Pool, class.Cache.Volume, class.Cache.Symlinks)
		}
	}
	return runtime, cache, nil
}

func wireCIProvider(cfg *config.Config, class config.RunnerClass, githubProvidersByOrg map[string]*ghprovider.Provider) (iface.CIProvider, error) {
	switch cfg.CI.Provider {
	case "github":
		var p *ghprovider.Provider
		var err error
		if class.RepoScoped() {
			p, err = ghprovider.NewForRepo(cfg.CI.GitHub.Token, class.Repo, class.Prefix)
		} else {
			p = ghprovider.New(cfg.CI.GitHub.Token, class.Org, class.Prefix)
		}
		if err != nil {
			return nil, err
		}
		targetCacheKey := githubTargetCacheKey(class)
		if shared := githubProvidersByOrg[targetCacheKey]; shared != nil {
			p.ShareRunnerCacheWith(shared)
		} else {
			githubProvidersByOrg[targetCacheKey] = p
		}
		p.SetValidator(cfg.CI.GitHub.WebhookSecret)
		p.SetWorkflowRepoBatchSize(cfg.Metrics.WorkflowRepoBatchSize)
		return p, nil
	default:
		return nil, fmt.Errorf("unsupported CI provider: %s", cfg.CI.Provider)
	}
}

func githubTargetCacheKey(class config.RunnerClass) string {
	if class.RepoScoped() {
		return strings.ToLower("repo:" + class.Repo)
	}
	return strings.ToLower("org:" + class.Org)
}

func wireMetricsBackend(cfg *config.Config, class config.RunnerClass) iface.MetricsBackend {
	return wireMetricsBackendForTarget(cfg, class.TargetName())
}

func wireMetricsBackendForTarget(cfg *config.Config, target string) iface.MetricsBackend {
	if !cfg.Metrics.Enabled {
		return nil
	}
	return loki.New(
		cfg.Metrics.Loki.PushURL,
		cfg.Metrics.Loki.Username,
		cfg.Metrics.Loki.APIKey,
		target,
	)
}

func daemonMetricsTarget(classes []config.RunnerClass) string {
	if len(classes) == 0 {
		return ""
	}
	target := classes[0].TargetName()
	for _, class := range classes[1:] {
		if !strings.EqualFold(class.TargetName(), target) {
			return multiTargetMetricsLabel
		}
	}
	return target
}

func wireState(cfg *config.Config, stateDir string) (iface.StateStore, error) {
	switch cfg.State.Provider {
	case "filesystem":
		s, err := fsstate.New(stateDir)
		if err != nil {
			return nil, fmt.Errorf("filesystem state: %w", err)
		}
		return s, nil
	default:
		return nil, fmt.Errorf("unsupported state provider: %s", cfg.State.Provider)
	}
}
