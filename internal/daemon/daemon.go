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
	PollInterval     time.Duration
	WebhookEnabled   bool
	WebhookPort      int
	WebhookDebounce  time.Duration
	MetricsEnabled   bool
	MetricsInterval  time.Duration
	CollectWorkflows bool
	CollectHost      bool
	CachePool        string            // for host metrics
	SyncRepos        map[string]string // repo -> cache path
}

// Daemon runs all subsystems as goroutines in a single process.
type Daemon struct {
	cfg        Config
	reconciler *engine.Reconciler
	ci         iface.CIProvider
	metrics    iface.MetricsBackend
	runtime    iface.ContainerRuntime
	log        *slog.Logger

	triggerCh chan struct{}
	debouncer *debouncer
	mu        sync.Mutex

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

// New creates a Daemon with all subsystems wired.
func New(
	cfg Config,
	reconciler *engine.Reconciler,
	ci iface.CIProvider,
	metrics iface.MetricsBackend,
	runtime iface.ContainerRuntime,
	log *slog.Logger,
) *Daemon {
	if log == nil {
		log = slog.Default()
	}
	return &Daemon{
		cfg:          cfg,
		reconciler:   reconciler,
		ci:           ci,
		metrics:      metrics,
		runtime:      runtime,
		log:          log,
		triggerCh:    make(chan struct{}, 1),
		debouncer:    newDebouncer(),
		lifecycleCtx: context.Background(),
	}
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
		return
	}

	rm := buildRunnerMetrics(runners, d.ci)
	if err := d.metrics.PushRunnerMetrics(ctx, rm); err != nil {
		d.log.Error("failed to push runner metrics", "error", err)
	}

	// Workflow metrics.
	if d.cfg.CollectWorkflows {
		wm, err := d.ci.ListRecentWorkflowRuns(ctx, 5)
		if err != nil {
			d.log.Warn("failed to collect workflow metrics", "error", err)
		} else if len(wm) > 0 {
			if err := d.metrics.PushWorkflowMetrics(ctx, wm); err != nil {
				d.log.Error("failed to push workflow metrics", "error", err)
			}
		}
	}

	// Host metrics (requires runtime to support it).
	if d.cfg.CollectHost {
		d.collectHostMetrics(ctx)
	}
}

// collectHostMetrics attempts to gather host-level metrics from the runtime.
// This is provider-specific, so we use a type assertion.
func (d *Daemon) collectHostMetrics(ctx context.Context) {
	type hostMetricsProvider interface {
		HostMetrics(cachePool string) (domain.HostMetrics, error)
	}

	if hmp, ok := d.runtime.(hostMetricsProvider); ok {
		hm, err := hmp.HostMetrics(d.cfg.CachePool)
		if err != nil {
			d.log.Warn("failed to collect host metrics", "error", err)
			return
		}
		if err := d.metrics.PushHostMetrics(ctx, hm); err != nil {
			d.log.Error("failed to push host metrics", "error", err)
		}
	}
}

// buildRunnerMetrics converts runner data into the metrics payload.
func buildRunnerMetrics(runners []domain.Runner, ci iface.CIProvider) domain.RunnerMetrics {
	m := domain.RunnerMetrics{
		TotalRunners: len(runners),
	}

	details := make([]domain.RunnerDetail, 0, len(runners))
	for _, r := range runners {
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
	m.OfflineRunners = m.TotalRunners - m.OnlineRunners
	m.PermanentRunners = m.TotalRunners - m.AutoRunners
	if m.TotalRunners > 0 {
		m.UtilizationPct = float64(m.BusyRunners) / float64(m.TotalRunners) * 100
	}
	m.Runners = details

	return m
}

// unused import guard
var _ = fmt.Sprintf
