package daemon

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cordtus/gh-runner-scaler/internal/domain"
	"github.com/Cordtus/gh-runner-scaler/internal/engine"
	"github.com/Cordtus/gh-runner-scaler/internal/iface"
)

type daemonTestRuntime struct{}

func (daemonTestRuntime) CloneFromTemplate(context.Context, string) error { return nil }
func (daemonTestRuntime) StartContainer(context.Context, string) error    { return nil }
func (daemonTestRuntime) StopContainer(context.Context, string) error     { return nil }
func (daemonTestRuntime) DeleteContainer(context.Context, string) error   { return nil }
func (daemonTestRuntime) ExecCommand(context.Context, string, []string) (string, error) {
	return "", nil
}
func (daemonTestRuntime) WaitForReady(context.Context, string, []string, time.Duration) error {
	return nil
}
func (daemonTestRuntime) ListContainers(context.Context, string) ([]domain.Container, error) {
	return nil, nil
}
func (daemonTestRuntime) GetContainerStatus(context.Context, string) (domain.ContainerStatus, error) {
	return domain.StatusUnknown, nil
}

type syncCountingRuntime struct {
	execCalls  atomic.Int32
	containers []domain.Container
}

func (r *syncCountingRuntime) CloneFromTemplate(context.Context, string) error { return nil }
func (r *syncCountingRuntime) StartContainer(context.Context, string) error    { return nil }
func (r *syncCountingRuntime) StopContainer(context.Context, string) error     { return nil }
func (r *syncCountingRuntime) DeleteContainer(context.Context, string) error   { return nil }
func (r *syncCountingRuntime) ExecCommand(context.Context, string, []string) (string, error) {
	r.execCalls.Add(1)
	return "", nil
}
func (r *syncCountingRuntime) WaitForReady(context.Context, string, []string, time.Duration) error {
	return nil
}
func (r *syncCountingRuntime) ListContainers(context.Context, string) ([]domain.Container, error) {
	return r.containers, nil
}
func (r *syncCountingRuntime) GetContainerStatus(context.Context, string) (domain.ContainerStatus, error) {
	return domain.StatusUnknown, nil
}

type daemonTestState struct{}

func (daemonTestState) GetLastActive(context.Context, string) (time.Time, error) {
	return time.Time{}, nil
}
func (daemonTestState) SetLastActive(context.Context, string, time.Time) error { return nil }
func (daemonTestState) Create(context.Context, string) error                   { return nil }
func (daemonTestState) Delete(context.Context, string) error                   { return nil }
func (daemonTestState) ListAll(context.Context) (map[string]domain.ContainerState, error) {
	return nil, nil
}

type daemonTestCI struct {
	event *domain.WebhookEvent
}

func (d daemonTestCI) ListRunners(context.Context) ([]domain.Runner, error) { return nil, nil }
func (d daemonTestCI) GetRegistrationToken(context.Context) (string, error) { return "", nil }
func (d daemonTestCI) GetRemoveToken(context.Context) (string, error)       { return "", nil }
func (d daemonTestCI) DeleteRunner(context.Context, int64) error            { return nil }
func (d daemonTestCI) RegistrationURL() string                              { return "https://github.com/test-org" }
func (d daemonTestCI) ClassifyRunner(string) bool                           { return false }
func (d daemonTestCI) ValidateWebhookPayload([]byte, string) error          { return nil }
func (d daemonTestCI) ParseWebhookEvent(string, []byte) (*domain.WebhookEvent, error) {
	return d.event, nil
}
func (d daemonTestCI) ListRecentWorkflowRuns(context.Context, int) ([]domain.WorkflowMetrics, error) {
	return nil, nil
}

type blockingCI struct {
	calls        atomic.Int32
	firstStarted chan struct{}
	releaseFirst chan struct{}
}

