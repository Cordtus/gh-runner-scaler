package daemon

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Cordtus/gh-runner-scaler/internal/domain"
)

const (
	workflowMetricCacheFile = "workflow_metrics_seen.json"
	issueEventCacheFile     = "issue_events_seen.json"
)

// seenCache tracks keys already delivered (issues or workflow metrics),
// bounded to a limit in LRU order and persisted so the daemon does not
// re-deliver after a restart. Both callers (issue events and workflow
// metrics) share this one mechanism; only the key function differs.
type seenCache[T any] struct {
	mu    sync.Mutex
	path  string
	limit int
	keys  map[string]struct{}
	order []string
}

// newSeenCache returns a cache persisted to file under stateDir.
func newSeenCache[T any](stateDir, file string, limit int) *seenCache[T] {
	c := &seenCache[T]{
		path:  filepath.Join(stateDir, file),
		limit: limit,
		keys:  make(map[string]struct{}),
	}
	if stateDir == "" {
		c.path = ""
	}
	return c
}

func (c *seenCache[T]) load(log *slog.Logger, label string) {
	if c.path == "" {
		return
	}
	data, err := os.ReadFile(c.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Warn("failed to read cache", "cache", label, "path", c.path, "error", err)
		}
		return
	}
	var state struct {
		Keys []string `json:"keys"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		log.Warn("failed to decode cache", "cache", label, "path", c.path, "error", err)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.order = append(c.order[:0], state.Keys...)
	for _, key := range state.Keys {
		c.keys[key] = struct{}{}
	}
	c.trim()
}

// filterNew returns items whose keys have not been delivered, de-duplicating
// within a single batch as well.
func (c *seenCache[T]) filterNew(items []T, key func(T) string) []T {
	c.mu.Lock()
	defer c.mu.Unlock()
	fresh := make([]T, 0, len(items))
	batchSeen := make(map[string]struct{}, len(items))
	for _, item := range items {
		k := key(item)
		if _, seen := c.keys[k]; seen {
			continue
		}
		if _, seen := batchSeen[k]; seen {
			continue
		}
		batchSeen[k] = struct{}{}
		fresh = append(fresh, item)
	}
	return fresh
}

// markDelivered records keys as delivered, trims to the limit, and persists.
func (c *seenCache[T]) markDelivered(items []T, key func(T) string, log *slog.Logger, label string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, item := range items {
		k := key(item)
		if _, exists := c.keys[k]; exists {
			continue
		}
		c.keys[k] = struct{}{}
		c.order = append(c.order, k)
	}
	c.trim()
	if err := c.persistLocked(); err != nil {
		log.Warn("failed to persist cache", "cache", label, "error", err)
	}
}

func (c *seenCache[T]) trim() {
	for len(c.order) > c.limit {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.keys, oldest)
	}
}

func (c *seenCache[T]) persistLocked() error {
	if c.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return fmt.Errorf("creating cache dir: %w", err)
	}
	payload, err := json.Marshal(struct {
		Keys []string `json:"keys"`
	}{Keys: append([]string(nil), c.order...)})
	if err != nil {
		return fmt.Errorf("marshaling cache: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(c.path), "cache-*.tmp")
	if err != nil {
		return fmt.Errorf("creating cache temp file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(append(payload, '\n')); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing cache: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing cache temp file: %w", err)
	}
	if err := os.Rename(tmpPath, c.path); err != nil {
		return fmt.Errorf("replacing cache: %w", err)
	}
	cleanup = false
	return nil
}

func issueEventKey(entry domain.LogEntry) string {
	return fmt.Sprintf(
		"%s|%s|%s|%s|%s|%s|%s|%d|%d|%d|%s|%s",
		entry.Time.UTC().Format(time.RFC3339Nano),
		entry.Level,
		entry.EventType,
		entry.Action,
		entry.Repo,
		entry.Workflow,
		entry.Job,
		entry.JobID,
		entry.RunID,
		entry.RunAttempt,
		entry.Detail,
		entry.Error,
	)
}
