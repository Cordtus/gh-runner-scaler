// Package engine contains the scaling logic. It depends only on interfaces
// from iface/ and types from domain/ -- never on concrete providers.
package engine

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Cordtus/gh-runner-scaler/internal/domain"
	"github.com/Cordtus/gh-runner-scaler/internal/iface"
)

// ReconcilerConfig holds the tuning parameters for the reconciler.
type ReconcilerConfig struct {
	Prefix         string
	MaxAutoRunners int
	IdleTimeout    time.Duration
	Labels         string
	RunnerWorkDir  string
	CacheEnabled   bool
	ReadyCheck     []string       // command to poll inside container (e.g. ["test", "-f", "/home/runner/config.sh"])
	ReadyTimeout   time.Duration  // max wait for container boot
}

// Reconciler implements the scale-up/scale-down decision loop.
type Reconciler struct {
	cfg     ReconcilerConfig
	runtime iface.ContainerRuntime
	cache   iface.CacheManager // nil if cache is disabled
	ci      iface.CIProvider
	state   iface.StateStore
	log     *slog.Logger
}

// NewReconciler creates a Reconciler wired to the given providers.
func NewReconciler(
	cfg ReconcilerConfig,
	runtime iface.ContainerRuntime,
	cache iface.CacheManager,
	ci iface.CIProvider,
	state iface.StateStore,
	log *slog.Logger,
) *Reconciler {
	if log == nil {
		log = slog.Default()
	}
	return &Reconciler{
		cfg:     cfg,
		runtime: runtime,
		cache:   cache,
		ci:      ci,
		state:   state,
		log:     log.With("component", "reconciler"),
	}
}

// Reconcile performs a single scale-up/scale-down pass.
// This is the direct port of the bash scaler's main() function.
func (r *Reconciler) Reconcile(ctx context.Context) error {
	// 1. Query runners from CI provider.
	runners, err := r.ci.ListRunners(ctx)
	if err != nil {
		return fmt.Errorf("listing runners: %w", err)
	}

	// 2. Build snapshot.
	snap := buildSnapshot(runners, r.cfg.Prefix)
	r.log.Info("runner state",
		"total", snap.Total, "busy", snap.Busy, "idle", snap.Idle,
		"auto", snap.Auto, "permanent", snap.Permanent,
	)

	// 3. List auto-scaled containers from runtime.
	containers, err := r.runtime.ListContainers(ctx, r.cfg.Prefix)
	if err != nil {
		return fmt.Errorf("listing containers: %w", err)
	}
	autoCount := len(containers)

	// 4. Scale up: all runners busy and under the cap.
	if snap.Idle == 0 && autoCount < r.cfg.MaxAutoRunners {
		r.log.Info("all runners busy, scaling up")
		if err := r.scaleUp(ctx); err != nil {
			r.log.Error("scale-up failed", "error", err)
		}
	}

	// 5. Scale down: iterate auto containers.
	now := time.Now()
	for _, c := range containers {
		status, err := r.runtime.GetContainerStatus(ctx, c.Name)
		if err != nil {
			r.log.Warn("failed to get container status", "container", c.Name, "error", err)
			continue
		}

		switch {
		case status == domain.StatusStopped:
			// Ephemeral runner finished its job and stopped.
			r.log.Info("container stopped (job complete)", "container", c.Name)
			r.scaleDown(ctx, c.Name, runners)

		case isRunnerBusy(c.Name, runners):
			r.state.SetLastActive(ctx, c.Name, now)

		case !hasRunner(c.Name, runners):
			// Container exists but has no registered runner -- orphaned.
			// This catches containers left behind by crashed scalers,
			// failed config.sh, or manual intervention.
			r.log.Info("orphaned container (no registered runner)", "container", c.Name)
			r.scaleDown(ctx, c.Name, runners)

		default:
			// Container is running with a registered idle runner.
			lastActive, err := r.state.GetLastActive(ctx, c.Name)
			if err != nil {
				// No state file -- initialize it.
				r.state.SetLastActive(ctx, c.Name, now)
				continue
			}
			idleDur := now.Sub(lastActive)
			if idleDur >= r.cfg.IdleTimeout {
				r.log.Info("container idle past timeout",
					"container", c.Name, "idle", idleDur.Round(time.Second),
				)
				r.scaleDown(ctx, c.Name, runners)
			}
		}
	}

	return nil
}