func newBlockingCI() *blockingCI {
	return &blockingCI{
		firstStarted: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
}

func (b *blockingCI) ListRunners(context.Context) ([]domain.Runner, error) {
	call := b.calls.Add(1)
	if call == 1 {
		close(b.firstStarted)
		<-b.releaseFirst
	}
	return []domain.Runner{{Name: "permanent", Status: "online"}}, nil
}

func (b *blockingCI) GetRegistrationToken(context.Context) (string, error) { return "", nil }
func (b *blockingCI) GetRemoveToken(context.Context) (string, error)       { return "", nil }
func (b *blockingCI) DeleteRunner(context.Context, int64) error            { return nil }
func (b *blockingCI) RegistrationURL() string                              { return "https://github.com/test-org" }
func (b *blockingCI) ClassifyRunner(string) bool                           { return false }
func (b *blockingCI) ValidateWebhookPayload([]byte, string) error          { return nil }
func (b *blockingCI) ParseWebhookEvent(string, []byte) (*domain.WebhookEvent, error) {
	return nil, nil
}
func (b *blockingCI) ListRecentWorkflowRuns(context.Context, int) ([]domain.WorkflowMetrics, error) {
	return nil, nil
}

type failingCI struct{}

func (failingCI) ListRunners(context.Context) ([]domain.Runner, error) {
	return nil, context.DeadlineExceeded
}
func (failingCI) GetRegistrationToken(context.Context) (string, error) { return "", nil }
func (failingCI) GetRemoveToken(context.Context) (string, error)       { return "", nil }
func (failingCI) DeleteRunner(context.Context, int64) error            { return nil }
func (failingCI) RegistrationURL() string                              { return "https://github.com/test-org" }
func (failingCI) ClassifyRunner(string) bool                           { return false }
func (failingCI) ValidateWebhookPayload([]byte, string) error          { return nil }
func (failingCI) ParseWebhookEvent(string, []byte) (*domain.WebhookEvent, error) {
	return nil, nil
}
func (failingCI) ListRecentWorkflowRuns(context.Context, int) ([]domain.WorkflowMetrics, error) {
	return nil, nil
}

func newTestDaemon(t *testing.T, ci any) *Daemon {
	t.Helper()
	return newTestDaemonWithRuntime(t, ci, daemonTestRuntime{})
}

func newTestDaemonWithRuntime(t *testing.T, ci any, runtime iface.ContainerRuntime) *Daemon {
	t.Helper()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	var provider interface {
		ListRunners(context.Context) ([]domain.Runner, error)
		GetRegistrationToken(context.Context) (string, error)
		GetRemoveToken(context.Context) (string, error)
		DeleteRunner(context.Context, int64) error
		RegistrationURL() string
		ClassifyRunner(string) bool
		ValidateWebhookPayload([]byte, string) error
		ParseWebhookEvent(string, []byte) (*domain.WebhookEvent, error)
		ListRecentWorkflowRuns(context.Context, int) ([]domain.WorkflowMetrics, error)
	}

	switch v := ci.(type) {
	case daemonTestCI:
		provider = v
	case *blockingCI:
		provider = v
	case failingCI:
		provider = v
	default:
		t.Fatalf("unsupported test CI type %T", ci)
	}

	reconciler := engine.NewReconciler(
		engine.ReconcilerConfig{
			Prefix:         "auto",
			MaxAutoRunners: 0,
			IdleTimeout:    time.Minute,
			Labels:         "self-hosted",
			RunnerWorkDir:  "_work",
		},
		runtime,
		nil,
		provider,
		daemonTestState{},
		log,
	)

	return New(
		Config{
			PollInterval:    time.Second,
			WebhookEnabled:  true,
			WebhookPort:     9876,
			WebhookDebounce: 0,
		},
		reconciler,
		provider,
		nil,
		runtime,
		log,
	)
}

func waitForTriggerCount(t *testing.T, d *Daemon, want int) {
	t.Helper()

	deadline := time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(deadline) {
		if got := len(d.triggerCh); got == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for %d queued triggers, got %d", want, len(d.triggerCh))
}

func waitForCondition(t *testing.T, fn func() bool) {
	t.Helper()

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}

	t.Fatal("timed out waiting for condition")
}

func TestHandleWebhook_TriggersForWorkflowJobs(t *testing.T) {
	t.Run("queued", func(t *testing.T) {
		d := newTestDaemon(t, daemonTestCI{
			event: &domain.WebhookEvent{
				Type:   domain.EventJobQueued,
				Repo:   "Acme/repo",
				Detail: "queued: Acme/repo / quality",
			},
		})

		req := httptest.NewRequest("POST", "/", strings.NewReader(`{}`))
		req.Header.Set("X-Hub-Signature-256", "sha256=test")
		req.Header.Set("X-GitHub-Event", "workflow_job")
		rr := httptest.NewRecorder()

		d.handleWebhook(rr, req)

		if rr.Code != 200 {
			t.Fatalf("expected 200 response, got %d", rr.Code)
		}
		waitForTriggerCount(t, d, 1)
	})

	t.Run("completed", func(t *testing.T) {
		d := newTestDaemon(t, daemonTestCI{
			event: &domain.WebhookEvent{
				Type:   domain.EventJobCompleted,
				Repo:   "Acme/repo",
				Detail: "completed: Acme/repo / quality",
			},
		})

		req := httptest.NewRequest("POST", "/", strings.NewReader(`{}`))
		req.Header.Set("X-Hub-Signature-256", "sha256=test")
		req.Header.Set("X-GitHub-Event", "workflow_job")
		rr := httptest.NewRecorder()

		d.handleWebhook(rr, req)

		if rr.Code != 200 {
			t.Fatalf("expected 200 response, got %d", rr.Code)
		}
		waitForTriggerCount(t, d, 1)
	})
}

func TestHandleWebhook_DebounceIsScopedPerDaemonInstance(t *testing.T) {
	makeRequest := func() *http.Request {
		req := httptest.NewRequest("POST", "/", strings.NewReader(`{}`))
		req.Header.Set("X-Hub-Signature-256", "sha256=test")
		req.Header.Set("X-GitHub-Event", "workflow_job")
		return req
	}

	event := daemonTestCI{
		event: &domain.WebhookEvent{
			Type:   domain.EventJobQueued,
			Repo:   "Acme/repo",
			Detail: "queued: Acme/repo / quality",
		},
	}

	d1 := newTestDaemon(t, event)
	d1.cfg.WebhookDebounce = 20 * time.Millisecond
	d2 := newTestDaemon(t, event)
	d2.cfg.WebhookDebounce = 20 * time.Millisecond

	d1.handleWebhook(httptest.NewRecorder(), makeRequest())
	d2.handleWebhook(httptest.NewRecorder(), makeRequest())

	time.Sleep(50 * time.Millisecond)

	if got := len(d1.triggerCh); got != 1 {
		t.Fatalf("expected first daemon to receive its own debounced trigger, got %d", got)
	}
	if got := len(d2.triggerCh); got != 1 {
		t.Fatalf("expected second daemon to receive its own debounced trigger, got %d", got)
	}
}

func TestHandleWebhook_DebounceHonorsLifecycleCancellation(t *testing.T) {
	d := newTestDaemon(t, daemonTestCI{
		event: &domain.WebhookEvent{
			Type:   domain.EventJobQueued,
			Repo:   "Acme/repo",
			Detail: "queued: Acme/repo / quality",
		},
	})
	d.cfg.WebhookDebounce = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	d.lifecycleCtx = ctx
	cancel()

	req := httptest.NewRequest("POST", "/", strings.NewReader(`{}`))
	req.Header.Set("X-Hub-Signature-256", "sha256=test")
	req.Header.Set("X-GitHub-Event", "workflow_job")

	d.handleWebhook(httptest.NewRecorder(), req)
	time.Sleep(50 * time.Millisecond)

	if got := len(d.triggerCh); got != 0 {
		t.Fatalf("expected canceled daemon lifecycle to suppress debounced trigger, got %d", got)
	}
}

func TestHandleStatus_ReturnsDaemonSnapshot(t *testing.T) {
	d := newTestDaemon(t, daemonTestCI{})
	d.recordWebhook("workflow_job", &domain.WebhookEvent{Detail: "queued: Acme/repo / quality"})

	req := httptest.NewRequest("GET", "/statusz", nil)
	rr := httptest.NewRecorder()

	d.handleStatus(rr, req)

	if rr.Code != 200 {
		t.Fatalf("expected 200 response, got %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `"webhook_enabled":true`) {
		t.Fatalf("expected status response to include webhook_enabled, got %s", body)
	}
	if !strings.Contains(body, `"last_webhook_type":"workflow_job"`) {
		t.Fatalf("expected status response to include last webhook type, got %s", body)
	}
}

func TestHandleHealth_RestrictsMethods(t *testing.T) {
	d := newTestDaemon(t, daemonTestCI{})

	getReq := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	getRR := httptest.NewRecorder()
	d.handleHealth(getRR, getReq)

	if getRR.Code != http.StatusOK {
		t.Fatalf("expected GET health to return 200, got %d", getRR.Code)
	}
	if body := getRR.Body.String(); body != "ok" {
		t.Fatalf("expected health body ok, got %q", body)
	}

	postReq := httptest.NewRequest(http.MethodPost, "/healthz", nil)
	postRR := httptest.NewRecorder()
	d.handleHealth(postRR, postReq)

	if postRR.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected POST health to return 405, got %d", postRR.Code)
	}
}

func TestHandleStatus_RestrictsMethods(t *testing.T) {
	d := newTestDaemon(t, daemonTestCI{})

	req := httptest.NewRequest(http.MethodPost, "/statusz", nil)
	rr := httptest.NewRecorder()
	d.handleStatus(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected POST status to return 405, got %d", rr.Code)
	}
}

func TestDoReconcile_RerunsWhenTriggeredDuringAnActivePass(t *testing.T) {
	ci := newBlockingCI()
	d := newTestDaemon(t, ci)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		d.doReconcile(ctx)
	}()

	select {
	case <-ci.firstStarted:
	case <-ctx.Done():
		t.Fatal("timed out waiting for first reconcile pass to start")
	}

	d.Trigger()
	close(ci.releaseFirst)
	wg.Wait()

	if got := ci.calls.Load(); got != 2 {
		t.Fatalf("expected reconcile to rerun after a trigger during the first pass, got %d calls", got)
	}

	status := d.snapshotStatus()
	if status.ReconcileRuns != 2 {
		t.Fatalf("expected reconcile run count 2, got %d", status.ReconcileRuns)
	}
	if status.TriggerSequence != 1 {
		t.Fatalf("expected trigger sequence 1, got %d", status.TriggerSequence)
	}
	if status.LastReconcileStartedAt == "" || status.LastReconcileFinishedAt == "" || status.LastSuccessfulReconcileAt == "" {
		t.Fatalf("expected reconcile timestamps to be recorded, got %+v", status)
	}
}

