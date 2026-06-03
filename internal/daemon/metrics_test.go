package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Cordtus/gh-runner-scaler/internal/domain"
)

type metricsTestCI struct {
	prefix string
}

func (m metricsTestCI) ListRunners(_ context.Context) ([]domain.Runner, error) {
	return nil, nil
}

func (m metricsTestCI) GetRegistrationToken(_ context.Context) (string, error) {
	return "", nil
}

func (m metricsTestCI) GetRemoveToken(_ context.Context) (string, error) {
	return "", nil
}

func (m metricsTestCI) DeleteRunner(_ context.Context, _ int64) error {
	return nil
}

func (m metricsTestCI) RegistrationURL() string { return "" }

func (m metricsTestCI) ClassifyRunner(name string) bool {
	return len(name) >= len(m.prefix) && name[:len(m.prefix)] == m.prefix
}

func (m metricsTestCI) ValidateWebhookPayload(_ []byte, _ string) error { return nil }

func (m metricsTestCI) ParseWebhookEvent(_ string, _ []byte) (*domain.WebhookEvent, error) {
	return nil, nil
}

func (m metricsTestCI) ListRecentWorkflowRuns(_ context.Context, _ int) ([]domain.WorkflowMetrics, error) {
	return nil, nil
}

func (m metricsTestCI) ListRecentWorkflowRunsShallow(_ context.Context, _ int) ([]domain.WorkflowMetrics, error) {
	return nil, nil
}

func (m metricsTestCI) EnrichWorkflowMetrics(_ context.Context, runs []domain.WorkflowMetrics) ([]domain.WorkflowMetrics, error) {
	return append([]domain.WorkflowMetrics(nil), runs...), nil
}

func TestBuildRunnerMetrics_TracksAvailableOnlineSeparatelyFromIdle(t *testing.T) {
	runners := []domain.Runner{
		{ID: 1, Name: "permanent-1", Status: "online", Busy: true},
		{ID: 2, Name: "auto-1", Status: "online", Busy: false},
		{ID: 3, Name: "auto-2", Status: "offline", Busy: false},
	}
	containers := []domain.Container{
		{Name: "auto-1", Status: domain.StatusRunning},
		{Name: "auto-2", Status: domain.StatusRunning},
		{Name: "auto-3", Status: domain.StatusRunning},
	}

	metrics := buildRunnerMetrics(runners, containers, metricsTestCI{prefix: "auto"})

	if metrics.TotalRunners != 3 {
		t.Fatalf("TotalRunners = %d, want 3", metrics.TotalRunners)
	}
	if metrics.BusyRunners != 1 {
		t.Fatalf("BusyRunners = %d, want 1", metrics.BusyRunners)
	}
	if metrics.IdleRunners != 2 {
		t.Fatalf("IdleRunners = %d, want 2", metrics.IdleRunners)
	}
	if metrics.AvailableOnlineRunners != 1 {
		t.Fatalf("AvailableOnlineRunners = %d, want 1", metrics.AvailableOnlineRunners)
	}
	if metrics.OnlineRunners != 2 {
		t.Fatalf("OnlineRunners = %d, want 2", metrics.OnlineRunners)
	}
	if metrics.OfflineRunners != 1 {
		t.Fatalf("OfflineRunners = %d, want 1", metrics.OfflineRunners)
	}
	if metrics.AutoRunners != 2 {
		t.Fatalf("AutoRunners = %d, want 2", metrics.AutoRunners)
	}
	if metrics.PermanentRunners != 1 {
		t.Fatalf("PermanentRunners = %d, want 1", metrics.PermanentRunners)
	}
	if metrics.ProvisioningRunners != 2 {
		t.Fatalf("ProvisioningRunners = %d, want 2", metrics.ProvisioningRunners)
	}
	if metrics.UtilizationPct != 50 {
		t.Fatalf("UtilizationPct = %v, want 50", metrics.UtilizationPct)
	}
}

type collectMetricsTestCI struct {
	metricsTestCI
	runners             []domain.Runner
	runnersErr          error
	runnerInventoryMeta domain.RunnerInventoryMeta
	workflowRunsBatches [][]domain.WorkflowMetrics
	workflowCall        int
	enrichCalls         [][]domain.WorkflowMetrics
	enrichErr           error
	enrichFn            func([]domain.WorkflowMetrics) []domain.WorkflowMetrics
}

func (m *collectMetricsTestCI) ListRunners(_ context.Context) ([]domain.Runner, error) {
	if m.runnersErr != nil {
		return nil, m.runnersErr
	}
	return append([]domain.Runner(nil), m.runners...), nil
}

func (m *collectMetricsTestCI) ListRunnersForMetrics(ctx context.Context) ([]domain.Runner, domain.RunnerInventoryMeta, error) {
	runners, err := m.ListRunners(ctx)
	return runners, m.runnerInventoryMeta, err
}

func (m *collectMetricsTestCI) ListRecentWorkflowRuns(ctx context.Context, perRepo int) ([]domain.WorkflowMetrics, error) {
	return m.ListRecentWorkflowRunsShallow(ctx, perRepo)
}

func (m *collectMetricsTestCI) ListRecentWorkflowRunsShallow(_ context.Context, _ int) ([]domain.WorkflowMetrics, error) {
	if len(m.workflowRunsBatches) == 0 {
		return nil, nil
	}
	idx := m.workflowCall
	if idx >= len(m.workflowRunsBatches) {
		idx = len(m.workflowRunsBatches) - 1
	}
	m.workflowCall++
	return append([]domain.WorkflowMetrics(nil), m.workflowRunsBatches[idx]...), nil
}

