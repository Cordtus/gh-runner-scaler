// Package demand persists queued workflow jobs until GitHub reports that they
// started or completed.
package demand

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/Cordtus/gh-runner-scaler/internal/domain"
)

type Clock func() time.Time

type persistedState struct {
	Groups map[string]map[int64]domain.QueuedJob `json:"groups"`
}

// Tracker stores demand by runner group and job ID.
type Tracker struct {
	mu    sync.Mutex
	path  string
	ttl   time.Duration
	now   Clock
	state persistedState
}

func NewTracker(path string, ttl time.Duration, now Clock) (*Tracker, error) {
	if path == "" {
		return nil, errors.New("demand state path is required")
	}
	if ttl <= 0 {
		return nil, errors.New("demand TTL must be positive")
	}
	if now == nil {
		now = time.Now
	}

	tracker := &Tracker{
		path: path,
		ttl:  ttl,
		now:  now,
		state: persistedState{
			Groups: make(map[string]map[int64]domain.QueuedJob),
		},
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return tracker, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read demand state: %w", err)
	}
	if err := json.Unmarshal(data, &tracker.state); err != nil {
		return nil, fmt.Errorf("decode demand state: %w", err)
	}
	if tracker.state.Groups == nil {
		tracker.state.Groups = make(map[string]map[int64]domain.QueuedJob)
	}
	tracker.pruneLocked()
	return tracker, nil
}

func (t *Tracker) Queue(groupID string, job domain.QueuedJob) error {
	if groupID == "" {
		return errors.New("runner group is required")
	}
	if job.ID == 0 {
		return errors.New("job ID is required")
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked()
	if job.QueuedAt.IsZero() {
		job.QueuedAt = t.now()
	}
	if t.state.Groups[groupID] == nil {
		t.state.Groups[groupID] = make(map[int64]domain.QueuedJob)
	}
	t.state.Groups[groupID][job.ID] = job
	return t.persistLocked()
}

func (t *Tracker) Clear(groupID string, jobID int64) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked()
	if jobs := t.state.Groups[groupID]; jobs != nil {
		delete(jobs, jobID)
		if len(jobs) == 0 {
			delete(t.state.Groups, groupID)
		}
	}
	return t.persistLocked()
}

// Replace atomically refreshes one group's demand from an authoritative audit.
func (t *Tracker) Replace(groupID string, jobs []domain.QueuedJob) error {
	if groupID == "" {
		return errors.New("runner group is required")
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked()
	replacement := make(map[int64]domain.QueuedJob, len(jobs))
	for _, job := range jobs {
		if job.ID == 0 {
			continue
		}
		if job.QueuedAt.IsZero() {
			job.QueuedAt = t.now()
		}
		replacement[job.ID] = job
	}
	if len(replacement) == 0 {
		delete(t.state.Groups, groupID)
	} else {
		t.state.Groups[groupID] = replacement
	}
	return t.persistLocked()
}

func (t *Tracker) Snapshot(groupID string) domain.CapacityDemand {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked()

	jobs := t.state.Groups[groupID]
	demand := domain.CapacityDemand{QueuedJobs: len(jobs)}
	now := t.now()
	for _, job := range jobs {
		age := now.Sub(job.QueuedAt)
		if age > demand.OldestAge {
			demand.OldestAge = age
		}
	}
	return demand
}

func (t *Tracker) ActiveGroups() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked()

	groups := make([]string, 0, len(t.state.Groups))
	for groupID, jobs := range t.state.Groups {
		if len(jobs) > 0 {
			groups = append(groups, groupID)
		}
	}
	sort.Strings(groups)
	return groups
}

func (t *Tracker) pruneLocked() {
	cutoff := t.now().Add(-t.ttl)
	for groupID, jobs := range t.state.Groups {
		for jobID, job := range jobs {
			if job.QueuedAt.Before(cutoff) {
				delete(jobs, jobID)
			}
		}
		if len(jobs) == 0 {
			delete(t.state.Groups, groupID)
		}
	}
}

func (t *Tracker) persistLocked() error {
	if err := os.MkdirAll(filepath.Dir(t.path), 0o750); err != nil {
		return fmt.Errorf("create demand state directory: %w", err)
	}
	data, err := json.MarshalIndent(t.state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode demand state: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(t.path), ".demand-*.tmp")
	if err != nil {
		return fmt.Errorf("create demand state temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("set demand state permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write demand state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync demand state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close demand state: %w", err)
	}
	if err := os.Rename(tmpPath, t.path); err != nil {
		return fmt.Errorf("replace demand state: %w", err)
	}
	return nil
}
