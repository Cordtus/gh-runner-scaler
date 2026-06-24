// Package daemon orchestrates the scaler's concurrent subsystems:
// reconciler poll loop, webhook HTTP server, and metrics push loop.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/Cordtus/gh-runner-scaler/internal/domain"
	"github.com/Cordtus/gh-runner-scaler/internal/engine"
	"github.com/Cordtus/gh-runner-scaler/internal/iface"
)

// Config holds daemon-level settings.
type Config struct {
	Prefix           string
	PollInterval     time.Duration
	WebhookEnabled   bool
	WebhookPort      int
	WebhookDebounce  time.Duration
	LogsToken        string
	MetricsEnabled   bool
	MetricsInterval  time.Duration
	CollectWorkflows bool
	CollectHost      bool
	CachePool        string            // for host metrics
	StateDir         string            // persisted daemon state (workflow metrics dedupe)
	SyncRepos        map[string]string // repo -> cache path
}

// RunnerGroup is one logical runner class managed by the daemon.
type RunnerGroup struct {
	ID          string
	Target      string
	RepoScoped  bool
	Prefix      string
	MatchLabels []string
	CachePool   string
	Reconciler  *engine.Reconciler
	CI          iface.CIProvider
	Runtime     iface.ContainerRuntime
	Metrics     iface.MetricsBackend
}

// Daemon runs all subsystems as goroutines in a single process.
type Daemon struct {
	cfg        Config
	reconciler *engine.Reconciler
	ci         iface.CIProvider
	metrics    iface.MetricsBackend
	runtime    iface.ContainerRuntime
	groups     []RunnerGroup
	logStore   *LogStore
	log        *slog.Logger

	triggerCh chan string
	debouncer *debouncer
	mu        sync.Mutex

	workflowMu            sync.Mutex
	workflowDelivered     map[string]struct{}
	workflowDeliveredKeys []string

	issueMu            sync.Mutex
	issueDelivered     map[string]struct{}
	issueDeliveredKeys []string

	analyticsMu            sync.Mutex
	logAnalyticsCached     bool
	logAnalyticsVersion    uint64
	cachedLifecycleMetrics domain.LifecycleMetrics
	cachedIssueLogEntries  []domain.LogEntry

	reconcileRunning          bool
	triggerSeq                uint64
	reconcileRuns             uint64
	reconcileFailures         uint64
	lastWebhookAt             time.Time
	lastWebhookType           string
	lastWebhookDetail         string
	lastReconcileStartedAt    time.Time
	lastReconcileFinishedAt   time.Time
	lastSuccessfulReconcileAt time.Time
	lastReconcileError        string
	lifecycleCtx              context.Context
}

type statusSnapshot struct {
	PollInterval              string `json:"poll_interval"`
	WebhookEnabled            bool   `json:"webhook_enabled"`
	MetricsEnabled            bool   `json:"metrics_enabled"`
	ReconcileRunning          bool   `json:"reconcile_running"`
	TriggerSequence           uint64 `json:"trigger_sequence"`
	ReconcileRuns             uint64 `json:"reconcile_runs"`
	ReconcileFailures         uint64 `json:"reconcile_failures"`
	LastWebhookAt             string `json:"last_webhook_at,omitempty"`
	LastWebhookType           string `json:"last_webhook_type,omitempty"`
	LastWebhookDetail         string `json:"last_webhook_detail,omitempty"`
	LastReconcileStartedAt    string `json:"last_reconcile_started_at,omitempty"`
	LastReconcileFinishedAt   string `json:"last_reconcile_finished_at,omitempty"`
	LastSuccessfulReconcileAt string `json:"last_successful_reconcile_at,omitempty"`
	LastReconcileError        string `json:"last_reconcile_error,omitempty"`
}

const (
	workflowMetricCacheLimit    = 20000
	workflowMetricRunFetchLimit = 20
)

