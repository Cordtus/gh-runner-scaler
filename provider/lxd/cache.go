package lxd

import (
	"context"
	"fmt"
	"strings"

	"github.com/canonical/lxd/shared/api"

	"github.com/Cordtus/gh-runner-scaler/internal/config"
)

// CacheManager handles persistent cache volume attachment and symlink setup.
type CacheManager struct {
	runtime  *Runtime
	pool     string
	volume   string
	symlinks []config.SymlinkConfig
}

// NewCacheManager creates a CacheManager backed by the given Runtime.
func NewCacheManager(runtime *Runtime, pool, volume string, symlinks []config.SymlinkConfig) *CacheManager {
	return &CacheManager{
		runtime:  runtime,
		pool:     pool,
		volume:   volume,
		symlinks: symlinks,
	}
}

// AttachCache adds the shared cache volume as a disk device to the container.
// Equivalent to: lxc storage volume attach <pool> <volume> <container> /cache
func (cm *CacheManager) AttachCache(ctx context.Context, containerName string) error {
	inst, etag, err := cm.runtime.server.GetInstance(containerName)
	if err != nil {
		return fmt.Errorf("getting instance %s for cache attach: %w", containerName, err)
	}

	if inst.Devices == nil {
		inst.Devices = make(map[string]map[string]string)
	}

	inst.Devices["cache"] = map[string]string{
		"type":   "disk",
		"pool":   cm.pool,
		"source": cm.volume,
		"path":   "/cache",
	}

	op, err := cm.runtime.server.UpdateInstance(containerName, inst.Writable(), etag)
	if err != nil {
		return fmt.Errorf("attaching cache to %s: %w", containerName, err)
	}
	return waitOperation(ctx, op)
}

// SetupCacheSymlinks creates symlinks inside the container mapping standard
// tool paths to the cache mount point. The symlink list is driven by config.
func (cm *CacheManager) SetupCacheSymlinks(ctx context.Context, containerName string) error {
	if len(cm.symlinks) == 0 {
		return nil
	}

	// Build a shell script that creates all symlinks.
	// mkdir -p for parent dirs and ln -sfn for each mapping.
	var cmds []string
	for _, sl := range cm.symlinks {
		parent := sl.Target[:strings.LastIndex(sl.Target, "/")]
		cmds = append(cmds, fmt.Sprintf("mkdir -p '%s'", parent))
		cmds = append(cmds, fmt.Sprintf("ln -sfn '%s' '%s'", sl.Source, sl.Target))
	}

	script := strings.Join(cmds, " && ")
	_, err := cm.runtime.ExecCommand(ctx, containerName, []string{"bash", "-c", script})
	if err != nil {
		return fmt.Errorf("setting up cache symlinks in %s: %w", containerName, err)
	}
	return nil
}

// Compile-time interface assertion is not possible here because CacheManager
// lives in provider/ which is outside internal/. The wiring in main.go
// handles type assignment.
var _ api.InstancePut // reference to suppress unused import lint (api is used above)
