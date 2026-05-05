package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cordtus/gh-runner-scaler/internal/domain"
)

// --- Mock providers ---

type mockRuntime struct {
	mu          sync.Mutex
	containers  map[string]domain.ContainerStatus
	listed      map[string]domain.ContainerStatus
	execCalls   [][]string
	listCalls   int
	statusCalls int
	cloneHook   func(name string) error
	execHook    func(cmd []string) error
	statusErr   map[string]error
	cloneErr    error
	execErr     error
	stopErr     error
	deleteErr   error
}

func newMockRuntime() *mockRuntime {
	return &mockRuntime{
		containers: make(map[string]domain.ContainerStatus),
		statusErr:  make(map[string]error),
	}
}

func (m *mockRuntime) CloneFromTemplate(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cloneErr != nil {
		return m.cloneErr
	}
	if m.cloneHook != nil {
		if err := m.cloneHook(name); err != nil {
			return err
		}
	}
	m.containers[name] = domain.StatusStopped
	return nil
}

func (m *mockRuntime) StartContainer(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.containers[name] = domain.StatusRunning
	return nil
}

func (m *mockRuntime) StopContainer(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopErr != nil {
		return m.stopErr
	}
	m.containers[name] = domain.StatusStopped
	return nil
}

func (m *mockRuntime) DeleteContainer(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deleteErr != nil {
		return m.deleteErr
	}
	delete(m.containers, name)
	return nil
}

func (m *mockRuntime) ExecCommand(_ context.Context, _ string, cmd []string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.execCalls = append(m.execCalls, cmd)
	if m.execHook != nil {
		if err := m.execHook(cmd); err != nil {
			return "", err
		}
	}
	return "", m.execErr
}

func (m *mockRuntime) WaitForReady(_ context.Context, _ string, _ []string, _ time.Duration) error {
	return nil
}

func (m *mockRuntime) ListContainers(_ context.Context, prefix string) ([]domain.Container, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.listCalls++
	var result []domain.Container
	source := m.containers
	if m.listed != nil {
		source = m.listed
	}
	for name, status := range source {
		if len(prefix) == 0 || len(name) >= len(prefix) && name[:len(prefix)] == prefix {
			result = append(result, domain.Container{Name: name, Status: status})
		}
	}
	return result, nil
}

func (m *mockRuntime) GetContainerStatus(_ context.Context, name string) (domain.ContainerStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.statusCalls++
	if err := m.statusErr[name]; err != nil {
		return domain.StatusUnknown, err
	}
	s, ok := m.containers[name]
	if !ok {
		return domain.StatusUnknown, fmt.Errorf("not found: %s", name)
	}
	return s, nil
}

type mockCI struct {
	runners     []domain.Runner
	regToken    string
	removeToken string
	deletedIDs  []int64
	prefix      string
	removeCalls int
}

func (m *mockCI) ListRunners(_ context.Context) ([]domain.Runner, error) {
	return m.runners, nil
}

func (m *mockCI) GetRegistrationToken(_ context.Context) (string, error) {
	return m.regToken, nil
}

func (m *mockCI) GetRemoveToken(_ context.Context) (string, error) {
	m.removeCalls++
	return m.removeToken, nil
}

func (m *mockCI) DeleteRunner(_ context.Context, id int64) error {
	m.deletedIDs = append(m.deletedIDs, id)
	return nil
}

func (m *mockCI) RegistrationURL() string { return "https://github.com/test-org" }

func (m *mockCI) ClassifyRunner(name string) bool {
	return len(name) >= len(m.prefix) && name[:len(m.prefix)] == m.prefix
}

func (m *mockCI) ValidateWebhookPayload(_ []byte, _ string) error { return nil }

func (m *mockCI) ParseWebhookEvent(_ string, _ []byte) (*domain.WebhookEvent, error) {
	return nil, nil
}

func (m *mockCI) ListRecentWorkflowRuns(_ context.Context, _ int) ([]domain.WorkflowMetrics, error) {
	return nil, nil
}

func (m *mockCI) EnrichWorkflowMetrics(_ context.Context, runs []domain.WorkflowMetrics) ([]domain.WorkflowMetrics, error) {
	return append([]domain.WorkflowMetrics(nil), runs...), nil
}

type mockCache struct {
	attached []string
	symlinks []string
}

func (m *mockCache) AttachCache(_ context.Context, name string) error {
	m.attached = append(m.attached, name)
	return nil
}

func (m *mockCache) SetupCacheSymlinks(_ context.Context, name string) error {
	m.symlinks = append(m.symlinks, name)
	return nil
}