// New creates a Daemon with all subsystems wired.
func New(
	cfg Config,
	reconciler *engine.Reconciler,
	ci iface.CIProvider,
	metrics iface.MetricsBackend,
	runtime iface.ContainerRuntime,
	logStore *LogStore,
	log *slog.Logger,
) *Daemon {
	group := RunnerGroup{
		ID:         "default",
		Target:     "",
		Prefix:     cfg.Prefix,
		CachePool:  cfg.CachePool,
		Reconciler: reconciler,
		CI:         ci,
		Runtime:    runtime,
		Metrics:    metrics,
	}
	return NewWithRunnerGroups(cfg, []RunnerGroup{group}, metrics, logStore, log)
}

// NewWithRunnerGroups creates a daemon that can route jobs to multiple logical runner classes.
func NewWithRunnerGroups(
	cfg Config,
	groups []RunnerGroup,
	metrics iface.MetricsBackend,
	logStore *LogStore,
	log *slog.Logger,
) *Daemon {
	if log == nil {
		log = slog.Default()
	}
	if len(groups) == 0 {
		panic("daemon requires at least one runner group")
	}
	groups = append([]RunnerGroup(nil), groups...)
	for i := range groups {
		if groups[i].Metrics == nil {
			groups[i].Metrics = metrics
		}
	}
	primary := groups[0]
	d := &Daemon{
		cfg:               cfg,
		reconciler:        primary.Reconciler,
		ci:                primary.CI,
		metrics:           metrics,
		runtime:           primary.Runtime,
		groups:            groups,
		logStore:          logStore,
		log:               log,
		triggerCh:         make(chan string, len(groups)+1),
		debouncer:         newDebouncer(),
		workflowDelivered: make(map[string]struct{}),
		issueDelivered:    make(map[string]struct{}),
		lifecycleCtx:      context.Background(),
	}
	d.loadWorkflowMetricCache()
	d.loadIssueEventCache()
	return d
}

// Run starts all subsystems and blocks until ctx is cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	var wg sync.WaitGroup

	d.mu.Lock()
	d.lifecycleCtx = ctx
	d.mu.Unlock()

	// Reconciler loop.
	wg.Add(1)
	go func() {
		defer wg.Done()
		d.reconcileLoop(ctx)
	}()

	// Webhook server.
	if d.cfg.WebhookEnabled {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.runWebhookServer(ctx)
		}()
	}

	// Metrics loop.
	if d.cfg.MetricsEnabled && d.metrics != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.metricsLoop(ctx)
		}()
	}

	d.log.Info("daemon started",
		"poll_interval", d.cfg.PollInterval,
		"webhook", d.cfg.WebhookEnabled,
		"metrics", d.cfg.MetricsEnabled,
	)

	<-ctx.Done()
	d.log.Info("shutting down")
	wg.Wait()
	return nil
}

// Trigger requests an immediate reconcile (called by webhook handler).
func (d *Daemon) Trigger() {
	d.triggerGroup("")
}

// TriggerGroup requests an immediate reconcile for a single runner group.
func (d *Daemon) TriggerGroup(groupID string) {
	d.triggerGroup(groupID)
}

func (d *Daemon) triggerGroup(groupID string) {
	d.mu.Lock()
	d.triggerSeq++
	running := d.reconcileRunning
	d.mu.Unlock()

	if running {
		return
	}

	select {
	case d.triggerCh <- groupID:
	default:
		// Channel full -- a trigger is already pending.
	}
}

// reconcileLoop runs the reconciler on a timer and on webhook triggers.
func (d *Daemon) reconcileLoop(ctx context.Context) {
	ticker := time.NewTicker(d.cfg.PollInterval)
	defer ticker.Stop()

	if ctx.Err() == nil {
		d.doReconcile(ctx)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.doReconcile(ctx)
		case groupID := <-d.triggerCh:
			d.doReconcileGroups(ctx, []string{groupID})
			ticker.Reset(d.cfg.PollInterval) // avoid redundant tick
		}
	}
}

// doReconcile runs a single reconcile pass with mutex protection.
func (d *Daemon) doReconcile(ctx context.Context) {
	d.doReconcileGroups(ctx, nil)
}

