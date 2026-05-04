package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Cordtus/gh-runner-scaler/internal/domain"
)

const issueEventCacheFile = "issue_events_seen.json"

type issueEventCacheState struct {
	Keys []string `json:"keys"`
}

func (d *Daemon) loadIssueEventCache() {
	if d.cfg.StateDir == "" {
		return
	}

	path := filepath.Join(d.cfg.StateDir, issueEventCacheFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			d.log.Warn("failed to read issue event cache", "path", path, "error", err)
		}
		return
	}

	var state issueEventCacheState
	if err := json.Unmarshal(data, &state); err != nil {
		d.log.Warn("failed to decode issue event cache", "path", path, "error", err)
		return
	}

	if len(state.Keys) > workflowMetricCacheLimit {
		state.Keys = state.Keys[len(state.Keys)-workflowMetricCacheLimit:]
	}

	d.issueDelivered = make(map[string]struct{}, len(state.Keys))
	d.issueDeliveredKeys = append(d.issueDeliveredKeys[:0], state.Keys...)
	for _, key := range state.Keys {
		d.issueDelivered[key] = struct{}{}
	}
}

func (d *Daemon) filterNewIssueEntries(entries []domain.LogEntry) []domain.LogEntry {
	d.issueMu.Lock()
	defer d.issueMu.Unlock()

	if d.issueDelivered == nil {
		d.issueDelivered = make(map[string]struct{})
	}

	fresh := make([]domain.LogEntry, 0, len(entries))
	batchSeen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		key := issueEventKey(entry)
		if _, seen := d.issueDelivered[key]; seen {
			continue
		}
		if _, seen := batchSeen[key]; seen {
			continue
		}
		batchSeen[key] = struct{}{}
		fresh = append(fresh, entry)
	}
	return fresh
}

func (d *Daemon) markIssueEntriesDelivered(entries []domain.LogEntry) {
	d.issueMu.Lock()
	defer d.issueMu.Unlock()

	if d.issueDelivered == nil {
		d.issueDelivered = make(map[string]struct{})
	}

	for _, entry := range entries {
		key := issueEventKey(entry)
		if _, exists := d.issueDelivered[key]; exists {
			continue
		}
		d.issueDelivered[key] = struct{}{}
		d.issueDeliveredKeys = append(d.issueDeliveredKeys, key)
	}

	for len(d.issueDeliveredKeys) > workflowMetricCacheLimit {
		oldest := d.issueDeliveredKeys[0]
		d.issueDeliveredKeys = d.issueDeliveredKeys[1:]
		delete(d.issueDelivered, oldest)
	}

	if err := d.persistIssueEventCacheLocked(); err != nil {
		d.log.Warn("failed to persist issue event cache", "error", err)
	}
}

func (d *Daemon) persistIssueEventCacheLocked() error {
	if d.cfg.StateDir == "" {
		return nil
	}
	if err := os.MkdirAll(d.cfg.StateDir, 0o755); err != nil {
		return fmt.Errorf("creating issue event cache dir: %w", err)
	}

	payload, err := json.Marshal(issueEventCacheState{
		Keys: append([]string(nil), d.issueDeliveredKeys...),
	})
	if err != nil {
		return fmt.Errorf("marshaling issue event cache: %w", err)
	}

	tmp, err := os.CreateTemp(d.cfg.StateDir, "issue-events-*.tmp")
	if err != nil {
		return fmt.Errorf("creating issue event cache temp file: %w", err)
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
		return fmt.Errorf("writing issue event cache: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing issue event cache temp file: %w", err)
	}

	path := filepath.Join(d.cfg.StateDir, issueEventCacheFile)
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replacing issue event cache: %w", err)
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