type mockState struct {
	mu        sync.Mutex
	states    map[string]time.Time
	deleteErr error
}

func newMockState() *mockState {
	return &mockState{states: make(map[string]time.Time)}
}

func (m *mockState) GetLastActive(_ context.Context, name string) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.states[name]
	if !ok {
		return time.Time{}, fmt.Errorf("not found: %s", name)
	}
	return t, nil
}

func (m *mockState) SetLastActive(_ context.Context, name string, t time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.states[name] = t
	return nil
}

func (m *mockState) Create(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.states[name] = time.Now()
	return nil
}

func (m *mockState) Delete(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deleteErr != nil {
		return m.deleteErr
	}
	delete(m.states, name)
	return nil
}

func (m *mockState) ListAll(_ context.Context) (map[string]domain.ContainerState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make(map[string]domain.ContainerState)
	for k, v := range m.states {
		result[k] = domain.ContainerState{Name: k, LastActive: v}
	}
	return result, nil
}

// --- Tests ---

func newTestReconciler(runtime *mockRuntime, ci *mockCI, state *mockState, cache *mockCache) *Reconciler {
	return NewReconciler(
		ReconcilerConfig{
			Prefix:         "auto",
			MaxAutoRunners: 3,
			IdleTimeout:    5 * time.Minute,
			Labels:         "self-hosted",
			RunnerWorkDir:  "_work",
			CacheEnabled:   cache != nil,
		},
		runtime, cache, ci, state, nil,
	)
}

func TestScaleUp_WhenAllBusy(t *testing.T) {
	runtime := newMockRuntime()
	ci := &mockCI{
		runners:  []domain.Runner{{ID: 1, Name: "permanent", Busy: true, Status: "online"}},
		regToken: "test-token",
		prefix:   "auto",
	}
	state := newMockState()
	r := newTestReconciler(runtime, ci, state, nil)

	err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	// Should have created a new container.
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if _, ok := runtime.containers["auto-1"]; !ok {
		t.Error("expected auto-1 container to be created")
	}
	if runtime.listCalls != 1 {
		t.Fatalf("expected one container listing, got %d", runtime.listCalls)
	}
}

func TestScaleUp_ReusesExistingContainerSnapshotForNextName(t *testing.T) {
	runtime := newMockRuntime()
	runtime.containers["auto-1"] = domain.StatusRunning

	ci := &mockCI{
		runners: []domain.Runner{
			{ID: 1, Name: "permanent", Busy: true, Status: "online"},
			{ID: 2, Name: "auto-1", Busy: true, Status: "online"},
		},
		regToken: "test-token",
		prefix:   "auto",
	}
	state := newMockState()
	r := newTestReconciler(runtime, ci, state, nil)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if _, ok := runtime.containers["auto-2"]; !ok {
		t.Fatal("expected auto-2 container to be created")
	}
	if runtime.listCalls != 1 {
		t.Fatalf("expected one container listing, got %d", runtime.listCalls)
	}
}

func TestNoScaleUp_WhenIdleRunnerExists(t *testing.T) {
	runtime := newMockRuntime()
	ci := &mockCI{
		runners: []domain.Runner{
			{ID: 1, Name: "permanent", Busy: false, Status: "online"},
		},
		regToken: "test-token",
		prefix:   "auto",
	}
	state := newMockState()
	r := newTestReconciler(runtime, ci, state, nil)

	err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if len(runtime.containers) > 0 {
		t.Error("should not have created any containers when idle runner exists")
	}
}

func TestScaleUp_WhenOnlyOfflineRunnerExists(t *testing.T) {
	runtime := newMockRuntime()
	ci := &mockCI{
		runners: []domain.Runner{
			{ID: 1, Name: "permanent", Busy: false, Status: "offline"},
		},
		regToken: "test-token",
		prefix:   "auto",
	}
	state := newMockState()
	r := newTestReconciler(runtime, ci, state, nil)

	err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if _, ok := runtime.containers["auto-1"]; !ok {
		t.Error("expected auto-1 container to be created when only offline runners exist")
	}
}

func TestNoScaleUp_WhenAtMax(t *testing.T) {
	runtime := newMockRuntime()
	runtime.containers["auto-1"] = domain.StatusRunning
	runtime.containers["auto-2"] = domain.StatusRunning
	runtime.containers["auto-3"] = domain.StatusRunning

	ci := &mockCI{
		runners: []domain.Runner{
			{ID: 1, Name: "auto-1", Busy: true, Status: "online"},
			{ID: 2, Name: "auto-2", Busy: true, Status: "online"},
			{ID: 3, Name: "auto-3", Busy: true, Status: "online"},
		},
		regToken: "test-token",
		prefix:   "auto",
	}
	state := newMockState()
	r := newTestReconciler(runtime, ci, state, nil)

	err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if len(runtime.containers) != 3 {
		t.Errorf("expected 3 containers, got %d", len(runtime.containers))
	}
}

