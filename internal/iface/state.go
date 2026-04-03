package iface

import (
	"context"
	"time"

	"github.com/Cordtus/gh-runner-scaler/internal/domain"
)

// StateStore abstracts container state tracking (last-active timestamps).
// The filesystem provider uses .state/ files; future providers could
// use Supabase, Redis, a SQL database, etc.
type StateStore interface {
	// GetLastActive returns the last-active timestamp for a container.
	GetLastActive(ctx context.Context, name string) (time.Time, error)

	// SetLastActive updates the last-active timestamp for a container.
	SetLastActive(ctx context.Context, name string, t time.Time) error

	// Create initializes state tracking for a newly scaled-up container.
	Create(ctx context.Context, name string) error

	// Delete removes all tracked state for a container (called on scale-down).
	Delete(ctx context.Context, name string) error

	// ListAll returns state for all tracked containers.
	ListAll(ctx context.Context) (map[string]domain.ContainerState, error)
}