func TestDoReconcile_DrainsRedundantQueuedTriggerBeforeReturn(t *testing.T) {
	ci := newBlockingCI()
	d := newTestDaemon(t, ci)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		d.doReconcile(ctx)
	}()

	select {
	case <-ci.firstStarted:
	case <-ctx.Done():
		t.Fatal("timed out waiting for first reconcile pass to start")
	}

	d.Trigger()
	d.triggerCh <- struct{}{}
	close(ci.releaseFirst)
	wg.Wait()

	if got := ci.calls.Load(); got != 2 {
		t.Fatalf("expected exactly one rerun, got %d calls", got)
	}
	if got := len(d.triggerCh); got != 0 {
		t.Fatalf("expected redundant queued trigger to be drained, got %d buffered signals", got)
	}
}

func TestDoReconcile_DrainsBufferedTriggerCoveredByCurrentPass(t *testing.T) {
	ci := newBlockingCI()
	d := newTestDaemon(t, ci)
	d.Trigger()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		d.doReconcile(ctx)
	}()

	select {
	case <-ci.firstStarted:
	case <-ctx.Done():
		t.Fatal("timed out waiting for reconcile pass to start")
	}

	close(ci.releaseFirst)
	wg.Wait()

	if got := ci.calls.Load(); got != 1 {
		t.Fatalf("expected buffered trigger to be covered by the current pass, got %d calls", got)
	}
	if got := len(d.triggerCh); got != 0 {
		t.Fatalf("expected buffered trigger to be drained, got %d queued signals", got)
	}

	status := d.snapshotStatus()
	if status.TriggerSequence != 1 {
		t.Fatalf("expected trigger sequence 1, got %d", status.TriggerSequence)
	}
	if status.ReconcileRuns != 1 {
		t.Fatalf("expected reconcile runs 1, got %d", status.ReconcileRuns)
	}
}