func (m *collectMetricsTestCI) EnrichWorkflowMetrics(_ context.Context, runs []domain.WorkflowMetrics) ([]domain.WorkflowMetrics, error) {
	batch := append([]domain.WorkflowMetrics(nil), runs...)
	m.enrichCalls = append(m.enrichCalls, batch)
	if m.enrichFn != nil {
		return m.enrichFn(batch), m.enrichErr
	}
	return batch, m.enrichErr
}

type metricsRecorder struct {
	runnerBatches    []domain.RunnerMetrics
	workflowBatches  [][]domain.WorkflowMetrics
	hostBatches      []domain.HostMetrics
	issueBatches     [][]domain.IssueEvent
	lifecycleBatches []domain.LifecycleMetrics
	workflowErrs     []error
	issueErrs        []error
	workflowCalls    int
	issueCalls       int
}

func (m *metricsRecorder) PushRunnerMetrics(_ context.Context, rm domain.RunnerMetrics) error {
	m.runnerBatches = append(m.runnerBatches, rm)
	return nil
}

func (m *metricsRecorder) PushWorkflowMetrics(_ context.Context, runs []domain.WorkflowMetrics) error {
	call := m.workflowCalls
	m.workflowCalls++
	if call < len(m.workflowErrs) && m.workflowErrs[call] != nil {
		return m.workflowErrs[call]
	}
	batch := append([]domain.WorkflowMetrics(nil), runs...)
	m.workflowBatches = append(m.workflowBatches, batch)
	return nil
}

func (m *metricsRecorder) PushHostMetrics(_ context.Context, hm domain.HostMetrics) error {
	m.hostBatches = append(m.hostBatches, hm)
	return nil
}

func (m *metricsRecorder) PushIssueEvents(_ context.Context, issues []domain.IssueEvent) error {
	call := m.issueCalls
	m.issueCalls++
	if call < len(m.issueErrs) && m.issueErrs[call] != nil {
		return m.issueErrs[call]
	}
	batch := append([]domain.IssueEvent(nil), issues...)
	m.issueBatches = append(m.issueBatches, batch)
	return nil
}

func (m *metricsRecorder) PushLifecycleMetrics(_ context.Context, metrics domain.LifecycleMetrics) error {
	m.lifecycleBatches = append(m.lifecycleBatches, metrics)
	return nil
}

type metricsTestRuntime struct {
	hostMetrics domain.HostMetrics
	hostErr     error
	containers  []domain.Container
	listErr     error
	listCalls   int
	hostCalls   int
	hostInputs  [][]domain.Container
}

func (m metricsTestRuntime) CloneFromTemplate(context.Context, string) error {
	return nil
}

func (m metricsTestRuntime) StartContainer(context.Context, string) error {
	return nil
}

func (m metricsTestRuntime) StopContainer(context.Context, string) error {
	return nil
}

func (m metricsTestRuntime) DeleteContainer(context.Context, string) error {
	return nil
}

func (m metricsTestRuntime) ExecCommand(context.Context, string, []string) (string, error) {
	return "", nil
}

func (m metricsTestRuntime) WaitForReady(context.Context, string, []string, time.Duration) error {
	return nil
}

func (m *metricsTestRuntime) ListContainers(_ context.Context, prefix string) ([]domain.Container, error) {
	m.listCalls++
	if m.listErr != nil {
		return nil, m.listErr
	}
	if prefix == "" {
		return append([]domain.Container(nil), m.containers...), nil
	}
	filtered := make([]domain.Container, 0, len(m.containers))
	for _, container := range m.containers {
		if len(container.Name) >= len(prefix) && container.Name[:len(prefix)] == prefix {
			filtered = append(filtered, container)
		}
	}
	return filtered, nil
}

func (m *metricsTestRuntime) GetContainerStatus(context.Context, string) (domain.ContainerStatus, error) {
	return domain.StatusUnknown, nil
}

func (m *metricsTestRuntime) HostMetrics(_ string) (domain.HostMetrics, error) {
	return m.hostMetricsFor(nil)
}

func (m *metricsTestRuntime) HostMetricsFromContainers(_ string, containers []domain.Container) (domain.HostMetrics, error) {
	return m.hostMetricsFor(containers)
}

