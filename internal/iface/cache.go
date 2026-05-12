package iface

import (
	"context"

	"github.com/Cordtus/gh-runner-scaler/internal/domain"
)

// CacheManager abstracts persistent cache volume operations.
// Attaching a shared volume and setting up symlinks inside the container
// are separate steps because the implementation may differ by provider.
type CacheManager interface {
	// AttachCache mounts the shared cache volume to the named container.
	AttachCache(ctx context.Context, containerName string) error

	// SetupCacheSymlinks creates symlinks inside the container mapping
	// standard tool paths to the cache mount point.
	SetupCacheSymlinks(ctx context.Context, containerName string) error

	// PruneCache removes stale bounded cache entries when policy allows it.
	PruneCache(ctx context.Context, containerName string, policy domain.CachePrunePolicy) error
}
