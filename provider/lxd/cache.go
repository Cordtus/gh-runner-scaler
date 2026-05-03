package lxd

import (
	"context"
	"fmt"
	"path/filepath"
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

	script := cacheSetupScript(cm.symlinks)
	_, err := cm.runtime.ExecCommand(ctx, containerName, []string{"bash", "-c", script})
	if err != nil {
		return fmt.Errorf("setting up cache symlinks in %s: %w", containerName, err)
	}

	if toolCacheDir := toolCacheDir(cm.symlinks); toolCacheDir != "" {
		_, err := cm.runtime.ExecCommand(ctx, containerName, []string{"bash", "-c", runnerEnvScript(toolCacheDir)})
		if err != nil {
			return fmt.Errorf("configuring runner cache env in %s: %w", containerName, err)
		}
	}
	return nil
}

func cacheSetupScript(symlinks []config.SymlinkConfig) string {
	var lines []string
	lines = append(lines, "set -eu")
	for _, sl := range symlinks {
		source := shellQuote(sl.Source)
		target := shellQuote(sl.Target)
		parent := shellQuote(filepath.Dir(sl.Target))
		lines = append(lines,
			fmt.Sprintf("mkdir -p %s", source),
			fmt.Sprintf("chown runner:runner %s || true", source),
			fmt.Sprintf("mkdir -p %s", parent),
			fmt.Sprintf("if [ -d %s ] && [ ! -L %s ]; then", target, target),
			fmt.Sprintf("  cp -an %s/. %s/", target, source),
			fmt.Sprintf("  rm -rf %s", target),
			fmt.Sprintf("elif [ -e %s ] || [ -L %s ]; then", target, target),
			fmt.Sprintf("  rm -rf %s", target),
			"fi",
			fmt.Sprintf("ln -s %s %s", source, target),
			fmt.Sprintf("chown -h runner:runner %s || true", target),
		)
	}
	return strings.Join(lines, "\n")
}

func toolCacheDir(symlinks []config.SymlinkConfig) string {
	for _, sl := range symlinks {
		if filepath.Base(sl.Target) == "hostedtoolcache" {
			return sl.Target
		}
	}
	return ""
}

func runnerEnvScript(toolCache string) string {
	toolCacheLine := shellQuote("AGENT_TOOLSDIRECTORY=" + toolCache)
	runnerToolCacheLine := shellQuote("RUNNER_TOOL_CACHE=" + toolCache)
	return strings.Join([]string{
		"set -eu",
		`env_file=/home/runner/.env`,
		`tmp_file=$(mktemp)`,
		`if [ -f "$env_file" ]; then`,
		`  grep -v -E '^(AGENT_TOOLSDIRECTORY|RUNNER_TOOL_CACHE)=' "$env_file" > "$tmp_file" || true`,
		`fi`,
		fmt.Sprintf(`printf '%%s\n%%s\n' %s %s >> "$tmp_file"`, toolCacheLine, runnerToolCacheLine),
		`chown runner:runner "$tmp_file"`,
		`chmod 600 "$tmp_file"`,
		`mv "$tmp_file" "$env_file"`,
		`chown runner:runner "$env_file"`,
	}, "\n")
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

// Compile-time interface assertion is not possible here because CacheManager
// lives in provider/ which is outside internal/. The wiring in main.go
// handles type assignment.
var _ api.InstancePut // reference to suppress unused import lint (api is used above)