func (d *Daemon) doReconcileGroups(ctx context.Context, requested []string) {
	d.mu.Lock()
	if d.reconcileRunning {
		d.mu.Unlock()
		d.log.Debug("reconcile already running, skipping")
		return
	}
	d.reconcileRunning = true
	d.mu.Unlock()

	for {
		if ctx.Err() != nil {
			d.mu.Lock()
			d.reconcileRunning = false
			d.mu.Unlock()
			return
		}

		requested = mergeRequestedGroups(requested, d.drainTriggerSignals())

		startedAt := time.Now().UTC()

		d.mu.Lock()
		startSeq := d.triggerSeq
		d.lastReconcileStartedAt = startedAt
		d.mu.Unlock()

		requested = mergeRequestedGroups(requested, d.drainTriggerSignals())

		if ctx.Err() != nil {
			d.mu.Lock()
			d.reconcileRunning = false
			d.mu.Unlock()
			return
		}

		err := d.reconcileTargets(ctx, requested)
		finishedAt := time.Now().UTC()

		d.mu.Lock()
		d.reconcileRuns++
		d.lastReconcileFinishedAt = finishedAt
		if err != nil {
			d.reconcileFailures++
			d.lastReconcileError = err.Error()
		} else {
			d.lastSuccessfulReconcileAt = finishedAt
			d.lastReconcileError = ""
		}
		rerun := d.triggerSeq != startSeq && ctx.Err() == nil
		if !rerun {
			d.reconcileRunning = false
		}
		d.mu.Unlock()

		if err != nil {
			d.log.Error("reconcile failed", "error", err)
		}
		if !rerun {
			return
		}

		requested = nil
		d.log.Info("reconcile rerun requested")
	}
}

func (d *Daemon) reconcileTargets(ctx context.Context, requested []string) error {
	targets := d.selectedGroups(requested)
	var errs []error
	for _, group := range targets {
		if group.Reconciler == nil {
			continue
		}
		log := d.log.With("runner_group", group.ID)
		log.Info("reconcile group started")
		if err := group.Reconciler.Reconcile(ctx); err != nil {
			log.Error("reconcile group failed", "error", err)
			errs = append(errs, fmt.Errorf("%s: %w", group.ID, err))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

func (d *Daemon) selectedGroups(requested []string) []RunnerGroup {
	if len(requested) == 0 {
		return append([]RunnerGroup(nil), d.groups...)
	}
	ids := make(map[string]struct{}, len(requested))
	for _, id := range requested {
		if id == "" {
			return append([]RunnerGroup(nil), d.groups...)
		}
		ids[id] = struct{}{}
	}
	groups := make([]RunnerGroup, 0, len(ids))
	for _, group := range d.groups {
		if _, ok := ids[group.ID]; ok {
			groups = append(groups, group)
		}
	}
	return groups
}

func (d *Daemon) drainTriggerSignals() []string {
	var groups []string
	for {
		select {
		case groupID := <-d.triggerCh:
			groups = append(groups, groupID)
		default:
			return groups
		}
	}
}

func mergeRequestedGroups(current, pending []string) []string {
	if len(pending) == 0 {
		return current
	}
	if len(current) == 0 {
		return append([]string(nil), pending...)
	}
	merged := append([]string(nil), current...)
	return append(merged, pending...)
}

func (d *Daemon) recordWebhook(eventType string, event *domain.WebhookEvent) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.lastWebhookAt = time.Now().UTC()
	d.lastWebhookType = eventType
	if event != nil {
		d.lastWebhookDetail = event.Detail
	} else {
		d.lastWebhookDetail = ""
	}
}

func (d *Daemon) snapshotStatus() statusSnapshot {
	d.mu.Lock()
	defer d.mu.Unlock()

	return statusSnapshot{
		PollInterval:              d.cfg.PollInterval.String(),
		WebhookEnabled:            d.cfg.WebhookEnabled,
		MetricsEnabled:            d.cfg.MetricsEnabled,
		ReconcileRunning:          d.reconcileRunning,
		TriggerSequence:           d.triggerSeq,
		ReconcileRuns:             d.reconcileRuns,
		ReconcileFailures:         d.reconcileFailures,
		LastWebhookAt:             formatStatusTime(d.lastWebhookAt),
		LastWebhookType:           d.lastWebhookType,
		LastWebhookDetail:         d.lastWebhookDetail,
		LastReconcileStartedAt:    formatStatusTime(d.lastReconcileStartedAt),
		LastReconcileFinishedAt:   formatStatusTime(d.lastReconcileFinishedAt),
		LastSuccessfulReconcileAt: formatStatusTime(d.lastSuccessfulReconcileAt),
		LastReconcileError:        d.lastReconcileError,
	}
}

func (d *Daemon) currentLifecycleContext() context.Context {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.lifecycleCtx == nil {
		return context.Background()
	}
	return d.lifecycleCtx
}

func formatStatusTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func (d *Daemon) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		w.Write([]byte("ok"))
	}
}