func TestScaleDown_StoppedContainer(t *testing.T) {
	runtime := newMockRuntime()
	runtime.containers["auto-1"] = domain.StatusStopped

	ci := &mockCI{
		runners: []domain.Runner{
			{ID: 1, Name: "auto-1", Busy: false, Status: "offline"},
			{ID: 2, Name: "permanent", Busy: false, Status: "online"},
		},
		removeToken: "remove-token",
		prefix:      "auto",
	}
	state := newMockState()
	state.states["auto-1"] = time.Now()

	r := newTestReconciler(runtime, ci, state, nil)

	err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if _, ok := runtime.containers["auto-1"]; ok {
		t.Error("stopped container should have been deleted")
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if _, ok := state.states["auto-1"]; ok {
		t.Error("state should have been cleaned up")
	}
	if runtime.statusCalls != 1 {
		t.Fatalf("expected listed stopped status to be refreshed before deletion, got %d lookups", runtime.statusCalls)
	}
}

func TestReconcile_RefreshesContainerStatusesBeforeScaleUpCapacityDecision(t *testing.T) {
	runtime := newMockRuntime()
	runtime.listed = map[string]domain.ContainerStatus{"auto-1": domain.StatusRunning}
	runtime.containers["auto-1"] = domain.StatusStopped

	ci := &mockCI{
		runners:     []domain.Runner{{ID: 1, Name: "auto-1", Busy: true, Status: "online"}},
		regToken:    "test-token",
		removeToken: "remove-token",
		prefix:      "auto",
	}
	state := newMockState()
	state.states["auto-1"] = time.Now()

	r := newTestReconciler(runtime, ci, state, nil)
	r.cfg.MaxAutoRunners = 1

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.statusCalls == 0 {
		t.Fatal("expected live status refresh when no idle runners are available")
	}
	if status := runtime.containers["auto-1"]; status != domain.StatusRunning {
		t.Fatalf("auto-1 status = %v, want running after replacement scale-up", status)
	}
	if len(ci.deletedIDs) != 1 || ci.deletedIDs[0] != 1 {
		t.Fatalf("deleted runner IDs = %v, want [1]", ci.deletedIDs)
	}
}

func TestReconcile_ReplacesDeletedContainerDespiteBestEffortScaleDownErrors(t *testing.T) {
	runtime := newMockRuntime()
	runtime.containers["auto-1"] = domain.StatusStopped
	runtime.execHook = func(cmd []string) error {
		if strings.Contains(strings.Join(cmd, " "), "./svc.sh stop") {
			return errors.New("svc stop failed")
		}
		return nil
	}

	ci := &mockCI{
		runners:     []domain.Runner{{ID: 1, Name: "auto-1", Busy: false, Status: "offline"}},
		regToken:    "test-token",
		removeToken: "remove-token",
		prefix:      "auto",
	}
	state := newMockState()
	state.states["auto-1"] = time.Now()

	r := newTestReconciler(runtime, ci, state, nil)
	r.cfg.MaxAutoRunners = 1

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if status := runtime.containers["auto-1"]; status != domain.StatusRunning {
		t.Fatalf("auto-1 status = %v, want running replacement after delete-with-warning", status)
	}
	if len(ci.deletedIDs) != 1 || ci.deletedIDs[0] != 1 {
		t.Fatalf("deleted runner IDs = %v, want [1]", ci.deletedIDs)
	}
}

func TestReconcile_DoesNotDeleteStoppedSnapshotWhenLiveRefreshFails(t *testing.T) {
	runtime := newMockRuntime()
	runtime.listed = map[string]domain.ContainerStatus{"auto-1": domain.StatusStopped}
	runtime.containers["auto-1"] = domain.StatusRunning
	runtime.statusErr["auto-1"] = errors.New("status lookup failed")

	ci := &mockCI{
		runners: []domain.Runner{
			{ID: 1, Name: "auto-1", Busy: true, Status: "online"},
		},
		regToken:    "test-token",
		removeToken: "remove-token",
		prefix:      "auto",
	}
	state := newMockState()
	state.states["auto-1"] = time.Now()

	r := newTestReconciler(runtime, ci, state, nil)
	r.cfg.MaxAutoRunners = 1

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if _, ok := runtime.containers["auto-1"]; !ok {
		t.Fatal("expected container to remain when live status refresh fails")
	}
	if len(ci.deletedIDs) != 0 {
		t.Fatalf("deleted runner IDs = %v, want none", ci.deletedIDs)
	}
}

func TestReconcile_RefreshesListedStoppedContainerBeforeDeletingIt(t *testing.T) {
	runtime := newMockRuntime()
	runtime.listed = map[string]domain.ContainerStatus{"auto-1": domain.StatusStopped}
	runtime.containers["auto-1"] = domain.StatusRunning

	ci := &mockCI{
		runners: []domain.Runner{
			{ID: 1, Name: "auto-1", Busy: false, Status: "online"},
			{ID: 2, Name: "permanent", Busy: false, Status: "online"},
		},
		removeToken: "remove-token",
		prefix:      "auto",
	}
	state := newMockState()
	state.states["auto-1"] = time.Now()

	r := newTestReconciler(runtime, ci, state, nil)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if _, ok := runtime.containers["auto-1"]; !ok {
		t.Fatal("expected running container to survive stale listed stopped status")
	}
	if len(ci.deletedIDs) != 0 {
		t.Fatalf("deleted runner IDs = %v, want none", ci.deletedIDs)
	}
}

func TestScaleDown_IdleTimeout(t *testing.T) {
	runtime := newMockRuntime()
	runtime.containers["auto-1"] = domain.StatusRunning

	ci := &mockCI{
		runners:     []domain.Runner{{ID: 1, Name: "auto-1", Busy: false, Status: "online"}},
		removeToken: "remove-token",
		prefix:      "auto",
	}
	state := newMockState()
	state.states["auto-1"] = time.Now().Add(-10 * time.Minute) // idle for 10 min

	r := newTestReconciler(runtime, ci, state, nil)

	err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if _, ok := runtime.containers["auto-1"]; ok {
		t.Error("idle container should have been scaled down")
	}
}

func TestNoScaleDown_BusyRunner(t *testing.T) {
	runtime := newMockRuntime()
	runtime.containers["auto-1"] = domain.StatusRunning

	ci := &mockCI{
		runners: []domain.Runner{{ID: 1, Name: "auto-1", Busy: true, Status: "online"}},
		prefix:  "auto",
	}
	state := newMockState()
	state.states["auto-1"] = time.Now().Add(-10 * time.Minute) // old timestamp

	r := newTestReconciler(runtime, ci, state, nil)

	err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if _, ok := runtime.containers["auto-1"]; !ok {
		t.Error("busy container should NOT have been scaled down")
	}

	// Last-active should have been updated.
	state.mu.Lock()
	defer state.mu.Unlock()
	if time.Since(state.states["auto-1"]) > 5*time.Second {
		t.Error("last-active should have been refreshed")
	}
}

func TestScaleUp_WithCache(t *testing.T) {
	runtime := newMockRuntime()
	ci := &mockCI{
		runners:  []domain.Runner{{ID: 1, Name: "permanent", Busy: true, Status: "online"}},
		regToken: "test-token",
		prefix:   "auto",
	}
	state := newMockState()
	cache := &mockCache{}
	r := newTestReconciler(runtime, ci, state, cache)

	err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	if len(cache.attached) != 1 || cache.attached[0] != "auto-1" {
		t.Errorf("expected cache attached to auto-1, got %v", cache.attached)
	}
	if len(cache.symlinks) != 1 || cache.symlinks[0] != "auto-1" {
		t.Errorf("expected symlinks set up for auto-1, got %v", cache.symlinks)
	}
}

func TestScaleDown_OrphanedContainer(t *testing.T) {
	runtime := newMockRuntime()
	runtime.containers["auto-1"] = domain.StatusRunning

	ci := &mockCI{
		// No runners match auto-1 -- it's orphaned.
		runners:     []domain.Runner{{ID: 1, Name: "permanent", Busy: false, Status: "online"}},
		removeToken: "remove-token",
		prefix:      "auto",
	}
	state := newMockState()

	r := newTestReconciler(runtime, ci, state, nil)

	err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if _, ok := runtime.containers["auto-1"]; ok {
		t.Error("orphaned container should have been cleaned up")
	}
}

func TestScaleDown_ReturnsCleanupErrors(t *testing.T) {
	runtime := newMockRuntime()
	runtime.containers["auto-1"] = domain.StatusRunning
	runtime.stopErr = errors.New("stop failed")
	runtime.deleteErr = errors.New("delete failed")

	ci := &mockCI{
		runners:     []domain.Runner{{ID: 1, Name: "auto-1", Busy: false, Status: "offline"}},
		removeToken: "remove-token",
		prefix:      "auto",
	}
	state := newMockState()
	state.deleteErr = errors.New("state delete failed")

	r := newTestReconciler(runtime, ci, state, nil)

	result := r.scaleDown(context.Background(), "auto-1", ci.runners, &reconcilePass{removeToken: ci.removeToken, removeTokenFetched: true})
	err := result.err
	if err == nil {
		t.Fatal("expected scaleDown to return cleanup errors")
	}
	if result.deleted {
		t.Fatal("delete-container failure should not mark the scale-down as deleted")
	}
	if !strings.Contains(err.Error(), "stop container") {
		t.Fatalf("expected stop container error in %v", err)
	}
	if !strings.Contains(err.Error(), "delete container") {
		t.Fatalf("expected delete container error in %v", err)
	}
	if !strings.Contains(err.Error(), "delete state") {
		t.Fatalf("expected delete state error in %v", err)
	}
}

func TestReconcile_CachesRemoveTokenAcrossScaleDownsInOnePass(t *testing.T) {
	runtime := newMockRuntime()
	runtime.containers["auto-1"] = domain.StatusStopped
	runtime.containers["auto-2"] = domain.StatusStopped

	ci := &mockCI{
		runners: []domain.Runner{
			{ID: 1, Name: "auto-1", Busy: false, Status: "offline"},
			{ID: 2, Name: "auto-2", Busy: false, Status: "offline"},
		},
		removeToken: "remove-token",
		prefix:      "auto",
	}
	state := newMockState()
	state.states["auto-1"] = time.Now()
	state.states["auto-2"] = time.Now()

	r := newTestReconciler(runtime, ci, state, nil)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}
	if ci.removeCalls != 1 {
		t.Fatalf("GetRemoveToken calls = %d, want 1", ci.removeCalls)
	}
}

