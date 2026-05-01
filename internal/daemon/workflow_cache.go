package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const workflowMetricCacheFile = "workflow_metrics_seen.json"

type workflowMetricCacheState struct {
	Keys []string `json:"keys"`
}

func (d *Daemon) loadWorkflowMetricCache() {
	if d.cfg.StateDir == "" {
		return
	}

	path := filepath.Join(d.cfg.StateDir, workflowMetricCacheFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			d.log.Warn("failed to read workflow metric cache", "path", path, "error", err)
		}
		return
	}

	var state workflowMetricCacheState
	if err := json.Unmarshal(data, &state); err != nil {
		d.log.Warn("failed to decode workflow metric cache", "path", path, "error", err)
		return
	}

	if len(state.Keys) > workflowMetricCacheLimit {
		state.Keys = state.Keys[len(state.Keys)-workflowMetricCacheLimit:]
	}

	d.workflowDelivered = make(map[string]struct{}, len(state.Keys))
	d.workflowDeliveredKeys = append(d.workflowDeliveredKeys[:0], state.Keys...)
	for _, key := range state.Keys {
		d.workflowDelivered[key] = struct{}{}
	}
}

func (d *Daemon) persistWorkflowMetricCacheLocked() error {
	if d.cfg.StateDir == "" {
		return nil
	}
	if err := os.MkdirAll(d.cfg.StateDir, 0o755); err != nil {
		return fmt.Errorf("creating workflow metric cache dir: %w", err)
	}

	payload, err := json.Marshal(workflowMetricCacheState{
		Keys: append([]string(nil), d.workflowDeliveredKeys...),
	})
	if err != nil {
		return fmt.Errorf("marshaling workflow metric cache: %w", err)
	}

	tmp, err := os.CreateTemp(d.cfg.StateDir, "workflow-metrics-*.tmp")
	if err != nil {
		return fmt.Errorf("creating workflow metric cache temp file: %w", err)
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
		return fmt.Errorf("writing workflow metric cache: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing workflow metric cache temp file: %w", err)
	}

	path := filepath.Join(d.cfg.StateDir, workflowMetricCacheFile)
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replacing workflow metric cache: %w", err)
	}

	cleanup = false
	return nil
}