func TestDoReconcile_TracksFailureInStatus(t *testing.T) {
	d := newTestDaemon(t, failingCI{})

	d.doReconcile(context.Background())

	status := d.snapshotStatus()
	if status.ReconcileRuns != 1 {
		t.Fatalf("expected reconcile run count 1, got %d", status.ReconcileRuns)
	}
	if status.ReconcileFailures != 1 {
		t.Fatalf("expected reconcile failure count 1, got %d", status.ReconcileFailures)
	}
	if status.LastReconcileError == "" {
		t.Fatal("expected reconcile error to be recorded")
	}
	if status.LastSuccessfulReconcileAt != "" {
		t.Fatalf("expected no successful reconcile timestamp, got %q", status.LastSuccessfulReconcileAt)
	}
}

func TestHandlePushEvent_CollapsesSameRepoBurst(t *testing.T) {
	runtime := &syncCountingRuntime{
		containers: []domain.Container{{Name: "gh-runner", Status: domain.StatusRunning}},
	}
	d := newTestDaemonWithRuntime(t, daemonTestCI{}, runtime)
	d.cfg.WebhookDebounce = 20 * time.Millisecond
	d.cfg.SyncRepos = map[string]string{"Acme/repo": "/cache/repo"}

	event := &domain.WebhookEvent{
		Type:          domain.EventPush,
		Repo:          "Acme/repo",
		Ref:           "refs/heads/main",
		DefaultBranch: "main",
		Detail:        "push refs/heads/main to Acme/repo (0123456)",
	}

	d.handlePushEvent(event)
	time.Sleep(10 * time.Millisecond)
	d.handlePushEvent(event)

	waitForCondition(t, func() bool {
		return runtime.execCalls.Load() == 3
	})
}

func TestHandlePushEvent_HonorsLifecycleCancellation(t *testing.T) {
	runtime := &syncCountingRuntime{
		containers: []domain.Container{{Name: "gh-runner", Status: domain.StatusRunning}},
	}
	d := newTestDaemonWithRuntime(t, daemonTestCI{}, runtime)
	d.cfg.WebhookDebounce = 20 * time.Millisecond
	d.cfg.SyncRepos = map[string]string{"Acme/repo": "/cache/repo"}

	ctx, cancel := context.WithCancel(context.Background())
	d.lifecycleCtx = ctx
	cancel()

	d.handlePushEvent(&domain.WebhookEvent{
		Type:          domain.EventPush,
		Repo:          "Acme/repo",
		Ref:           "refs/heads/main",
		DefaultBranch: "main",
		Detail:        "push refs/heads/main to Acme/repo (0123456)",
	})

	time.Sleep(50 * time.Millisecond)

	if got := runtime.execCalls.Load(); got != 0 {
		t.Fatalf("expected canceled lifecycle to suppress cache sync, got %d exec calls", got)
	}
}
