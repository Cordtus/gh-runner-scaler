package daemon

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestSeenCache_DedupTrimAndPersist(t *testing.T) {
	dir := t.TempDir()
	c := newSeenCache[string](dir, "test_seen.json", 3)

	key := func(s string) string { return s }

	// First pass: all fresh, in-batch duplicates removed, persisted.
	fresh := c.filterNew([]string{"a", "a", "b", "c"}, key)
	if len(fresh) != 3 {
		t.Fatalf("first filter returned %d items, want 3", len(fresh))
	}
	c.markDelivered([]string{"a", "b", "c"}, key, slog.Default(), "test")

	// Already-delivered keys are filtered out on the next pass.
	if got := c.filterNew([]string{"a", "b"}, key); len(got) != 0 {
		t.Fatalf("delivered keys were re-filtered as fresh: %v", got)
	}

	// Exceeds the bound: marking d,e evicts "a" (LRU), so it becomes fresh again.
	if got := c.filterNew([]string{"d", "e"}, key); len(got) != 2 {
		t.Fatalf("filter returned %d items, want 2", len(got))
	}
	c.markDelivered([]string{"d", "e"}, key, slog.Default(), "test")
	if got := c.filterNew([]string{"a"}, key); len(got) != 1 {
		t.Fatalf("evicted key did not re-appear: got %d, want 1", len(got))
	}

	// Reload from disk sees the same state as before the reload.
	reloaded := newSeenCache[string](dir, "test_seen.json", 3)
	reloaded.load(slog.Default(), "test")
	if got := reloaded.filterNew([]string{"c", "d", "e"}, key); len(got) != 0 {
		t.Fatalf("persisted keys were not loaded: %v", got)
	}
	// After trimming to 3 (c,d,e), both a and b were evicted.
	if got := reloaded.filterNew([]string{"a", "b"}, key); len(got) != 2 {
		t.Fatalf("evicted keys not persisted as evicted: got %d, want 2", len(got))
	}
}

func TestSeenCache_NoStateDirDoesNotPersist(t *testing.T) {
	c := newSeenCache[string]("", "test_seen.json", 3)
	key := func(s string) string { return s }
	if got := c.filterNew([]string{"x"}, key); len(got) != 1 {
		t.Fatalf("filter without state dir returned %d items, want 1", len(got))
	}
	c.markDelivered([]string{"x"}, key, slog.Default(), "test")
	if got := c.filterNew([]string{"x"}, key); len(got) != 0 {
		t.Fatalf("no-state-dir cache did not dedup: %v", got)
	}
	if _, err := os.ReadFile(filepath.Join("/nonexistent", "test_seen.json")); err == nil {
		t.Fatal("cache persisted despite empty state dir")
	}
}
