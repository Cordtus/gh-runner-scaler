package demand

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Cordtus/gh-runner-scaler/internal/domain"
)

func TestTrackerPersistsDeduplicatedDemandAndClearsCompletedJob(t *testing.T) {
	now := time.Date(2026, 7, 25, 18, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "demand.json")
	tracker, err := NewTracker(path, 30*time.Minute, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	job := domain.QueuedJob{ID: 42, Repo: "Cordtus/poolbet", Labels: []string{"runner-class-node"}, QueuedAt: now}

	if err := tracker.Queue("node", job); err != nil {
		t.Fatal(err)
	}
	if err := tracker.Queue("node", job); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewTracker(path, 30*time.Minute, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Snapshot("node").QueuedJobs; got != 1 {
		t.Fatalf("reloaded queued jobs = %d, want 1", got)
	}

	if err := reloaded.Clear("node", job.ID); err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Snapshot("node").QueuedJobs; got != 0 {
		t.Fatalf("queued jobs after completion = %d, want 0", got)
	}
}

func TestTrackerExpiresAbandonedDemand(t *testing.T) {
	now := time.Date(2026, 7, 25, 18, 0, 0, 0, time.UTC)
	tracker, err := NewTracker(filepath.Join(t.TempDir(), "demand.json"), 30*time.Minute, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := tracker.Queue("node", domain.QueuedJob{ID: 7, QueuedAt: now}); err != nil {
		t.Fatal(err)
	}

	now = now.Add(31 * time.Minute)
	if got := tracker.Snapshot("node").QueuedJobs; got != 0 {
		t.Fatalf("expired queued jobs = %d, want 0", got)
	}
}
