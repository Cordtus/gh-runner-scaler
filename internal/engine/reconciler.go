// Package engine contains the scaling logic. It depends only on interfaces
// from iface/ and types from domain/ -- never on concrete providers.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Cordtus/gh-runner-scaler/internal/domain"
	"github.com/Cordtus/gh-runner-scaler/internal/iface"
	"github.com/Cordtus/gh-runner-scaler/internal/runnerobs"
)

const runnerAvailabilityGrace = 2 * time.Minute
const runnerInstallDir = "/home/runner/actions-runner/current"

// ReconcilerConfig holds the tuning parameters for the reconciler.
type ReconcilerConfig struct {
	Prefix         string
	Baseline       bool
	BaselineName   string
	MaxAutoRunners int
	IdleTimeout    time.Duration
	Labels         string
	RunnerWorkDir  string
	CacheEnabled   bool
	CachePrune     domain.CachePrunePolicy
	ReadyCheck     []string      // command to poll inside container (e.g. ["test", "-f", "/home/runner/config.sh"])
	ReadyTimeout   time.Duration // max wait for container boot
	Observability  *runnerobs.Bootstrapper
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

type reconcilePass struct {
	removeToken        string
	removeTokenFetched bool
}

type scaleDownResult struct {
	deleted bool
	err     error
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
	return r.ReconcileDemand(ctx, domain.CapacityDemand{QueuedJobs: 1})
}

// ReconcileDemand maintains the runner class and provisions overflow only for
// persisted queued work. A zero demand snapshot never creates overflow.
func (r *Reconciler) ReconcileDemand(ctx context.Context, demand domain.CapacityDemand) error {
	// 1. Query runners from CI provider.
	runners, err := r.ci.ListRunners(ctx)
	if err != nil {
		return fmt.Errorf("listing runners: %w", err)
	}

	baselinePending := 0
	if r.cfg.Baseline {
		pending, ensureErr := r.ensureBaseline(ctx, runners)
		if ensureErr != nil {
			r.log.Error("baseline maintenance failed", "event_type", "baseline", "action", "failed", "error", ensureErr)
		} else {
			baselinePending = pending
		}
	}

	// 2. Build snapshot.
	snap := buildSnapshot(runners, r.cfg.Prefix)
	availableOnline := AvailableRunnerCountForLabels(runners, r.cfg.Labels)
	r.log.Debug("runner state",
		"event_type", "runner_state",
		"total", snap.Total, "busy", snap.Busy, "idle", snap.Idle,
		"auto", snap.Auto, "permanent", snap.Permanent, "available_online", availableOnline,
	)

	// 3. List auto-scaled containers from runtime.
	containers, err := r.runtime.ListContainers(ctx, r.cfg.Prefix)
	if err != nil {
		return fmt.Errorf("listing containers: %w", err)
	}
	autoCount := len(containers)
	if availableOnline == 0 {
		containers = r.refreshContainerStatuses(ctx, containers)
	}

	// 4. Scale down: iterate auto containers.
	now := time.Now()
	pass := &reconcilePass{}
	removed := make(map[string]bool)
	pendingCapacity := 0
	handleScaleDown := func(name string) {
		result := r.scaleDown(ctx, name, runners, pass)
		if result.deleted {
			autoCount--
			removed[name] = true
		}
		if result.err != nil {
			r.log.Warn("scale-down cleanup encountered errors", "container", name, "error", result.err)
		}
	}
	for i, c := range containers {
		status := c.Status
		if status == domain.StatusUnknown || (availableOnline > 0 && status == domain.StatusStopped) {
			status, err = r.refreshContainerStatus(ctx, &containers[i])
			if err != nil {
				r.log.Warn("failed to get container status", "container", c.Name, "error", err)
				continue
			}
			c.Status = status
		}

		runner, runnerFound := findRunner(c.Name, runners)
		switch {
		case status == domain.StatusStopped:
			// Ephemeral runner finished its job and stopped.
			r.log.Info("container stopped (job complete)", "event_type", "scale_down", "action", "eligible", "container", c.Name, "runner", c.Name, "detail", "job complete")
			handleScaleDown(c.Name)

		case runnerFound && runner.Busy:
			r.state.SetLastActive(ctx, c.Name, now)

		case !runnerFound:
			// Container is running but has no registered runner. For ephemeral
			// runners this is the normal post-job state (the runner deregisters
			// itself after completing), so tearing down immediately can cancel a
			// just-finished job before GitHub records it as completed. Give the
			// container a grace period since it was last active before treating it
			// as orphaned. Genuinely orphaned containers (crashed scaler, failed
			// config.sh, manual intervention) are still cleaned up after the grace.
			lastActive, err := r.state.GetLastActive(ctx, c.Name)
			if err != nil {
				r.state.SetLastActive(ctx, c.Name, now)
				lastActive = now
			}
			if now.Sub(lastActive) < r.pendingAvailabilityGrace() {
				continue
			}
			r.log.Info("orphaned container (no registered runner)", "event_type", "scale_down", "action", "eligible", "container", c.Name, "runner", c.Name, "detail", "orphaned container")
			handleScaleDown(c.Name)

		default:
			// Container is running with a registered idle runner.
			lastActive, err := r.state.GetLastActive(ctx, c.Name)
			if err != nil {
				// No state file -- initialize it.
				r.state.SetLastActive(ctx, c.Name, now)
				lastActive = now
			}
			idleDur := now.Sub(lastActive)
			if pendingRunnerCapacity(status, runner, r.cfg.Labels, idleDur, r.pendingAvailabilityGrace()) {
				pendingCapacity++
			}
			if idleDur >= r.cfg.IdleTimeout {
				r.log.Info("container idle past timeout",
					"event_type", "scale_down", "action", "eligible", "container", c.Name, "runner", c.Name, "idle", idleDur.Round(time.Second),
				)
				handleScaleDown(c.Name)
			}
		}
	}

	// 5. Scale up only when persisted queued demand exceeds idle and
	// provisioning capacity.
	needed := demand.QueuedJobs - availableOnline - pendingCapacity - baselinePending
	for needed > 0 && autoCount < r.cfg.MaxAutoRunners {
		r.log.Info("queued demand requires overflow", "event_type", "scale_up", "action", "requested", "queued_jobs", demand.QueuedJobs, "needed", needed)
		name, err := r.scaleUp(ctx, filterRemovedContainers(containers, removed))
		if err != nil {
			r.log.Error("scale-up failed", "event_type", "scale_up", "action", "failed", "error", err)
			break
		}
		containers = append(containers, domain.Container{Name: name, Status: domain.StatusRunning})
		autoCount++
		needed--
	}

	return nil
}

// scaleUp provisions a new ephemeral runner container.
// Preserves the full bash scaler sequence: clone -> cache attach -> start ->
// wait ready -> symlinks -> config.sh --ephemeral -> svc.sh install+start -> track state.
func (r *Reconciler) scaleUp(ctx context.Context, existing []domain.Container) (string, error) {
	r.suspendRunnerInventoryCache()
	token, err := r.ci.GetRegistrationToken(ctx)
	if err != nil {
		r.resumeRunnerInventoryCache()
		return "", fmt.Errorf("getting registration token: %w", err)
	}
	defer r.resumeRunnerInventoryCache()

	name, err := r.cloneWithFreshName(ctx, existing)
	if err != nil {
		return "", err
	}
	if err := r.configureClonedRunner(ctx, name, token, true); err != nil {
		return "", err
	}
	return name, nil
}

func (r *Reconciler) configureClonedRunner(ctx context.Context, name, token string, ephemeral bool) error {
	r.log.Info("scaling up", "event_type", "scale_up", "action", "started", "container", name, "runner", name)

	// Attach cache volume (optional).
	if r.cache != nil && r.cfg.CacheEnabled {
		if err := r.cache.AttachCache(ctx, name); err != nil {
			r.log.Warn("cache attach failed", "container", name, "error", err)
			// Non-fatal: continue without cache.
		}
	}

	// Start container.
	if err := r.runtime.StartContainer(ctx, name); err != nil {
		return r.scaleUpFailure(name, "starting container", err, r.cleanupFailedScaleUp(ctx, name, false))
	}

	// Wait for boot.
	readyCheck := r.cfg.ReadyCheck
	if len(readyCheck) == 0 {
		readyCheck = []string{"test", "-x", runnerInstallDir + "/config.sh"}
	}
	timeout := r.cfg.ReadyTimeout
	if timeout == 0 {
		timeout = 90 * time.Second
	}
	if err := r.runtime.WaitForReady(ctx, name, readyCheck, timeout); err != nil {
		return r.scaleUpFailure(name, "container not ready", err, r.cleanupFailedScaleUp(ctx, name, true))
	}

	// Setup cache symlinks (optional).
	if r.cache != nil && r.cfg.CacheEnabled {
		if err := r.cache.SetupCacheSymlinks(ctx, name); err != nil {
			r.log.Warn("cache symlink setup failed", "container", name, "error", err)
		} else if err := r.cache.PruneCache(ctx, name, r.cfg.CachePrune); err != nil {
			r.log.Warn("cache prune failed", "container", name, "error", err)
		}
	}

	lifecycleFlag := ""
	if ephemeral {
		lifecycleFlag = " --ephemeral"
	}
	configCmd := []string{
		"su", "-", "runner", "-c",
		fmt.Sprintf(
			"cd %s && ./config.sh --url %s --token '%s' --name '%s' --labels '%s' --work %s --unattended%s --disableupdate --replace",
			runnerInstallDir, r.ci.RegistrationURL(), token, name, r.cfg.Labels, r.cfg.RunnerWorkDir, lifecycleFlag,
		),
	}
	if _, err := r.runtime.ExecCommand(ctx, name, configCmd); err != nil {
		return r.scaleUpFailure(name, "runner config failed", err, r.cleanupFailedScaleUp(ctx, name, true))
	}
	if r.cfg.Observability != nil {
		if err := r.cfg.Observability.Prepare(ctx, name); err != nil {
			r.log.Warn("runner log delivery disabled", "event_type", "runner_log_delivery", "action", "disabled", "container", name, "runner", name, "error", err)
		}
	}

	// Install and start the runner service.
	svcCmd := []string{"bash", "-c", "cd " + runnerInstallDir + " && ./svc.sh install runner && ./svc.sh start"}
	if _, err := r.runtime.ExecCommand(ctx, name, svcCmd); err != nil {
		return r.scaleUpFailure(name, "runner service start failed", err, r.cleanupFailedScaleUp(ctx, name, true))
	}

	// Track state.
	if err := r.state.Create(ctx, name); err != nil {
		r.log.Warn("failed to create state", "container", name, "error", err)
	}

	r.log.Info("scaled up", "event_type", "scale_up", "action", "completed", "container", name, "runner", name)
	return nil
}

func (r *Reconciler) ensureBaseline(ctx context.Context, runners []domain.Runner) (int, error) {
	if strings.TrimSpace(r.cfg.BaselineName) == "" {
		return 0, errors.New("baseline name is required")
	}
	for _, runner := range runners {
		if runner.Name != r.cfg.BaselineName {
			continue
		}
		if runner.Status == "online" {
			return 0, nil
		}
	}

	containers, err := r.runtime.ListContainers(ctx, r.cfg.BaselineName)
	if err != nil {
		return 0, fmt.Errorf("listing baseline container: %w", err)
	}
	for _, container := range containers {
		if container.Name != r.cfg.BaselineName {
			continue
		}
		if container.Status == domain.StatusRunning {
			lastActive, stateErr := r.state.GetLastActive(ctx, container.Name)
			if stateErr != nil {
				if err := r.state.SetLastActive(ctx, container.Name, time.Now()); err != nil {
					r.log.Warn("failed to initialize baseline state", "container", container.Name, "error", err)
				}
				return 1, nil
			}
			if time.Since(lastActive) < r.pendingAvailabilityGrace() {
				return 1, nil
			}
		}

		result := r.scaleDown(ctx, container.Name, runners, &reconcilePass{})
		if result.err != nil {
			r.log.Warn("stale baseline cleanup encountered errors", "container", container.Name, "error", result.err)
		}
		if !result.deleted {
			return 0, fmt.Errorf("stale baseline %s could not be deleted", container.Name)
		}
	}

	r.suspendRunnerInventoryCache()
	token, err := r.ci.GetRegistrationToken(ctx)
	if err != nil {
		r.resumeRunnerInventoryCache()
		return 0, fmt.Errorf("getting baseline registration token: %w", err)
	}
	defer r.resumeRunnerInventoryCache()
	if err := r.runtime.CloneFromTemplate(ctx, r.cfg.BaselineName); err != nil {
		return 0, fmt.Errorf("cloning baseline template: %w", err)
	}
	if err := r.configureClonedRunner(ctx, r.cfg.BaselineName, token, false); err != nil {
		return 0, err
	}
	r.log.Info("baseline ready", "event_type", "baseline", "action", "completed", "container", r.cfg.BaselineName)
	return 1, nil
}

func (r *Reconciler) cloneWithFreshName(ctx context.Context, existing []domain.Container) (string, error) {
	snapshot := append([]domain.Container(nil), existing...)
	for attempt := 0; attempt < 3; attempt++ {
		name := r.nextName(snapshot)
		if err := r.runtime.CloneFromTemplate(ctx, name); err != nil {
			if !isContainerNameConflict(err) {
				return "", fmt.Errorf("cloning template: %w", err)
			}

			latest, listErr := r.runtime.ListContainers(ctx, r.cfg.Prefix)
			if listErr != nil {
				return "", fmt.Errorf("cloning template: %w (refreshing names: %v)", err, listErr)
			}
			if len(latest) >= r.cfg.MaxAutoRunners {
				return "", fmt.Errorf("cloning template: max auto runners reached during concurrent scale-up")
			}

			snapshot = latest
			r.log.Warn("container name conflict during clone, retrying", "event_type", "scale_up", "action", "retry", "attempt", attempt+1, "error", err)
			continue
		}
		return name, nil
	}
	return "", fmt.Errorf("cloning template: exhausted unique name retries")
}

// scaleDown tears down a container with belt-and-suspenders deregistration.
func (r *Reconciler) scaleDown(ctx context.Context, name string, runners []domain.Runner, pass *reconcilePass) scaleDownResult {
	r.suspendRunnerInventoryCache()
	defer r.resumeRunnerInventoryCache()

	r.log.Info("scaling down", "event_type", "scale_down", "action", "started", "container", name, "runner", name)
	var errs []error
	result := scaleDownResult{}

	// Stop and uninstall the service before config.sh removes the registration.
	serviceStopped := true
	if _, err := r.runtime.ExecCommand(ctx, name, []string{"bash", "-c", "cd " + runnerInstallDir + " && ./svc.sh stop"}); err != nil {
		serviceStopped = false
		errs = append(errs, fmt.Errorf("stop runner service: %w", err))
	}
	if serviceStopped {
		if _, err := r.runtime.ExecCommand(ctx, name, []string{"bash", "-c", "cd " + runnerInstallDir + " && ./svc.sh uninstall"}); err != nil {
			errs = append(errs, fmt.Errorf("uninstall runner service: %w", err))
		}
	}

	// Deregister via config.sh remove (best-effort).
	removeToken, err := r.getRemoveToken(ctx, pass)
	if err != nil {
		errs = append(errs, fmt.Errorf("get remove token: %w", err))
	} else if removeToken != "" {
		cmd := []string{"su", "-", "runner", "-c", fmt.Sprintf("cd %s && ./config.sh remove --token '%s'", runnerInstallDir, removeToken)}
		if _, err := r.runtime.ExecCommand(ctx, name, cmd); err != nil {
			errs = append(errs, fmt.Errorf("runner config remove: %w", err))
		}
	}

	// Belt-and-suspenders: delete via API.
	for _, runner := range runners {
		if runner.Name == name {
			if runner.Busy {
				r.log.Info("skipping API runner delete because runner is busy", "container", name, "id", runner.ID)
				break
			}
			if err := r.ci.DeleteRunner(ctx, runner.ID); err != nil {
				r.log.Warn("API runner delete failed", "container", name, "error", err)
			} else {
				r.log.Info("deleted runner from CI platform", "container", name, "id", runner.ID)
			}
			break
		}
	}

	// Stop and delete container.
	if err := r.runtime.StopContainer(ctx, name); err != nil {
		errs = append(errs, fmt.Errorf("stop container: %w", err))
	}
	if err := r.runtime.DeleteContainer(ctx, name); err != nil {
		errs = append(errs, fmt.Errorf("delete container: %w", err))
	} else {
		result.deleted = true
	}

	// Clean up state.
	if err := r.state.Delete(ctx, name); err != nil {
		errs = append(errs, fmt.Errorf("delete state: %w", err))
	}

	if len(errs) > 0 {
		r.log.Warn("scaled down with cleanup errors", "event_type", "scale_down", "action", "failed", "container", name, "runner", name, "error", errors.Join(errs...))
		result.err = fmt.Errorf("scale-down %s: %w", name, errors.Join(errs...))
		return result
	}
	r.log.Info("scaled down", "event_type", "scale_down", "action", "completed", "container", name, "runner", name)
	return result
}

// nextName finds the next available container name (e.g. gh-runner-auto-1, -2, ...).
func (r *Reconciler) nextName(existing []domain.Container) string {
	used := make(map[string]bool, len(existing))
	for _, c := range existing {
		used[c.Name] = true
	}

	for i := 1; ; i++ {
		name := fmt.Sprintf("%s-%d", r.cfg.Prefix, i)
		if !used[name] {
			return name
		}
	}
}

func (r *Reconciler) refreshContainerStatuses(ctx context.Context, containers []domain.Container) []domain.Container {
	refreshed := append([]domain.Container(nil), containers...)
	for i := range refreshed {
		status, err := r.refreshContainerStatus(ctx, &refreshed[i])
		if err != nil {
			refreshed[i].Status = domain.StatusUnknown
			r.log.Warn("failed to refresh container status", "container", refreshed[i].Name, "error", err)
			continue
		}
		refreshed[i].Status = status
	}
	return refreshed
}

func (r *Reconciler) refreshContainerStatus(ctx context.Context, container *domain.Container) (domain.ContainerStatus, error) {
	status, err := r.runtime.GetContainerStatus(ctx, container.Name)
	if err != nil {
		return domain.StatusUnknown, err
	}
	container.Status = status
	return status, nil
}

func filterRemovedContainers(containers []domain.Container, removed map[string]bool) []domain.Container {
	if len(removed) == 0 {
		return append([]domain.Container(nil), containers...)
	}

	filtered := make([]domain.Container, 0, len(containers)-len(removed))
	for _, container := range containers {
		if removed[container.Name] {
			continue
		}
		filtered = append(filtered, container)
	}
	return filtered
}

func isContainerNameConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already exists") || strings.Contains(msg, "conflict")
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

func findRunner(containerName string, runners []domain.Runner) (domain.Runner, bool) {
	for _, r := range runners {
		if r.Name == containerName {
			return r, true
		}
	}
	return domain.Runner{}, false
}

func (r *Reconciler) pendingAvailabilityGrace() time.Duration {
	if r.cfg.IdleTimeout > 0 && r.cfg.IdleTimeout < runnerAvailabilityGrace {
		return r.cfg.IdleTimeout
	}
	return runnerAvailabilityGrace
}

func pendingRunnerCapacity(status domain.ContainerStatus, runner domain.Runner, labels string, idleDur, grace time.Duration) bool {
	if status != domain.StatusRunning || runner.Busy || runner.Status == "online" {
		return false
	}
	if grace <= 0 || idleDur > grace {
		return false
	}
	return runnerHasLabels(runner, labelsSet(labels))
}

// AvailableRunnerCount returns the number of runners that are both online and idle.
func AvailableRunnerCount(runners []domain.Runner) int {
	count := 0
	for _, r := range runners {
		if r.Status == "online" && !r.Busy {
			count++
		}
	}
	return count
}

// AvailableRunnerCountForLabels returns online idle runners matching all configured labels.
func AvailableRunnerCountForLabels(runners []domain.Runner, labels string) int {
	required := labelsSet(labels)
	count := 0
	for _, r := range runners {
		if r.Status == "online" && !r.Busy && runnerHasLabels(r, required) {
			count++
		}
	}
	return count
}

func runnerHasLabels(runner domain.Runner, required map[string]struct{}) bool {
	if len(required) == 0 {
		return true
	}
	if len(runner.Labels) == 0 {
		return false
	}

	available := make(map[string]struct{}, len(runner.Labels))
	for _, label := range runner.Labels {
		label = strings.ToLower(strings.TrimSpace(label))
		if label != "" {
			available[label] = struct{}{}
		}
	}
	for label := range required {
		if _, ok := available[label]; !ok {
			return false
		}
	}
	return true
}

func labelsSet(labels string) map[string]struct{} {
	parts := strings.Split(labels, ",")
	result := make(map[string]struct{}, len(parts))
	for _, label := range parts {
		label = strings.ToLower(strings.TrimSpace(label))
		if label != "" {
			result[label] = struct{}{}
		}
	}
	return result
}

func (r *Reconciler) suspendRunnerInventoryCache() {
	type runnerInventoryCacheSuspender interface {
		SuspendRunnerInventoryCache()
	}
	if cache, ok := r.ci.(runnerInventoryCacheSuspender); ok {
		cache.SuspendRunnerInventoryCache()
	}
}

func (r *Reconciler) resumeRunnerInventoryCache() {
	type runnerInventoryCacheResumer interface {
		ResumeRunnerInventoryCache()
	}
	if cache, ok := r.ci.(runnerInventoryCacheResumer); ok {
		cache.ResumeRunnerInventoryCache()
	}
}

func (r *Reconciler) getRemoveToken(ctx context.Context, pass *reconcilePass) (string, error) {
	if pass == nil {
		return r.ci.GetRemoveToken(ctx)
	}
	if pass.removeTokenFetched {
		return pass.removeToken, nil
	}

	removeToken, err := r.ci.GetRemoveToken(ctx)
	if err != nil {
		return "", err
	}
	pass.removeToken = removeToken
	pass.removeTokenFetched = true
	return pass.removeToken, nil
}

func (r *Reconciler) cleanupFailedScaleUp(ctx context.Context, name string, attemptStop bool) error {
	var errs []error
	if attemptStop {
		if err := r.runtime.StopContainer(ctx, name); err != nil {
			errs = append(errs, fmt.Errorf("stop container: %w", err))
		}
	}
	if err := r.runtime.DeleteContainer(ctx, name); err != nil {
		errs = append(errs, fmt.Errorf("delete container: %w", err))
	}
	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}

func (r *Reconciler) scaleUpFailure(name, action string, err, cleanupErr error) error {
	combined := scaleUpFailure(action, err, cleanupErr)
	r.log.Error("scale-up stage failed", "event_type", "scale_up", "action", "failed", "container", name, "runner", name, "detail", action, "error", combined)
	return combined
}

func scaleUpFailure(action string, err, cleanupErr error) error {
	if cleanupErr == nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	return errors.Join(
		fmt.Errorf("%s: %w", action, err),
		fmt.Errorf("cleanup after %s: %w", action, cleanupErr),
	)
}
