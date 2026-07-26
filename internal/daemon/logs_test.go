package daemon

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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
		"job_id", int64(991),
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
	if entry.JobID != 991 {
		t.Fatalf("job id = %d, want 991", entry.JobID)
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

func TestLogStore_RecordCompactsAfterRetentionTrim(t *testing.T) {
	store, err := NewLogStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewLogStore returned error: %v", err)
	}
	store.maxEntries = 2
	store.compactAt = 2

	for i := 0; i < 3; i++ {
		if err := store.Record(domain.LogEntry{
			Time:      time.Date(2026, 5, 4, 12, i, 0, 0, time.UTC),
			Level:     "INFO",
			Message:   "entry",
			EventType: "workflow_job",
			Action:    "completed",
			JobID:     int64(i + 1),
		}); err != nil {
			t.Fatalf("Record %d returned error: %v", i, err)
		}
	}

	_, entries := store.SnapshotWithVersion()
	if len(entries) != 2 {
		t.Fatalf("snapshot len = %d, want 2", len(entries))
	}
	if entries[0].JobID != 2 || entries[1].JobID != 3 {
		t.Fatalf("snapshot job IDs = [%d %d], want [2 3]", entries[0].JobID, entries[1].JobID)
	}

	data, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("persisted line count = %d, want 2", len(lines))
	}
}

func TestNewLogStore_IgnoresMalformedTrailingLine(t *testing.T) {
	stateDir := t.TempDir()
	path := stateDir + "/" + logStoreFile
	validEntry := `{"time":"2026-05-04T12:00:00Z","level":"INFO","message":"ok","event_type":"workflow_job","action":"completed","job_id":7}` + "\n"
	if err := os.WriteFile(path, []byte(validEntry+`{"time":"broken"`), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	store, err := NewLogStore(stateDir)
	if err != nil {
		t.Fatalf("NewLogStore returned error: %v", err)
	}

	entries := store.Query(logQuery{Limit: 10})
	if len(entries) != 1 {
		t.Fatalf("entries len = %d, want 1", len(entries))
	}
	if entries[0].JobID != 7 {
		t.Fatalf("job ID = %d, want 7", entries[0].JobID)
	}
}

func TestNewLogStore_RejectsMalformedPersistedEntry(t *testing.T) {
	stateDir := t.TempDir()
	path := stateDir + "/" + logStoreFile
	data := strings.Join([]string{
		`{"time":"2026-05-04T12:00:00Z","level":"INFO","message":"ok","event_type":"workflow_job","action":"completed","job_id":7}`,
		`{"time":}`,
		`{"time":"2026-05-04T12:01:00Z","level":"INFO","message":"still-ok","event_type":"workflow_job","action":"completed","job_id":8}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	_, err := NewLogStore(stateDir)
	if err == nil {
		t.Fatal("expected malformed persisted entry to fail load")
	}
}

func TestNewLogStore_IgnoresTruncatedTrailingLiteral(t *testing.T) {
	stateDir := t.TempDir()
	path := stateDir + "/" + logStoreFile
	validEntry := `{"time":"2026-05-04T12:00:00Z","level":"INFO","message":"ok","event_type":"workflow_job","action":"completed","job_id":7}` + "\n"
	if err := os.WriteFile(path, []byte(validEntry+`{"time":"2026-05-04T12:01:00Z","level":"INFO","message":"cut","event_type":"workflow_job","action":"completed","attributes":{"flag":tru`), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	store, err := NewLogStore(stateDir)
	if err != nil {
		t.Fatalf("NewLogStore returned error: %v", err)
	}

	entries := store.Query(logQuery{Limit: 10})
	if len(entries) != 1 {
		t.Fatalf("entries len = %d, want 1", len(entries))
	}
	if entries[0].JobID != 7 {
		t.Fatalf("job ID = %d, want 7", entries[0].JobID)
	}
}

func TestNewLogStore_RejectsMalformedTrailingLiteral(t *testing.T) {
	stateDir := t.TempDir()
	path := stateDir + "/" + logStoreFile
	validEntry := `{"time":"2026-05-04T12:00:00Z","level":"INFO","message":"ok","event_type":"workflow_job","action":"completed","job_id":7}` + "\n"
	if err := os.WriteFile(path, []byte(validEntry+`{"time":"2026-05-04T12:01:00Z","level":"INFO","message":"bad","event_type":"workflow_job","action":"completed","attributes":{"flag":trux}`), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	_, err := NewLogStore(stateDir)
	if err == nil {
		t.Fatal("expected malformed trailing literal to fail load")
	}
}

func TestNewLogStore_RejectsMixedCaseTrailingLiteralFragment(t *testing.T) {
	stateDir := t.TempDir()
	path := stateDir + "/" + logStoreFile
	validEntry := `{"time":"2026-05-04T12:00:00Z","level":"INFO","message":"ok","event_type":"workflow_job","action":"completed","job_id":7}` + "\n"
	if err := os.WriteFile(path, []byte(validEntry+`{"time":"2026-05-04T12:01:00Z","level":"INFO","message":"bad","event_type":"workflow_job","action":"completed","attributes":{"flag":Tr`), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	_, err := NewLogStore(stateDir)
	if err == nil {
		t.Fatal("expected mixed-case trailing literal fragment to fail load")
	}
}
