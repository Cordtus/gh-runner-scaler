package engine

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Cordtus/gh-runner-scaler/internal/domain"
)

// --- Mock providers ---

type mockRuntime struct {
	mu         sync.Mutex
	containers map[string]domain.ContainerStatus
	execCalls  [][]string
	cloneErr   error
	execErr    error
}

func newMockRuntime() *mockRuntime {
	return &mockRuntime{containers: make(map[string]domain.ContainerStatus)}
}

func (m *mockRuntime) CloneFromTemplate(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cloneErr != nil {
		return m.cloneErr
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
	m.containers[name] = domain.StatusStopped
	return nil
}

func (m *mockRuntime) DeleteContainer(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.containers, name)
	return nil
}

func (m *mockRuntime) ExecCommand(_ context.Context, _ string, cmd []string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.execCalls = append(m.execCalls, cmd)
	return "", m.execErr
}

func (m *mockRuntime) WaitForReady(_ context.Context, _ string, _ []string, _ time.Duration) error {
	return nil
}

func (m *mockRuntime) ListContainers(_ context.Context, prefix string) ([]domain.Container, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []domain.Container
	for name, status := range m.containers {
		if len(prefix) == 0 || len(name) >= len(prefix) && name[:len(prefix)] == prefix {
			result = append(result, domain.Container{Name: name, Status: status})
		}
	}
	return result, nil
}

func (m *mockRuntime) GetContainerStatus(_ context.Context, name string) (domain.ContainerStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
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
}

func (m *mockCI) ListRunners(_ context.Context) ([]domain.Runner, error) {
	return m.runners, nil
}

func (m *mockCI) GetRegistrationToken(_ context.Context) (string, error) {
	return m.regToken, nil
}

func (m *mockCI) GetRemoveToken(_ context.Context) (string, error) {
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
	mu     sync.Mutex
	states map[string]time.Time
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
		runners:     []domain.Runner{{ID: 1, Name: "auto-1", Busy: false, Status: "offline"}},
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
