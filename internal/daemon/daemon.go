// Package daemon orchestrates the scaler's concurrent subsystems:
// reconciler poll loop, webhook HTTP server, and metrics push loop.
package daemon

import (
	"context"
	"fmt"
	"log/slog"
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
	mu        sync.Mutex

	workflowMu            sync.Mutex
	workflowDelivered     map[string]struct{}
	workflowDeliveredKeys []string
}

const workflowMetricCacheLimit = 5000

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
		cfg:               cfg,
		reconciler:        reconciler,
		ci:                ci,
		metrics:           metrics,
		runtime:           runtime,
		log:               log,
		triggerCh:         make(chan struct{}, 1),
		workflowDelivered: make(map[string]struct{}),
	}
}

// Run starts all subsystems and blocks until ctx is cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	var wg sync.WaitGroup

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
	if !d.mu.TryLock() {
		d.log.Debug("reconcile already running, skipping")
		return
	}
	defer d.mu.Unlock()

	if err := d.reconciler.Reconcile(ctx); err != nil {
		d.log.Error("reconcile failed", "error", err)
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
	} else {
		rm := buildRunnerMetrics(runners, d.ci)
		if err := d.metrics.PushRunnerMetrics(ctx, rm); err != nil {
			d.log.Error("failed to push runner metrics", "error", err)
		}
	}

	// Workflow metrics.
	if d.cfg.CollectWorkflows {
		wm, err := d.ci.ListRecentWorkflowRuns(ctx, 5)
		if err != nil {
			d.log.Warn("failed to collect workflow metrics", "error", err)
		} else if len(wm) > 0 {
			wm = d.filterNewWorkflowMetrics(wm)
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
	m.AvailableOnlineRunners = engine.AvailableRunnerCount(runners)
	m.OfflineRunners = m.TotalRunners - m.OnlineRunners
	m.PermanentRunners = m.TotalRunners - m.AutoRunners
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

// unused import guard
var _ = fmt.Sprintf
