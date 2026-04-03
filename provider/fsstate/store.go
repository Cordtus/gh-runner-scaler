// Package fsstate implements iface.StateStore using filesystem timestamp files.
// Each container gets a .state/<name>.last_active file containing a Unix timestamp.
// This maintains backward compatibility with the bash scaler's state directory.
package fsstate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Cordtus/gh-runner-scaler/internal/domain"
)

const suffix = ".last_active"

// Store tracks container state via timestamp files on disk.
type Store struct {
	dir string
}

// New creates a Store rooted at dir, creating the directory if needed.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating state dir %s: %w", dir, err)
	}
	return &Store{dir: dir}, nil
}

// GetLastActive reads the last-active timestamp for a container.
func (s *Store) GetLastActive(_ context.Context, name string) (time.Time, error) {
	data, err := os.ReadFile(s.path(name))
	if err != nil {
		return time.Time{}, fmt.Errorf("reading state for %s: %w", name, err)
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing timestamp for %s: %w", name, err)
	}
	return time.Unix(ts, 0), nil
}

// SetLastActive writes the last-active timestamp for a container.
func (s *Store) SetLastActive(_ context.Context, name string, t time.Time) error {
	data := []byte(strconv.FormatInt(t.Unix(), 10) + "\n")
	if err := os.WriteFile(s.path(name), data, 0o644); err != nil {
		return fmt.Errorf("writing state for %s: %w", name, err)
	}
	return nil
}

// Create initializes state tracking for a new container (sets last-active to now).
func (s *Store) Create(ctx context.Context, name string) error {
	return s.SetLastActive(ctx, name, time.Now())
}

// Delete removes all state files for a container.
func (s *Store) Delete(_ context.Context, name string) error {
	pattern := filepath.Join(s.dir, name+".*")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Errorf("globbing state files for %s: %w", name, err)
	}
	for _, match := range matches {
		os.Remove(match)
	}
	return nil
}

// ListAll returns state for all tracked containers.
func (s *Store) ListAll(_ context.Context) (map[string]domain.ContainerState, error) {
	pattern := filepath.Join(s.dir, "*"+suffix)
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("listing state files: %w", err)
	}

	states := make(map[string]domain.ContainerState, len(matches))
	for _, match := range matches {
		base := filepath.Base(match)
		name := strings.TrimSuffix(base, suffix)

		data, err := os.ReadFile(match)
		if err != nil {
			continue
		}
		ts, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
		if err != nil {
			continue
		}

		states[name] = domain.ContainerState{
			Name:       name,
			LastActive: time.Unix(ts, 0),
		}
	}
	return states, nil
}

func (s *Store) path(name string) string {
	return filepath.Join(s.dir, name+suffix)
}
