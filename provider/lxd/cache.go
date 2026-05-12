package lxd

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/canonical/lxd/shared/api"

	"github.com/Cordtus/gh-runner-scaler/internal/config"
	"github.com/Cordtus/gh-runner-scaler/internal/domain"
)

const buildxCacheRoot = "/cache/buildx"

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
	script := cacheSetupScript(cm.symlinks)
	_, err := cm.runtime.ExecCommand(ctx, containerName, []string{"bash", "-c", script})
	if err != nil {
		return fmt.Errorf("setting up cache symlinks in %s: %w", containerName, err)
	}

	_, err = cm.runtime.ExecCommand(ctx, containerName, []string{"bash", "-c", runnerEnvScript(toolCacheDir(cm.symlinks))})
	if err != nil {
		return fmt.Errorf("configuring runner cache env in %s: %w", containerName, err)
	}
	return nil
}

// PruneCache removes stale bounded shared-cache entries from inside a container
// with the cache volume attached. A stamp file on /cache throttles real cleanup
// across daemon restarts and concurrent scale-up attempts.
func (cm *CacheManager) PruneCache(ctx context.Context, containerName string, policy domain.CachePrunePolicy) error {
	if !policy.Enabled {
		return nil
	}
	script := cachePruneScript(policy)
	_, err := cm.runtime.ExecCommand(ctx, containerName, []string{"bash", "-c", script})
	if err != nil {
		return fmt.Errorf("pruning cache in %s: %w", containerName, err)
	}
	return nil
}

func cacheSetupScript(symlinks []config.SymlinkConfig) string {
	lines := []string{
		"set -eu",
		fmt.Sprintf("mkdir -p %s", shellQuote(buildxCacheRoot)),
		fmt.Sprintf("chown runner:runner %s || true", shellQuote(buildxCacheRoot)),
	}
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

func cachePruneScript(policy domain.CachePrunePolicy) string {
	paths := policy.Paths
	if len(paths) == 0 {
		paths = []string{buildxCacheRoot}
	}
	intervalSeconds := seconds(policy.Interval, 24*time.Hour)
	intervalMinutes := minutes(policy.Interval, 24*time.Hour)
	maxAgeMinutes := minutes(policy.MaxAge, 14*24*time.Hour)
	tempMaxAgeMinutes := minutes(policy.TempMaxAge, 6*time.Hour)

	lines := []string{
		"set -eu",
		"stamp='/cache/.gh-runner-scaler-prune.stamp'",
		"lock='/cache/.gh-runner-scaler-prune.lock'",
		"now=$(date +%s)",
		"last=0",
		`if [ -f "$stamp" ]; then last=$(cat "$stamp" 2>/dev/null || echo 0); fi`,
		`case "$last" in ''|*[!0-9]*) last=0 ;; esac`,
		fmt.Sprintf(`if [ "$last" -gt 0 ] && [ $((now - last)) -lt %d ]; then exit 0; fi`, intervalSeconds),
		fmt.Sprintf(`if [ -d "$lock" ]; then find "$lock" -maxdepth 0 -type d -mmin +%d -exec rmdir {} \; 2>/dev/null || true; fi`, intervalMinutes),
		`if ! mkdir "$lock" 2>/dev/null; then exit 0; fi`,
		`trap 'rmdir "$lock" 2>/dev/null || true' EXIT`,
		"now=$(date +%s)",
		"last=0",
		`if [ -f "$stamp" ]; then last=$(cat "$stamp" 2>/dev/null || echo 0); fi`,
		`case "$last" in ''|*[!0-9]*) last=0 ;; esac`,
		fmt.Sprintf(`if [ "$last" -gt 0 ] && [ $((now - last)) -lt %d ]; then exit 0; fi`, intervalSeconds),
	}
	for _, path := range paths {
		quoted := shellQuote(path)
		lines = append(lines,
			fmt.Sprintf("if [ -d %s ]; then", quoted),
			fmt.Sprintf("  find %s -mindepth 3 -maxdepth 3 -type d -name '*-next' -mmin +%d -prune -exec rm -rf -- {} +", quoted, tempMaxAgeMinutes),
			fmt.Sprintf("  find %s -mindepth 3 -maxdepth 3 -type d -mmin +%d -prune -exec rm -rf -- {} +", quoted, maxAgeMinutes),
			fmt.Sprintf("  find %s -mindepth 1 -maxdepth 6 -type d -empty -delete", quoted),
			"fi",
		)
	}
	lines = append(lines,
		`printf '%s\n' "$now" > "$stamp"`,
		`chown runner:runner "$stamp" || true`,
	)
	return strings.Join(lines, "\n")
}

func seconds(value, fallback time.Duration) int {
	if value <= 0 {
		value = fallback
	}
	result := int(value.Seconds())
	if result < 1 {
		return 1
	}
	return result
}

func minutes(value, fallback time.Duration) int {
	if value <= 0 {
		value = fallback
	}
	result := int(value.Minutes())
	if result < 1 {
		return 1
	}
	return result
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
	envLines := []string{
		"RUNNER_BUILDX_CACHE_ROOT=" + buildxCacheRoot,
		"DOCKER_BUILDKIT=1",
	}
	if toolCache != "" {
		envLines = append([]string{
			"AGENT_TOOLSDIRECTORY=" + toolCache,
			"RUNNER_TOOL_CACHE=" + toolCache,
		}, envLines...)
	}
	quotedLines := make([]string, 0, len(envLines))
	for _, line := range envLines {
		quotedLines = append(quotedLines, shellQuote(line))
	}
	return strings.Join([]string{
		"set -eu",
		`env_file=/home/runner/.env`,
		`tmp_file=$(mktemp)`,
		`if [ -f "$env_file" ]; then`,
		`  grep -v -E '^(AGENT_TOOLSDIRECTORY|RUNNER_TOOL_CACHE|RUNNER_BUILDX_CACHE_ROOT|DOCKER_BUILDKIT)=' "$env_file" > "$tmp_file" || true`,
		`fi`,
		fmt.Sprintf(`printf '%%s\n' %s >> "$tmp_file"`, strings.Join(quotedLines, " ")),
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