func (d *Daemon) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	if err := json.NewEncoder(w).Encode(d.snapshotStatus()); err != nil {
		http.Error(w, "status encode error", http.StatusInternalServerError)
	}
}

// metricsLoop collects and pushes metrics on a timer.
func (d *Daemon) metricsLoop(ctx context.Context) {
	ticker := time.NewTicker(d.cfg.MetricsInterval)
	defer ticker.Stop()

	if ctx.Err() == nil {
		d.collectAndPush(ctx)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.collectAndPush(ctx)
		}
	}
}

// collectAndPush gathers runner, workflow, and host metrics and pushes them.
func (d *Daemon) collectAndPush(ctx context.Context) {
	d.collectRunnerAndHostMetrics(ctx)

	d.collectWorkflowMetrics(ctx)

	d.collectLogDerivedMetrics(ctx)
}

func (d *Daemon) collectRunnerAndHostMetrics(ctx context.Context) {
	for _, group := range d.groups {
		if group.CI == nil || group.Metrics == nil {
			continue
		}
		log := d.log.With("runner_group", group.ID)
		runners, runnerInventory, err := d.listRunnersForMetrics(ctx, group.CI)
		if err != nil {
			log.Error("failed to list runners for metrics", "error", err)
		} else if runnerInventory.Stale {
			log.Info("using stale runner inventory for metrics",
				"event_type", "runner_inventory", "action", "stale_metrics",
				"runner_inventory_age_s", runnerInventory.AgeS,
				"runner_inventory_error", runnerInventory.Error,
			)
		}

		var allContainers []domain.Container
		var autoContainers []domain.Container
		containersLoaded := false
		if group.Runtime != nil && (d.cfg.CollectHost || err == nil) {
			listed, listErr := group.Runtime.ListContainers(ctx, "")
			if listErr != nil {
				log.Warn("failed to list containers for metrics", "error", listErr)
			} else {
				containersLoaded = true
				allContainers = append([]domain.Container{}, listed...)
				autoContainers = filterContainersByPrefix(listed, group.Prefix)
			}
		}

		if err == nil {
			rm := buildRunnerMetrics(runners, autoContainers, group.CI)
			rm.GroupID = group.ID
			rm.RunnerInventoryStale = runnerInventory.Stale
			rm.RunnerInventoryAgeS = runnerInventory.AgeS
			rm.RunnerInventoryAt = runnerInventory.FetchedAt
			rm.RunnerInventoryError = runnerInventory.Error
			if err := group.Metrics.PushRunnerMetrics(ctx, rm); err != nil {
				log.Error("failed to push runner metrics", "error", err)
			}
		}

		if d.cfg.CollectHost {
			d.collectHostMetrics(ctx, group.ID, group.CachePool, group.Runtime, allContainers, autoContainers, containersLoaded)
		}
	}
}