func (m *metricsTestRuntime) hostMetricsFor(containers []domain.Container) (domain.HostMetrics, error) {
	m.hostCalls++
	m.hostInputs = append(m.hostInputs, append([]domain.Container(nil), containers...))
	if m.hostErr != nil {
		return domain.HostMetrics{}, m.hostErr
	}
	return m.hostMetrics, nil
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestCollectAndPush_DeduplicatesRepeatedWorkflowMetrics(t *testing.T) {
	runA := domain.WorkflowMetrics{
		Repo:       "repo-a",
		Workflow:   "build",
		Conclusion: "success",
		DurationS:  90,
		RunNumber:  7,
		Event:      "push",
		Branch:     "main",
	}
	runB := domain.WorkflowMetrics{
		Repo:       "repo-a",
		Workflow:   "build",
		Conclusion: "success",
		DurationS:  95,
		RunNumber:  8,
		Event:      "push",
		Branch:     "main",
	}

	ci := &collectMetricsTestCI{
		metricsTestCI: metricsTestCI{prefix: "auto"},
		workflowRunsBatches: [][]domain.WorkflowMetrics{
			{runA},
			{runA, runB},
		},
	}
	backend := &metricsRecorder{}
	daemon := New(
		Config{CollectWorkflows: true},
		nil,
		ci,
		backend,
		nil,
		nil,
		testLogger(),
	)

	daemon.collectAndPush(context.Background())
	daemon.collectAndPush(context.Background())

	if len(backend.workflowBatches) != 2 {
		t.Fatalf("workflow batch count = %d, want 2", len(backend.workflowBatches))
	}
	if len(backend.workflowBatches[0]) != 1 || backend.workflowBatches[0][0] != runA {
		t.Fatalf("first workflow batch = %+v, want [%+v]", backend.workflowBatches[0], runA)
	}
	if len(backend.workflowBatches[1]) != 1 || backend.workflowBatches[1][0] != runB {
		t.Fatalf("second workflow batch = %+v, want [%+v]", backend.workflowBatches[1], runB)
	}
}

func TestCollectAndPush_CollectsWorkflowMetricsForEachRunnerGroup(t *testing.T) {
	typeScriptRun := domain.WorkflowMetrics{
		RunID:      101,
		RunAttempt: 1,
		Repo:       "Acme/typescript",
		Workflow:   "build",
		Conclusion: "success",
		DurationS:  30,
		RunNumber:  7,
		Event:      "push",
		Branch:     "main",
	}
	rustRun := domain.WorkflowMetrics{
		RunID:      202,
		RunAttempt: 1,
		Repo:       "Acme/rust",
		Workflow:   "test",
		Conclusion: "failure",
		DurationS:  45,
		RunNumber:  8,
		Event:      "pull_request",
		Branch:     "dev",
	}

	typeScriptCI := &collectMetricsTestCI{
		metricsTestCI: metricsTestCI{prefix: "ts-auto"},
		workflowRunsBatches: [][]domain.WorkflowMetrics{
			{typeScriptRun},
		},
	}
	rustCI := &collectMetricsTestCI{
		metricsTestCI: metricsTestCI{prefix: "rust-auto"},
		workflowRunsBatches: [][]domain.WorkflowMetrics{
			{rustRun},
		},
	}
	typeScriptBackend := &metricsRecorder{}
	rustBackend := &metricsRecorder{}
	daemon := NewWithRunnerGroups(
		Config{CollectWorkflows: true},
		[]RunnerGroup{
			{ID: "typescript", Prefix: "ts-auto", CI: typeScriptCI, Metrics: typeScriptBackend},
			{ID: "rust", Prefix: "rust-auto", CI: rustCI, Metrics: rustBackend},
		},
		nil,
		nil,
		testLogger(),
	)

	daemon.collectAndPush(context.Background())

	if len(typeScriptBackend.workflowBatches) != 1 {
		t.Fatalf("typescript workflow batch count = %d, want 1", len(typeScriptBackend.workflowBatches))
	}
	if len(typeScriptBackend.workflowBatches[0]) != 1 || typeScriptBackend.workflowBatches[0][0] != typeScriptRun {
		t.Fatalf("typescript workflow batch = %+v, want [%+v]", typeScriptBackend.workflowBatches[0], typeScriptRun)
	}
	if len(rustBackend.workflowBatches) != 1 {
		t.Fatalf("rust workflow batch count = %d, want 1", len(rustBackend.workflowBatches))
	}
	if len(rustBackend.workflowBatches[0]) != 1 || rustBackend.workflowBatches[0][0] != rustRun {
		t.Fatalf("rust workflow batch = %+v, want [%+v]", rustBackend.workflowBatches[0], rustRun)
	}
}

func TestCollectAndPush_PersistsWorkflowDedupeAcrossDaemonRestart(t *testing.T) {
	stateDir := t.TempDir()
	run := domain.WorkflowMetrics{
		RunID:      101,
		RunAttempt: 2,
		Repo:       "repo-a",
		Workflow:   "build",
		Conclusion: "success",
		DurationS:  90,
		RunNumber:  7,
		Event:      "push",
		Branch:     "main",
	}

	firstCI := &collectMetricsTestCI{
		metricsTestCI: metricsTestCI{prefix: "auto"},
		workflowRunsBatches: [][]domain.WorkflowMetrics{
			{run},
		},
	}
	firstBackend := &metricsRecorder{}
	first := New(
		Config{CollectWorkflows: true, StateDir: stateDir},
		nil,
		firstCI,
		firstBackend,
		nil,
		nil,
		testLogger(),
	)
	first.collectAndPush(context.Background())

	if len(firstBackend.workflowBatches) != 1 {
		t.Fatalf("first workflow batch count = %d, want 1", len(firstBackend.workflowBatches))
	}

	secondCI := &collectMetricsTestCI{
		metricsTestCI: metricsTestCI{prefix: "auto"},
		workflowRunsBatches: [][]domain.WorkflowMetrics{
			{run},
		},
	}
	secondBackend := &metricsRecorder{}
	second := New(
		Config{CollectWorkflows: true, StateDir: stateDir},
		nil,
		secondCI,
		secondBackend,
		nil,
		nil,
		testLogger(),
	)
	second.collectAndPush(context.Background())

	if len(secondBackend.workflowBatches) != 0 {
		t.Fatalf("second workflow batch count = %d, want 0", len(secondBackend.workflowBatches))
	}
}

func TestCollectAndPush_ContinuesWorkflowAndHostMetricsWhenRunnerListFails(t *testing.T) {
	run := domain.WorkflowMetrics{
		Repo:       "repo-a",
		Workflow:   "build",
		Conclusion: "success",
		DurationS:  90,
		RunNumber:  7,
		Event:      "push",
		Branch:     "main",
	}

	ci := &collectMetricsTestCI{
		metricsTestCI: metricsTestCI{prefix: "auto"},
		runnersErr:    errors.New("runner API unavailable"),
		workflowRunsBatches: [][]domain.WorkflowMetrics{
			{run},
		},
	}
	backend := &metricsRecorder{}
	runtime := &metricsTestRuntime{
		hostMetrics: domain.HostMetrics{
			ContainersRunning: 3,
			ContainersStopped: 12,
		},
		containers: []domain.Container{
			{Name: "auto-1", Status: domain.StatusRunning},
			{Name: "auto-2", Status: domain.StatusStopped},
		},
	}
	daemon := New(
		Config{Prefix: "auto", CollectWorkflows: true, CollectHost: true, CachePool: "pool9"},
		nil,
		ci,
		backend,
		runtime,
		nil,
		testLogger(),
	)

	daemon.collectAndPush(context.Background())

	if len(backend.runnerBatches) != 0 {
		t.Fatalf("runner batch count = %d, want 0", len(backend.runnerBatches))
	}
	if len(backend.workflowBatches) != 1 {
		t.Fatalf("workflow batch count = %d, want 1", len(backend.workflowBatches))
	}
	if len(backend.hostBatches) != 1 {
		t.Fatalf("host batch count = %d, want 1", len(backend.hostBatches))
	}
	if got := backend.hostBatches[0].ContainersRunning; got != 3 {
		t.Fatalf("host containers running = %d, want 3", got)
	}
	if backend.hostBatches[0].RunnerContainersRunning == nil {
		t.Fatal("runner containers running = nil, want 1")
	}
	if got := *backend.hostBatches[0].RunnerContainersRunning; got != 1 {
		t.Fatalf("runner containers running = %d, want 1", got)
	}
	if backend.hostBatches[0].RunnerContainersStopped == nil {
		t.Fatal("runner containers stopped = nil, want 1")
	}
	if got := *backend.hostBatches[0].RunnerContainersStopped; got != 1 {
		t.Fatalf("runner containers stopped = %d, want 1", got)
	}
}

func TestCollectAndPush_MarksStaleRunnerInventory(t *testing.T) {
	ci := &collectMetricsTestCI{
		metricsTestCI: metricsTestCI{prefix: "auto"},
		runners: []domain.Runner{
			{ID: 1, Name: "auto-1", Status: "online", Busy: false},
		},
		runnerInventoryMeta: domain.RunnerInventoryMeta{
			Stale:     true,
			AgeS:      75,
			FetchedAt: "2026-05-19T16:00:00Z",
			Error:     "listing runners: rate limited",
		},
	}
	backend := &metricsRecorder{}
	daemon := New(
		Config{Prefix: "auto"},
		nil,
		ci,
		backend,
		nil,
		nil,
		testLogger(),
	)

	daemon.collectAndPush(context.Background())

	if len(backend.runnerBatches) != 1 {
		t.Fatalf("runner batch count = %d, want 1", len(backend.runnerBatches))
	}
	got := backend.runnerBatches[0]
	if !got.RunnerInventoryStale {
		t.Fatal("RunnerInventoryStale = false, want true")
	}
	if got.RunnerInventoryAgeS != 75 {
		t.Fatalf("RunnerInventoryAgeS = %d, want 75", got.RunnerInventoryAgeS)
	}
	if got.RunnerInventoryAt != "2026-05-19T16:00:00Z" {
		t.Fatalf("RunnerInventoryAt = %q, want 2026-05-19T16:00:00Z", got.RunnerInventoryAt)
	}
	if got.RunnerInventoryError != "listing runners: rate limited" {
		t.Fatalf("RunnerInventoryError = %q, want listing runners: rate limited", got.RunnerInventoryError)
	}
}

func TestCollectAndPush_OmitsRunnerContainerSamplesWhenContainerListFails(t *testing.T) {
	ci := &collectMetricsTestCI{
		metricsTestCI: metricsTestCI{prefix: "auto"},
	}
	backend := &metricsRecorder{}
	runtime := &metricsTestRuntime{
		hostMetrics: domain.HostMetrics{
			ContainersRunning: 4,
			ContainersStopped: 9,
		},
		listErr: errors.New("lxc unavailable"),
	}
	daemon := New(
		Config{Prefix: "auto", CollectHost: true, CachePool: "pool9"},
		nil,
		ci,
		backend,
		runtime,
		nil,
		testLogger(),
	)

	daemon.collectAndPush(context.Background())

	if len(backend.hostBatches) != 1 {
		t.Fatalf("host batch count = %d, want 1", len(backend.hostBatches))
	}
	if got := backend.hostBatches[0].ContainersRunning; got != 4 {
		t.Fatalf("host containers running = %d, want 4", got)
	}
	if backend.hostBatches[0].RunnerContainersRunning != nil {
		t.Fatalf("runner containers running = %v, want nil", *backend.hostBatches[0].RunnerContainersRunning)
	}
	if backend.hostBatches[0].RunnerContainersStopped != nil {
		t.Fatalf("runner containers stopped = %v, want nil", *backend.hostBatches[0].RunnerContainersStopped)
	}
}

func TestCollectAndPush_ReusesContainerSnapshotAcrossRunnerAndHostMetrics(t *testing.T) {
	ci := &collectMetricsTestCI{
		metricsTestCI: metricsTestCI{prefix: "auto"},
		runners: []domain.Runner{
			{ID: 1, Name: "auto-1", Status: "online", Busy: false},
		},
	}
	backend := &metricsRecorder{}
	runtime := &metricsTestRuntime{
		hostMetrics: domain.HostMetrics{
			ContainersRunning: 2,
			ContainersStopped: 5,
		},
		containers: []domain.Container{
			{Name: "auto-1", Status: domain.StatusRunning},
			{Name: "auto-2", Status: domain.StatusStopped},
			{Name: "auto-3", Status: domain.StatusRunning},
			{Name: "permanent", Status: domain.StatusRunning},
		},
	}

	daemon := New(
		Config{Prefix: "auto", CollectHost: true},
		nil,
		ci,
		backend,
		runtime,
		nil,
		testLogger(),
	)

	daemon.collectAndPush(context.Background())

	if runtime.listCalls != 1 {
		t.Fatalf("ListContainers calls = %d, want 1", runtime.listCalls)
	}
	if runtime.hostCalls != 1 {
		t.Fatalf("HostMetrics calls = %d, want 1", runtime.hostCalls)
	}
	if len(runtime.hostInputs) != 1 || len(runtime.hostInputs[0]) != 4 {
		t.Fatalf("HostMetrics input = %+v, want all 4 containers", runtime.hostInputs)
	}
	if len(backend.runnerBatches) != 1 {
		t.Fatalf("runner batch count = %d, want 1", len(backend.runnerBatches))
	}
	if got := backend.runnerBatches[0].ProvisioningRunners; got != 1 {
		t.Fatalf("ProvisioningRunners = %d, want 1", got)
	}
	if len(backend.hostBatches) != 1 {
		t.Fatalf("host batch count = %d, want 1", len(backend.hostBatches))
	}
	if got := *backend.hostBatches[0].RunnerContainersRunning; got != 2 {
		t.Fatalf("runner containers running = %d, want 2", got)
	}
	if got := *backend.hostBatches[0].RunnerContainersStopped; got != 1 {
		t.Fatalf("runner containers stopped = %d, want 1", got)
	}
}

func TestCollectAndPush_PushesRunnerAndHostMetricsForEachRunnerGroup(t *testing.T) {
	typeScriptCI := &collectMetricsTestCI{
		metricsTestCI: metricsTestCI{prefix: "ts-auto"},
		runners: []domain.Runner{
			{ID: 1, Name: "ts-auto-1", Status: "online", Busy: false},
		},
	}
	rustCI := &collectMetricsTestCI{
		metricsTestCI: metricsTestCI{prefix: "rust-auto"},
		runners: []domain.Runner{
			{ID: 2, Name: "rust-auto-1", Status: "online", Busy: true},
		},
	}
	typeScriptRuntime := &metricsTestRuntime{
		hostMetrics: domain.HostMetrics{ContainersRunning: 4},
		containers: []domain.Container{
			{Name: "ts-auto-1", Status: domain.StatusRunning},
			{Name: "ts-auto-2", Status: domain.StatusStopped},
			{Name: "other", Status: domain.StatusRunning},
		},
	}
	rustRuntime := &metricsTestRuntime{
		hostMetrics: domain.HostMetrics{ContainersRunning: 3},
		containers: []domain.Container{
			{Name: "rust-auto-1", Status: domain.StatusRunning},
			{Name: "rust-auto-2", Status: domain.StatusRunning},
			{Name: "other", Status: domain.StatusRunning},
		},
	}

	backend := &metricsRecorder{}
	daemon := NewWithRunnerGroups(
		Config{CollectHost: true},
		[]RunnerGroup{
			{ID: "typescript", Prefix: "ts-auto", CachePool: "pool-ts", CI: typeScriptCI, Runtime: typeScriptRuntime},
			{ID: "rust", Prefix: "rust-auto", CachePool: "pool-rust", CI: rustCI, Runtime: rustRuntime},
		},
		backend,
		nil,
		testLogger(),
	)

	daemon.collectAndPush(context.Background())

	if len(backend.runnerBatches) != 2 {
		t.Fatalf("runner batch count = %d, want 2", len(backend.runnerBatches))
	}
	if backend.runnerBatches[0].GroupID != "typescript" || backend.runnerBatches[1].GroupID != "rust" {
		t.Fatalf("runner group ids = %q, %q; want typescript, rust", backend.runnerBatches[0].GroupID, backend.runnerBatches[1].GroupID)
	}
	if got := backend.runnerBatches[0].ProvisioningRunners; got != 0 {
		t.Fatalf("typescript provisioning runners = %d, want 0", got)
	}
	if got := backend.runnerBatches[1].BusyRunners; got != 1 {
		t.Fatalf("rust busy runners = %d, want 1", got)
	}
	if got := backend.runnerBatches[1].ProvisioningRunners; got != 1 {
		t.Fatalf("rust provisioning runners = %d, want 1", got)
	}

	if len(backend.hostBatches) != 2 {
		t.Fatalf("host batch count = %d, want 2", len(backend.hostBatches))
	}
	if backend.hostBatches[0].GroupID != "typescript" || backend.hostBatches[1].GroupID != "rust" {
		t.Fatalf("host group ids = %q, %q; want typescript, rust", backend.hostBatches[0].GroupID, backend.hostBatches[1].GroupID)
	}
	if got := *backend.hostBatches[0].RunnerContainersRunning; got != 1 {
		t.Fatalf("typescript running containers = %d, want 1", got)
	}
	if got := *backend.hostBatches[0].RunnerContainersStopped; got != 1 {
		t.Fatalf("typescript stopped containers = %d, want 1", got)
	}
	if got := *backend.hostBatches[1].RunnerContainersRunning; got != 2 {
		t.Fatalf("rust running containers = %d, want 2", got)
	}
}

func TestCollectAndPush_EnrichesOnlyFreshWorkflowRuns(t *testing.T) {
	runA := domain.WorkflowMetrics{
		RunID:       101,
		RunAttempt:  1,
		Repo:        "repo-a",
		Workflow:    "build",
		Conclusion:  "failure",
		DurationS:   90,
		RunNumber:   7,
		Event:       "push",
		Branch:      "main",
		CompletedAt: "2026-05-04T12:01:00Z",
	}
	runB := domain.WorkflowMetrics{
		RunID:       102,
		RunAttempt:  1,
		Repo:        "repo-a",
		Workflow:    "lint",
		Conclusion:  "failure",
		DurationS:   45,
		RunNumber:   8,
		Event:       "push",
		Branch:      "main",
		CompletedAt: "2026-05-04T12:03:00Z",
	}

	ci := &collectMetricsTestCI{
		metricsTestCI: metricsTestCI{prefix: "auto"},
		workflowRunsBatches: [][]domain.WorkflowMetrics{
			{runA},
			{runA, runB},
		},
		enrichFn: func(runs []domain.WorkflowMetrics) []domain.WorkflowMetrics {
			enriched := append([]domain.WorkflowMetrics(nil), runs...)
			for i := range enriched {
				enriched[i].FailureReason = "hydrated"
			}
			return enriched
		},
	}
	backend := &metricsRecorder{}
	daemon := New(
		Config{CollectWorkflows: true},
		nil,
		ci,
		backend,
		nil,
		nil,
		testLogger(),
	)

	daemon.collectAndPush(context.Background())
	daemon.collectAndPush(context.Background())

	if len(ci.enrichCalls) != 2 {
		t.Fatalf("enrich call count = %d, want 2", len(ci.enrichCalls))
	}
	if len(ci.enrichCalls[0]) != 1 || ci.enrichCalls[0][0].RunID != 101 {
		t.Fatalf("first enrich batch = %+v, want [101]", ci.enrichCalls[0])
	}
	if len(ci.enrichCalls[1]) != 1 || ci.enrichCalls[1][0].RunID != 102 {
		t.Fatalf("second enrich batch = %+v, want [102]", ci.enrichCalls[1])
	}
	if len(backend.workflowBatches) != 2 {
		t.Fatalf("workflow batch count = %d, want 2", len(backend.workflowBatches))
	}
	if got := backend.workflowBatches[0][0].FailureReason; got != "hydrated" {
		t.Fatalf("first workflow failure reason = %q, want hydrated", got)
	}
	if got := backend.workflowBatches[1][0].FailureReason; got != "hydrated" {
		t.Fatalf("second workflow failure reason = %q, want hydrated", got)
	}
}

func TestCollectAndPush_RetriesWorkflowMetricsAfterPushFailure(t *testing.T) {
	run := domain.WorkflowMetrics{
		Repo:       "repo-a",
		Workflow:   "build",
		Conclusion: "success",
		DurationS:  90,
		RunNumber:  7,
		Event:      "push",
		Branch:     "main",
	}

	ci := &collectMetricsTestCI{
		metricsTestCI: metricsTestCI{prefix: "auto"},
		workflowRunsBatches: [][]domain.WorkflowMetrics{
			{run},
			{run},
		},
	}
	backend := &metricsRecorder{
		workflowErrs: []error{errors.New("loki unavailable")},
	}
	daemon := New(
		Config{CollectWorkflows: true},
		nil,
		ci,
		backend,
		nil,
		nil,
		testLogger(),
	)

	daemon.collectAndPush(context.Background())
	daemon.collectAndPush(context.Background())

	if len(backend.workflowBatches) != 1 {
		t.Fatalf("workflow batch count = %d, want 1", len(backend.workflowBatches))
	}
	if len(backend.workflowBatches[0]) != 1 || backend.workflowBatches[0][0] != run {
		t.Fatalf("workflow batch = %+v, want [%+v]", backend.workflowBatches[0], run)
	}
}

func TestCollectAndPush_PushesLifecycleMetricsAndIssueEventsFromLogStore(t *testing.T) {
	store, err := NewLogStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewLogStore returned error: %v", err)
	}
	base := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	entries := []domain.LogEntry{
		{Time: base, Level: "INFO", Message: "queued", EventType: "workflow_job", Action: "queued", Repo: "Acme/repo", Workflow: "CI", Job: "integration", JobID: 41, RunID: 1001, RunAttempt: 1},
		{Time: base.Add(20 * time.Second), Level: "INFO", Message: "started", EventType: "workflow_job", Action: "in_progress", Repo: "Acme/repo", Workflow: "CI", Job: "integration", JobID: 41, RunID: 1001, RunAttempt: 1, Runner: "auto-1"},
		{Time: base.Add(30 * time.Second), Level: "INFO", Message: "scaled up", EventType: "scale_up", Action: "completed", Runner: "auto-1"},
		{Time: base.Add(90 * time.Second), Level: "INFO", Message: "queued", EventType: "workflow_job", Action: "queued", Repo: "Acme/repo", Workflow: "CI", Job: "deploy", JobID: 42, RunID: 1002, RunAttempt: 1},
		{Time: base.Add(110 * time.Second), Level: "INFO", Message: "started", EventType: "workflow_job", Action: "in_progress", Repo: "Acme/repo", Workflow: "CI", Job: "deploy", JobID: 42, RunID: 1002, RunAttempt: 1, Runner: "auto-1"},
		{Time: base.Add(4 * time.Minute), Level: "INFO", Message: "scaling down", EventType: "scale_down", Action: "started", Runner: "auto-1"},
		{Time: base.Add(9 * time.Minute), Level: "INFO", Message: "scale requested", EventType: "scale_up", Action: "requested", Runner: "auto-2"},
		{Time: base.Add(10 * time.Minute), Level: "WARN", Message: "failed to collect workflow metrics", Error: "rate limit exceeded", EventType: "workflow_metrics", Action: "failed", Repo: "Acme/repo", Branch: "main"},
	}
	for _, entry := range entries {
		if err := store.Record(entry); err != nil {
			t.Fatalf("Record returned error: %v", err)
		}
	}

	backend := &metricsRecorder{}
	daemon := New(
		Config{StateDir: t.TempDir()},
		nil,
		metricsTestCI{prefix: "auto"},
		backend,
		nil,
		store,
		testLogger(),
	)

	daemon.collectAndPush(context.Background())

	if len(backend.lifecycleBatches) != 1 {
		t.Fatalf("lifecycle batch count = %d, want 1", len(backend.lifecycleBatches))
	}
	lifecycle := backend.lifecycleBatches[0]
	if lifecycle.QueueWaitSamples != 2 {
		t.Fatalf("QueueWaitSamples = %d, want 2", lifecycle.QueueWaitSamples)
	}
	if lifecycle.AvgQueueWaitS != 20 {
		t.Fatalf("AvgQueueWaitS = %v, want 20", lifecycle.AvgQueueWaitS)
	}
	if lifecycle.LifecycleSamples != 1 {
		t.Fatalf("LifecycleSamples = %d, want 1", lifecycle.LifecycleSamples)
	}
	if lifecycle.AvgJobsPerLifecycle != 2 {
		t.Fatalf("AvgJobsPerLifecycle = %v, want 2", lifecycle.AvgJobsPerLifecycle)
	}
	if lifecycle.ReusedLifecyclePct != 100 {
		t.Fatalf("ReusedLifecyclePct = %v, want 100", lifecycle.ReusedLifecyclePct)
	}
	if lifecycle.ScaleDownToScaleUpSamples != 1 {
		t.Fatalf("ScaleDownToScaleUpSamples = %d, want 1", lifecycle.ScaleDownToScaleUpSamples)
	}
	if lifecycle.AvgScaleDownToScaleUpS != 300 {
		t.Fatalf("AvgScaleDownToScaleUpS = %v, want 300", lifecycle.AvgScaleDownToScaleUpS)
	}
	if len(backend.issueBatches) != 1 {
		t.Fatalf("issue batch count = %d, want 1", len(backend.issueBatches))
	}
	if len(backend.issueBatches[0]) != 1 {
		t.Fatalf("issue count = %d, want 1", len(backend.issueBatches[0]))
	}
	issue := backend.issueBatches[0][0]
	if issue.Reason != "rate limit exceeded" {
		t.Fatalf("issue reason = %q, want rate limit exceeded", issue.Reason)
	}
	if issue.Repo != "Acme/repo" {
		t.Fatalf("issue repo = %q, want Acme/repo", issue.Repo)
	}
}

