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

func TestBuildRunnerMetrics_TracksAvailableOnlineSeparatelyFromIdle(t *testing.T) {
	runners := []domain.Runner{
		{ID: 1, Name: "permanent-1", Status: "online", Busy: true},
		{ID: 2, Name: "auto-1", Status: "online", Busy: false},
		{ID: 3, Name: "auto-2", Status: "offline", Busy: false},
	}

	metrics := buildRunnerMetrics(runners, metricsTestCI{prefix: "auto"})

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
	if metrics.UtilizationPct != 50 {
		t.Fatalf("UtilizationPct = %v, want 50", metrics.UtilizationPct)
	}
}

type collectMetricsTestCI struct {
	metricsTestCI
	runners             []domain.Runner
	runnersErr          error
	workflowRunsBatches [][]domain.WorkflowMetrics
	workflowCall        int
}

func (m *collectMetricsTestCI) ListRunners(_ context.Context) ([]domain.Runner, error) {
	if m.runnersErr != nil {
		return nil, m.runnersErr
	}
	return append([]domain.Runner(nil), m.runners...), nil
}

func (m *collectMetricsTestCI) ListRecentWorkflowRuns(_ context.Context, _ int) ([]domain.WorkflowMetrics, error) {
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

type metricsRecorder struct {
	runnerBatches   []domain.RunnerMetrics
	workflowBatches [][]domain.WorkflowMetrics
	hostBatches     []domain.HostMetrics
	workflowErrs    []error
	workflowCalls   int
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

type metricsTestRuntime struct {
	hostMetrics domain.HostMetrics
	hostErr     error
	containers  []domain.Container
	listErr     error
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

func (m metricsTestRuntime) ListContainers(context.Context, string) ([]domain.Container, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	return append([]domain.Container(nil), m.containers...), nil
}

func (m metricsTestRuntime) GetContainerStatus(context.Context, string) (domain.ContainerStatus, error) {
	return domain.StatusUnknown, nil
}

func (m metricsTestRuntime) HostMetrics(string) (domain.HostMetrics, error) {
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
	runtime := metricsTestRuntime{
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

func TestCollectAndPush_OmitsRunnerContainerSamplesWhenContainerListFails(t *testing.T) {
	ci := &collectMetricsTestCI{
		metricsTestCI: metricsTestCI{prefix: "auto"},
	}
	backend := &metricsRecorder{}
	runtime := metricsTestRuntime{
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
