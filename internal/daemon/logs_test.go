package daemon

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Cordtus/gh-runner-scaler/internal/domain"
)

func TestLogStoreHandler_PersistsStructuredFields(t *testing.T) {
	store, err := NewLogStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewLogStore returned error: %v", err)
	}
	logger := slog.New(NewLogHandler(slog.NewTextHandler(io.Discard, nil), store))

	logger.Info(
		"webhook event",
		"event_type", "workflow_job",
		"action", "completed",
		"repo", "Acme/repo",
		"workflow", "CI",
		"job", "integration",
		"runner", "gh-runner-auto-2",
		"commit", "0123456789abcdef",
		"run_id", int64(42),
		"run_attempt", 3,
	)

	entries := store.Query(logQuery{Runner: "gh-runner-auto-2", Commit: "01234567", EventType: "workflow_job"})
	if len(entries) != 1 {
		t.Fatalf("entries len = %d, want 1", len(entries))
	}
	entry := entries[0]
	if entry.Repo != "Acme/repo" {
		t.Fatalf("repo = %q, want Acme/repo", entry.Repo)
	}
	if entry.Workflow != "CI" {
		t.Fatalf("workflow = %q, want CI", entry.Workflow)
	}
	if entry.Job != "integration" {
		t.Fatalf("job = %q, want integration", entry.Job)
	}
	if entry.RunID != 42 {
		t.Fatalf("run id = %d, want 42", entry.RunID)
	}
	if entry.RunAttempt != 3 {
		t.Fatalf("run attempt = %d, want 3", entry.RunAttempt)
	}
}

func TestHandleLogs_FiltersAndRequiresBearerToken(t *testing.T) {
	store, err := NewLogStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewLogStore returned error: %v", err)
	}
	now := time.Now().UTC().Round(time.Second)
	if err := store.Record(domain.LogEntry{
		Time:      now,
		Level:     "INFO",
		Message:   "workflow event",
		EventType: "workflow_job",
		Action:    "completed",
		Repo:      "Acme/repo",
		Runner:    "gh-runner-auto-3",
		Commit:    "abcdef0123456789",
		Workflow:  "CI",
		Job:       "integration",
	}); err != nil {
		t.Fatalf("Record returned error: %v", err)
	}
	if err := store.Record(domain.LogEntry{
		Time:      now.Add(-time.Hour),
		Level:     "INFO",
		Message:   "cache sync completed",
		EventType: "cache_sync",
		Action:    "completed",
		Repo:      "Acme/other",
		Runner:    "gh-runner-auto-4",
		Commit:    "deadbeef01234567",
	}); err != nil {
		t.Fatalf("Record returned error: %v", err)
	}

	daemon := New(
		Config{LogsToken: "logs-token"},
		nil,
		metricsTestCI{},
		nil,
		nil,
		store,
		testLogger(),
	)

	req := httptest.NewRequest(http.MethodGet, "/logs?runner=gh-runner-auto-3&repo=Acme/repo&commit=abcdef01&since="+now.Add(-time.Minute).Format(time.RFC3339), nil)
	req.Header.Set("Authorization", "Bearer logs-token")
	recorder := httptest.NewRecorder()
	daemon.handleLogs(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}

	var response logResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if response.Count != 1 {
		t.Fatalf("count = %d, want 1", response.Count)
	}
	if got := response.Entries[0].Runner; got != "gh-runner-auto-3" {
		t.Fatalf("runner = %q, want gh-runner-auto-3", got)
	}

	unauthorizedReq := httptest.NewRequest(http.MethodGet, "/logs", nil)
	unauthorizedRecorder := httptest.NewRecorder()
	daemon.handleLogs(unauthorizedRecorder, unauthorizedReq)
	if unauthorizedRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want 401", unauthorizedRecorder.Code)
	}
}

func TestHandleLogs_RejectsInvalidSinceFilter(t *testing.T) {
	store, err := NewLogStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewLogStore returned error: %v", err)
	}

	daemon := New(
		Config{LogsToken: "logs-token"},
		nil,
		metricsTestCI{},
		nil,
		nil,
		store,
		testLogger(),
	)

	req := httptest.NewRequest(http.MethodGet, "/logs?since=not-a-time", nil)
	req.Header.Set("Authorization", "Bearer logs-token")
	recorder := httptest.NewRecorder()
	daemon.handleLogs(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
}