func TestCollectAndPush_DoesNotReplayIssueTransportFailuresAsIssueEvents(t *testing.T) {
	store, err := NewLogStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewLogStore returned error: %v", err)
	}
	if err := store.Record(domain.LogEntry{
		Time:      time.Date(2026, 5, 4, 13, 0, 0, 0, time.UTC),
		Level:     "WARN",
		Message:   "failed to collect workflow metrics",
		Error:     "rate limit exceeded",
		EventType: "workflow_metrics",
		Action:    "failed",
		Repo:      "Acme/repo",
	}); err != nil {
		t.Fatalf("Record returned error: %v", err)
	}

	logger := slog.New(NewLogHandler(slog.NewTextHandler(io.Discard, nil), store))
	backend := &metricsRecorder{
		issueErrs: []error{errors.New("loki unavailable")},
	}
	daemon := New(
		Config{StateDir: t.TempDir()},
		nil,
		metricsTestCI{prefix: "auto"},
		backend,
		nil,
		store,
		logger,
	)

	daemon.collectAndPush(context.Background())
	daemon.collectAndPush(context.Background())

	if len(backend.issueBatches) != 1 {
		t.Fatalf("issue batch count = %d, want 1", len(backend.issueBatches))
	}
	if len(backend.issueBatches[0]) != 1 {
		t.Fatalf("issue count = %d, want 1", len(backend.issueBatches[0]))
	}
	if got := backend.issueBatches[0][0].Message; got != "failed to collect workflow metrics" {
		t.Fatalf("issue message = %q, want failed to collect workflow metrics", got)
	}
}