func (d *Daemon) collectWorkflowMetrics(ctx context.Context) {
	if !d.cfg.CollectWorkflows {
		return
	}
	for _, group := range d.groups {
		if group.CI == nil || group.Metrics == nil {
			continue
		}
		log := d.log.With("runner_group", group.ID)
		wm, needsEnrichment, err := d.listWorkflowMetrics(ctx, group.CI)
		if err != nil {
			log.Warn("failed to collect workflow metrics", "error", err)
		} else if len(wm) > 0 {
			wm = d.filterNewWorkflowMetrics(wm)
		}
		if len(wm) > 0 && needsEnrichment {
			enriched, enrichErr := group.CI.EnrichWorkflowMetrics(ctx, wm)
			if len(enriched) > 0 {
				wm = enriched
			}
			if enrichErr != nil {
				log.Warn("failed to enrich workflow metrics", "error", enrichErr)
			}
		}
		if len(wm) > 0 {
			if err := group.Metrics.PushWorkflowMetrics(ctx, wm); err != nil {
				log.Error("failed to push workflow metrics", "error", err)
			} else {
				d.markWorkflowMetricsDelivered(wm)
			}
		}
	}
}

func (d *Daemon) listRunnersForMetrics(ctx context.Context, ci iface.CIProvider) ([]domain.Runner, domain.RunnerInventoryMeta, error) {
	if provider, ok := ci.(iface.RunnerInventoryMetricsProvider); ok {
		return provider.ListRunnersForMetrics(ctx)
	}

	runners, err := ci.ListRunners(ctx)
	return runners, domain.RunnerInventoryMeta{}, err
}

// collectHostMetrics attempts to gather host-level metrics from the runtime.
// This is provider-specific, so we use a type assertion.
func (d *Daemon) collectHostMetrics(ctx context.Context, groupID, cachePool string, runtime iface.ContainerRuntime, allContainers, autoContainers []domain.Container, containersLoaded bool) {
	type hostMetricsSnapshotProvider interface {
		HostMetricsFromContainers(cachePool string, containers []domain.Container) (domain.HostMetrics, error)
	}
	type hostMetricsProvider interface {
		HostMetrics(cachePool string) (domain.HostMetrics, error)
	}

	var (
		hm  domain.HostMetrics
		err error
		ok  bool
	)

	if hmp, snapshotOK := runtime.(hostMetricsSnapshotProvider); snapshotOK {
		hm, err = hmp.HostMetricsFromContainers(cachePool, allContainers)
		ok = true
	} else if hmp, hostOK := runtime.(hostMetricsProvider); hostOK {
		hm, err = hmp.HostMetrics(cachePool)
		ok = true
	}

	if ok {
		if err != nil {
			d.log.Warn("failed to collect host metrics", "error", err)
			return
		}

		if containersLoaded {
			running := 0
			stopped := 0
			for _, container := range autoContainers {
				switch container.Status {
				case domain.StatusRunning:
					running++
				case domain.StatusStopped:
					stopped++
				}
			}
			hm.RunnerContainersRunning = &running
			hm.RunnerContainersStopped = &stopped
		}
		hm.GroupID = groupID
		metrics := d.metrics
		for _, group := range d.groups {
			if group.ID == groupID {
				metrics = group.Metrics
				break
			}
		}
		if metrics == nil {
			return
		}
		if err := metrics.PushHostMetrics(ctx, hm); err != nil {
			d.log.Error("failed to push host metrics", "error", err)
		}
	}
}

func (d *Daemon) listWorkflowMetrics(ctx context.Context, ci iface.CIProvider) ([]domain.WorkflowMetrics, bool, error) {
	type shallowWorkflowMetricsProvider interface {
		ListRecentWorkflowRunsShallow(ctx context.Context, perRepo int) ([]domain.WorkflowMetrics, error)
	}

	if provider, ok := ci.(shallowWorkflowMetricsProvider); ok {
		runs, err := provider.ListRecentWorkflowRunsShallow(ctx, workflowMetricRunFetchLimit)
		return runs, true, err
	}

	runs, err := ci.ListRecentWorkflowRuns(ctx, workflowMetricRunFetchLimit)
	return runs, false, err
}