func TestReconcile_FallsBackToStatusLookupWhenListedStatusUnknown(t *testing.T) {
	runtime := newMockRuntime()
	runtime.containers["auto-1"] = domain.StatusUnknown

	ci := &mockCI{
		runners:     []domain.Runner{{ID: 1, Name: "auto-1", Busy: false, Status: "offline"}},
		removeToken: "remove-token",
		prefix:      "auto",
	}
	state := newMockState()
	state.states["auto-1"] = time.Now()

	r := newTestReconciler(runtime, ci, state, nil)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}
	if runtime.statusCalls == 0 {
		t.Fatal("expected unknown listed status to trigger a direct status lookup")
	}
}

func TestScaleUp_RetriesAfterContainerNameConflict(t *testing.T) {
	runtime := newMockRuntime()
	runtime.containers["auto-1"] = domain.StatusRunning
	collided := false
	runtime.cloneHook = func(name string) error {
		if name == "auto-2" && !collided {
			collided = true
			runtime.containers["auto-2"] = domain.StatusRunning
			return errors.New("instance already exists")
		}
		return nil
	}

	ci := &mockCI{
		runners: []domain.Runner{
			{ID: 1, Name: "permanent", Busy: true, Status: "online"},
			{ID: 2, Name: "auto-1", Busy: true, Status: "online"},
		},
		regToken: "test-token",
		prefix:   "auto",
	}
	state := newMockState()
	r := newTestReconciler(runtime, ci, state, nil)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if _, ok := runtime.containers["auto-3"]; !ok {
		t.Fatalf("expected retry to create auto-3 after conflict, containers = %#v", runtime.containers)
	}
	if runtime.listCalls != 2 {
		t.Fatalf("expected conflict retry to refresh container list, got %d list calls", runtime.listCalls)
	}
}

func TestBuildSnapshot(t *testing.T) {
	runners := []domain.Runner{
		{ID: 1, Name: "permanent", Busy: true, Status: "online"},
		{ID: 2, Name: "auto-1", Busy: false, Status: "online"},
		{ID: 3, Name: "auto-2", Busy: true, Status: "offline"},
	}

	snap := buildSnapshot(runners, "auto")

	if snap.Total != 3 {
		t.Errorf("total: got %d, want 3", snap.Total)
	}
	if snap.Busy != 2 {
		t.Errorf("busy: got %d, want 2", snap.Busy)
	}
	if snap.Idle != 1 {
		t.Errorf("idle: got %d, want 1", snap.Idle)
	}
	if snap.Online != 2 {
		t.Errorf("online: got %d, want 2", snap.Online)
	}
	if snap.Auto != 2 {
		t.Errorf("auto: got %d, want 2", snap.Auto)
	}
	if snap.Permanent != 1 {
		t.Errorf("permanent: got %d, want 1", snap.Permanent)
	}
}