func TestCollectAndPush_ClearsPendingJobsWhenLifecycleEnds(t *testing.T) {
	store, err := NewLogStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewLogStore returned error: %v", err)
	}
	base := time.Date(2026, 5, 4, 14, 0, 0, 0, time.UTC)
	entries := []domain.LogEntry{
		{Time: base, Level: "INFO", Message: "old job started", EventType: "workflow_job", Action: "in_progress", Repo: "Acme/repo", Workflow: "CI", Job: "old", JobID: 51, RunID: 2001, RunAttempt: 1, Runner: "auto-1"},
		{Time: base.Add(30 * time.Second), Level: "INFO", Message: "old lifecycle scaling down", EventType: "scale_down", Action: "started", Runner: "auto-1"},
		{Time: base.Add(2 * time.Minute), Level: "INFO", Message: "new runner ready", EventType: "scale_up", Action: "completed", Runner: "auto-1"},
		{Time: base.Add(3 * time.Minute), Level: "INFO", Message: "new job started", EventType: "workflow_job", Action: "in_progress", Repo: "Acme/repo", Workflow: "CI", Job: "new", JobID: 52, RunID: 2002, RunAttempt: 1, Runner: "auto-1"},
		{Time: base.Add(5 * time.Minute), Level: "INFO", Message: "new lifecycle scaling down", EventType: "scale_down", Action: "started", Runner: "auto-1"},
	}
	for _, entry := range entries {
		if err := store.Record(entry); err != nil {
			t.Fatalf("Record returned error: %v", err)
		}
	}

	backend := &metricsRecorder{}
	daemon := New(
		Config{StateDir: t.TempDir()},
		nil,
		metricsTestCI{prefix: "auto"},
		backend,
		nil,
		store,
		testLogger(),
	)

	daemon.collectAndPush(context.Background())

	if len(backend.lifecycleBatches) != 1 {
		t.Fatalf("lifecycle batch count = %d, want 1", len(backend.lifecycleBatches))
	}
	lifecycle := backend.lifecycleBatches[0]
	if lifecycle.LifecycleSamples != 1 {
		t.Fatalf("LifecycleSamples = %d, want 1", lifecycle.LifecycleSamples)
	}
	if lifecycle.AvgJobsPerLifecycle != 1 {
		t.Fatalf("AvgJobsPerLifecycle = %v, want 1", lifecycle.AvgJobsPerLifecycle)
	}
	if lifecycle.ReusedLifecyclePct != 0 {
		t.Fatalf("ReusedLifecyclePct = %v, want 0", lifecycle.ReusedLifecyclePct)
	}
}

