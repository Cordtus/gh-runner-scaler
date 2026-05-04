// Package daemon orchestrates the scaler's concurrent subsystems:
// reconciler poll loop, webhook HTTP server, and metrics push loop.
package daemon

import (
	"context"
	"encoding/json"
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

// Daemon runs all subsystems as goroutines in a single process.
type Daemon struct {
	cfg        Config
	reconciler *engine.Reconciler
	ci         iface.CIProvider
	metrics    iface.MetricsBackend
	runtime    iface.ContainerRuntime
	logStore   *LogStore
	log        *slog.Logger

	triggerCh chan struct{}
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
	if log == nil {
		log = slog.Default()
	}
	d := &Daemon{
		cfg:               cfg,
		reconciler:        reconciler,
		ci:                ci,
		metrics:           metrics,
		runtime:           runtime,
		logStore:          logStore,
		log:               log,
		triggerCh:         make(chan struct{}, 1),
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
	d.mu.Lock()
	d.triggerSeq++
	running := d.reconcileRunning
	d.mu.Unlock()

	if running {
		return
	}

	select {
	case d.triggerCh <- struct{}{}:
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
		case <-d.triggerCh:
			d.doReconcile(ctx)
			ticker.Reset(d.cfg.PollInterval) // avoid redundant tick
		}
	}
}

// doReconcile runs a single reconcile pass with mutex protection.
func (d *Daemon) doReconcile(ctx context.Context) {
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

		d.drainTriggerSignals()

		startedAt := time.Now().UTC()

		d.mu.Lock()
		startSeq := d.triggerSeq
		d.lastReconcileStartedAt = startedAt
		d.mu.Unlock()

		d.drainTriggerSignals()

		if ctx.Err() != nil {
			d.mu.Lock()
			d.reconcileRunning = false
			d.mu.Unlock()
			return
		}

		err := d.reconciler.Reconcile(ctx)
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

		d.log.Info("reconcile rerun requested")
	}
}

func (d *Daemon) drainTriggerSignals() {
	for {
		select {
		case <-d.triggerCh:
		default:
			return
		}
	}
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
	// Runner metrics.
	runners, err := d.ci.ListRunners(ctx)
	if err != nil {
		d.log.Error("failed to list runners for metrics", "error", err)
	}

	var allContainers []domain.Container
	var autoContainers []domain.Container
	containersLoaded := false
	if d.runtime != nil && (d.cfg.CollectHost || err == nil) {
		listed, listErr := d.runtime.ListContainers(ctx, "")
		if listErr != nil {
			d.log.Warn("failed to list containers for metrics", "error", listErr)
		} else {
			containersLoaded = true
			allContainers = append([]domain.Container{}, listed...)
			autoContainers = filterContainersByPrefix(listed, d.cfg.Prefix)
		}
	}

	if err == nil {
		rm := buildRunnerMetrics(runners, autoContainers, d.ci)
		if err := d.metrics.PushRunnerMetrics(ctx, rm); err != nil {
			d.log.Error("failed to push runner metrics", "error", err)
		}
	}

	// Workflow metrics.
	if d.cfg.CollectWorkflows {
		wm, needsEnrichment, err := d.listWorkflowMetrics(ctx)
		if err != nil {
			d.log.Warn("failed to collect workflow metrics", "error", err)
		} else if len(wm) > 0 {
			wm = d.filterNewWorkflowMetrics(wm)
		}
		if len(wm) > 0 && needsEnrichment {
			enriched, enrichErr := d.ci.EnrichWorkflowMetrics(ctx, wm)
			if len(enriched) > 0 {
				wm = enriched
			}
			if enrichErr != nil {
				d.log.Warn("failed to enrich workflow metrics", "error", enrichErr)
			}
		}
		if len(wm) > 0 {
			if err := d.metrics.PushWorkflowMetrics(ctx, wm); err != nil {
				d.log.Error("failed to push workflow metrics", "error", err)
			} else {
				d.markWorkflowMetricsDelivered(wm)
			}
		}
	}

	// Host metrics (requires runtime to support it).
	if d.cfg.CollectHost {
		d.collectHostMetrics(ctx, allContainers, autoContainers, containersLoaded)
	}

	d.collectLogDerivedMetrics(ctx)
}

// collectHostMetrics attempts to gather host-level metrics from the runtime.
// This is provider-specific, so we use a type assertion.
func (d *Daemon) collectHostMetrics(ctx context.Context, allContainers, autoContainers []domain.Container, containersLoaded bool) {
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

	if hmp, snapshotOK := d.runtime.(hostMetricsSnapshotProvider); snapshotOK {
		hm, err = hmp.HostMetricsFromContainers(d.cfg.CachePool, allContainers)
		ok = true
	} else if hmp, hostOK := d.runtime.(hostMetricsProvider); hostOK {
		hm, err = hmp.HostMetrics(d.cfg.CachePool)
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
		if err := d.metrics.PushHostMetrics(ctx, hm); err != nil {
			d.log.Error("failed to push host metrics", "error", err)
		}
	}
}

func (d *Daemon) listWorkflowMetrics(ctx context.Context) ([]domain.WorkflowMetrics, bool, error) {
	type shallowWorkflowMetricsProvider interface {
		ListRecentWorkflowRunsShallow(ctx context.Context, perRepo int) ([]domain.WorkflowMetrics, error)
	}

	if provider, ok := d.ci.(shallowWorkflowMetricsProvider); ok {
		runs, err := provider.ListRecentWorkflowRunsShallow(ctx, workflowMetricRunFetchLimit)
		return runs, true, err
	}

	runs, err := d.ci.ListRecentWorkflowRuns(ctx, workflowMetricRunFetchLimit)
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