// buildRunnerMetrics converts runner data into the metrics payload.
func buildRunnerMetrics(runners []domain.Runner, containers []domain.Container, ci iface.CIProvider) domain.RunnerMetrics {
	m := domain.RunnerMetrics{
		TotalRunners: len(runners),
	}

	runnerByName := make(map[string]domain.Runner, len(runners))
	details := make([]domain.RunnerDetail, 0, len(runners))
	for _, r := range runners {
		runnerByName[r.Name] = r
		if r.Busy {
			m.BusyRunners++
		}
		if r.Status == "online" {
			m.OnlineRunners++
		}
		if ci.ClassifyRunner(r.Name) {
			m.AutoRunners++
		}
		details = append(details, domain.RunnerDetail{
			Name:   r.Name,
			Status: r.Status,
			Busy:   r.Busy,
			IsAuto: ci.ClassifyRunner(r.Name),
		})
	}

	m.IdleRunners = m.TotalRunners - m.BusyRunners
	m.AvailableOnlineRunners = engine.AvailableRunnerCount(runners)
	m.OfflineRunners = m.TotalRunners - m.OnlineRunners
	m.PermanentRunners = m.TotalRunners - m.AutoRunners
	for _, container := range containers {
		if container.Status != domain.StatusRunning {
			continue
		}
		runner, exists := runnerByName[container.Name]
		if !exists || runner.Status != "online" {
			m.ProvisioningRunners++
		}
	}
	if m.OnlineRunners > 0 {
		m.UtilizationPct = float64(m.BusyRunners) / float64(m.OnlineRunners) * 100
	}
	m.Runners = details

	return m
}

func (d *Daemon) filterNewWorkflowMetrics(runs []domain.WorkflowMetrics) []domain.WorkflowMetrics {
	d.workflowMu.Lock()
	defer d.workflowMu.Unlock()

	if d.workflowDelivered == nil {
		d.workflowDelivered = make(map[string]struct{})
	}

	fresh := make([]domain.WorkflowMetrics, 0, len(runs))
	batchSeen := make(map[string]struct{}, len(runs))
	for _, run := range runs {
		key := workflowMetricKey(run)
		if _, seen := d.workflowDelivered[key]; seen {
			continue
		}
		if _, seen := batchSeen[key]; seen {
			continue
		}
		batchSeen[key] = struct{}{}
		fresh = append(fresh, run)
	}
	return fresh
}

func (d *Daemon) markWorkflowMetricsDelivered(runs []domain.WorkflowMetrics) {
	d.workflowMu.Lock()
	defer d.workflowMu.Unlock()

	if d.workflowDelivered == nil {
		d.workflowDelivered = make(map[string]struct{})
	}

	for _, run := range runs {
		key := workflowMetricKey(run)
		if _, exists := d.workflowDelivered[key]; exists {
			continue
		}
		d.workflowDelivered[key] = struct{}{}
		d.workflowDeliveredKeys = append(d.workflowDeliveredKeys, key)
	}

	for len(d.workflowDeliveredKeys) > workflowMetricCacheLimit {
		oldest := d.workflowDeliveredKeys[0]
		d.workflowDeliveredKeys = d.workflowDeliveredKeys[1:]
		delete(d.workflowDelivered, oldest)
	}
	if err := d.persistWorkflowMetricCacheLocked(); err != nil {
		d.log.Warn("failed to persist workflow metric cache", "error", err)
	}
}

func workflowMetricKey(run domain.WorkflowMetrics) string {
	if run.RunID != 0 {
		return fmt.Sprintf("%d:%d", run.RunID, run.RunAttempt)
	}
	return fmt.Sprintf(
		"%s|%s|%d|%s|%s|%s|%d",
		run.Repo,
		run.Workflow,
		run.RunNumber,
		run.Branch,
		run.Event,
		run.Conclusion,
		run.DurationS,
	)
}

func filterContainersByPrefix(containers []domain.Container, prefix string) []domain.Container {
	if len(containers) == 0 {
		return nil
	}

	filtered := make([]domain.Container, 0, len(containers))
	for _, container := range containers {
		if prefix == "" || len(container.Name) >= len(prefix) && container.Name[:len(prefix)] == prefix {
			filtered = append(filtered, container)
		}
	}
	return filtered
}

// unused import guard
var _ = fmt.Sprintf