func TestCollectAndPush_SortsLifecycleEntriesBeforeAttribution(t *testing.T) {
	store, err := NewLogStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewLogStore returned error: %v", err)
	}
	base := time.Date(2026, 5, 4, 15, 0, 0, 0, time.UTC)
	entries := []domain.LogEntry{
		{Time: base.Add(30 * time.Second), Level: "INFO", Message: "old lifecycle scaling down", EventType: "scale_down", Action: "started", Runner: "auto-1"},
		{Time: base, Level: "INFO", Message: "delayed old job started", EventType: "workflow_job", Action: "in_progress", Repo: "Acme/repo", Workflow: "CI", Job: "old", JobID: 61, RunID: 3001, RunAttempt: 1, Runner: "auto-1"},
		{Time: base.Add(2 * time.Minute), Level: "INFO", Message: "new runner ready", EventType: "scale_up", Action: "completed", Runner: "auto-1"},
		{Time: base.Add(3 * time.Minute), Level: "INFO", Message: "new job started", EventType: "workflow_job", Action: "in_progress", Repo: "Acme/repo", Workflow: "CI", Job: "new", JobID: 62, RunID: 3002, RunAttempt: 1, Runner: "auto-1"},
		{Time: base.Add(5 * time.Minute), Level: "INFO", Message: "new lifecycle scaling down", EventType: "scale_down", Action: "started", Runner: "auto-1"},
	}
	for _, entry := range entries {
		if err := store.Record(entry); err != nil {
			t.Fatalf("Record returned error: %v", err)
		}
	}

	backend := &metricsRecorder{}
	daemon := New(
		Config{StateDir: t.TempDir()},
		nil,
		metricsTestCI{prefix: "auto"},
		backend,
		nil,
		store,
		testLogger(),
	)

	daemon.collectAndPush(context.Background())

	if len(backend.lifecycleBatches) != 1 {
		t.Fatalf("lifecycle batch count = %d, want 1", len(backend.lifecycleBatches))
	}
	lifecycle := backend.lifecycleBatches[0]
	if lifecycle.LifecycleSamples != 1 {
		t.Fatalf("LifecycleSamples = %d, want 1", lifecycle.LifecycleSamples)
	}
	if lifecycle.AvgJobsPerLifecycle != 1 {
		t.Fatalf("AvgJobsPerLifecycle = %v, want 1", lifecycle.AvgJobsPerLifecycle)
	}
	if lifecycle.ReusedLifecyclePct != 0 {
		t.Fatalf("ReusedLifecyclePct = %v, want 0", lifecycle.ReusedLifecyclePct)
	}
}