// scaleUp provisions a new ephemeral runner container.
// Preserves the full bash scaler sequence: clone -> cache attach -> start ->
// wait ready -> symlinks -> config.sh --ephemeral -> svc.sh install+start -> track state.
func (r *Reconciler) scaleUp(ctx context.Context) error {
	name, err := r.nextName(ctx)
	if err != nil {
		return fmt.Errorf("determining next name: %w", err)
	}

	token, err := r.ci.GetRegistrationToken(ctx)
	if err != nil {
		return fmt.Errorf("getting registration token: %w", err)
	}

	r.log.Info("scaling up", "container", name)

	// Clone template.
	if err := r.runtime.CloneFromTemplate(ctx, name); err != nil {
		return fmt.Errorf("cloning template: %w", err)
	}

	// From here on, any failure must clean up the container.
	cleanup := func() {
		r.runtime.StopContainer(ctx, name)
		r.runtime.DeleteContainer(ctx, name)
	}

	// Attach cache volume (optional).
	if r.cache != nil && r.cfg.CacheEnabled {
		if err := r.cache.AttachCache(ctx, name); err != nil {
			r.log.Warn("cache attach failed", "container", name, "error", err)
			// Non-fatal: continue without cache.
		}
	}

	// Start container.
	if err := r.runtime.StartContainer(ctx, name); err != nil {
		cleanup()
		return fmt.Errorf("starting container: %w", err)
	}

	// Wait for boot.
	readyCheck := r.cfg.ReadyCheck
	if len(readyCheck) == 0 {
		readyCheck = []string{"test", "-f", "/home/runner/config.sh"}
	}
	timeout := r.cfg.ReadyTimeout
	if timeout == 0 {
		timeout = 90 * time.Second
	}
	if err := r.runtime.WaitForReady(ctx, name, readyCheck, timeout); err != nil {
		cleanup()
		return fmt.Errorf("container not ready: %w", err)
	}

	// Setup cache symlinks (optional).
	if r.cache != nil && r.cfg.CacheEnabled {
		if err := r.cache.SetupCacheSymlinks(ctx, name); err != nil {
			r.log.Warn("cache symlink setup failed", "container", name, "error", err)
		}
	}

	// Configure runner as ephemeral.
	configCmd := []string{
		"su", "-", "runner", "-c",
		fmt.Sprintf(
			"./config.sh --url %s --token '%s' --name '%s' --labels '%s' --work %s --unattended --ephemeral --replace",
			r.ci.RegistrationURL(), token, name, r.cfg.Labels, r.cfg.RunnerWorkDir,
		),
	}
	if _, err := r.runtime.ExecCommand(ctx, name, configCmd); err != nil {
		cleanup()
		return fmt.Errorf("runner config failed: %w", err)
	}

	// Install and start the runner service.
	svcCmd := []string{"bash", "-c", "cd /home/runner && ./svc.sh install runner && ./svc.sh start"}
	if _, err := r.runtime.ExecCommand(ctx, name, svcCmd); err != nil {
		cleanup()
		return fmt.Errorf("runner service start failed: %w", err)
	}

	// Track state.
	if err := r.state.Create(ctx, name); err != nil {
		r.log.Warn("failed to create state", "container", name, "error", err)
	}

	r.log.Info("scaled up", "container", name)
	return nil
}

// scaleDown tears down a container with belt-and-suspenders deregistration.
func (r *Reconciler) scaleDown(ctx context.Context, name string, runners []domain.Runner) {
	r.log.Info("scaling down", "container", name)

	// Stop runner service (best-effort).
	r.runtime.ExecCommand(ctx, name, []string{"bash", "-c", "cd /home/runner && ./svc.sh stop"})

	// Deregister via config.sh remove (best-effort).
	removeToken, err := r.ci.GetRemoveToken(ctx)
	if err == nil && removeToken != "" {
		cmd := []string{"su", "-", "runner", "-c", fmt.Sprintf("./config.sh remove --token '%s'", removeToken)}
		r.runtime.ExecCommand(ctx, name, cmd)
	}

	// Belt-and-suspenders: delete via API.
	for _, runner := range runners {
		if runner.Name == name {
			if err := r.ci.DeleteRunner(ctx, runner.ID); err != nil {
				r.log.Warn("API runner delete failed", "container", name, "error", err)
			} else {
				r.log.Info("deleted runner from CI platform", "container", name, "id", runner.ID)
			}
			break
		}
	}

	// Stop and delete container.
	r.runtime.StopContainer(ctx, name)
	r.runtime.DeleteContainer(ctx, name)

	// Clean up state.
	r.state.Delete(ctx, name)

	r.log.Info("scaled down", "container", name)
}

// nextName finds the next available container name (e.g. gh-runner-auto-1, -2, ...).
func (r *Reconciler) nextName(ctx context.Context) (string, error) {
	existing, err := r.runtime.ListContainers(ctx, r.cfg.Prefix)
	if err != nil {
		return "", err
	}

	used := make(map[string]bool, len(existing))
	for _, c := range existing {
		used[c.Name] = true
	}

	for i := 1; ; i++ {
		name := fmt.Sprintf("%s-%d", r.cfg.Prefix, i)
		if !used[name] {
			return name, nil
		}
	}
}

// buildSnapshot computes aggregate runner statistics.
func buildSnapshot(runners []domain.Runner, prefix string) domain.RunnerSnapshot {
	snap := domain.RunnerSnapshot{
		Total:   len(runners),
		Runners: runners,
	}
	for _, r := range runners {
		if r.Busy {
			snap.Busy++
		}
		if r.Status == "online" {
			snap.Online++
		}
		if strings.HasPrefix(r.Name, prefix) {
			snap.Auto++
		}
	}
	snap.Idle = snap.Total - snap.Busy
	snap.Offline = snap.Total - snap.Online
	snap.Permanent = snap.Total - snap.Auto
	return snap
}

func isRunnerBusy(containerName string, runners []domain.Runner) bool {
	for _, r := range runners {
		if r.Name == containerName && r.Busy {
			return true
		}
	}
	return false
}

func hasRunner(containerName string, runners []domain.Runner) bool {
	for _, r := range runners {
		if r.Name == containerName {
			return true
		}
	}
	return false
}
