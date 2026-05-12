package lxd

import (
	"strings"
	"testing"
	"time"

	"github.com/Cordtus/gh-runner-scaler/internal/config"
	"github.com/Cordtus/gh-runner-scaler/internal/domain"
)

func TestCacheSetupScript_PreparesBuildxCacheRoot(t *testing.T) {
	script := cacheSetupScript(nil)

	required := []string{
		"mkdir -p '/cache/buildx'",
		"chown runner:runner '/cache/buildx' || true",
	}
	for _, snippet := range required {
		if !strings.Contains(script, snippet) {
			t.Fatalf("cache setup script missing %q\nscript:\n%s", snippet, script)
		}
	}
}

func TestCacheSetupScript_ReplacesExistingDirectoriesInsteadOfNestingSymlinks(t *testing.T) {
	script := cacheSetupScript([]config.SymlinkConfig{{
		Source: "/cache/pip",
		Target: "/home/runner/.cache/pip",
	}})

	required := []string{
		"cp -an '/home/runner/.cache/pip'/. '/cache/pip'/",
		"rm -rf '/home/runner/.cache/pip'",
		"ln -s '/cache/pip' '/home/runner/.cache/pip'",
	}
	for _, snippet := range required {
		if !strings.Contains(script, snippet) {
			t.Fatalf("cache setup script missing %q\nscript:\n%s", snippet, script)
		}
	}
}

func TestToolCacheDir_FindsHostedToolCacheTarget(t *testing.T) {
	dir := toolCacheDir([]config.SymlinkConfig{
		{Source: "/cache/pip", Target: "/home/runner/.cache/pip"},
		{Source: "/cache/tool-cache", Target: "/opt/hostedtoolcache"},
	})

	if dir != "/opt/hostedtoolcache" {
		t.Fatalf("toolCacheDir = %q, want /opt/hostedtoolcache", dir)
	}
}

func TestRunnerEnvScript_ConfiguresToolCacheEnvFile(t *testing.T) {
	script := runnerEnvScript("/opt/hostedtoolcache")

	required := []string{
		"env_file=/home/runner/.env",
		"'AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache'",
		"'RUNNER_TOOL_CACHE=/opt/hostedtoolcache'",
		"'RUNNER_BUILDX_CACHE_ROOT=/cache/buildx'",
		"'DOCKER_BUILDKIT=1'",
		"grep -v -E '^(AGENT_TOOLSDIRECTORY|RUNNER_TOOL_CACHE|RUNNER_BUILDX_CACHE_ROOT|DOCKER_BUILDKIT)='",
	}
	for _, snippet := range required {
		if !strings.Contains(script, snippet) {
			t.Fatalf("runner env script missing %q\nscript:\n%s", snippet, script)
		}
	}
}

func TestRunnerEnvScript_ConfiguresBuildxWithoutToolCache(t *testing.T) {
	script := runnerEnvScript("")

	required := []string{
		"'RUNNER_BUILDX_CACHE_ROOT=/cache/buildx'",
		"'DOCKER_BUILDKIT=1'",
	}
	for _, snippet := range required {
		if !strings.Contains(script, snippet) {
			t.Fatalf("runner env script missing %q\nscript:\n%s", snippet, script)
		}
	}
	if strings.Contains(script, "AGENT_TOOLSDIRECTORY=") || strings.Contains(script, "RUNNER_TOOL_CACHE=") {
		t.Fatalf("runner env script should not write tool-cache env without a configured tool cache\nscript:\n%s", script)
	}
}

func TestCachePruneScript_ThrottlesWithSharedStampAndLock(t *testing.T) {
	script := cachePruneScript(domain.CachePrunePolicy{
		Enabled:    true,
		Interval:   24 * time.Hour,
		MaxAge:     14 * 24 * time.Hour,
		TempMaxAge: 6 * time.Hour,
		Paths:      []string{"/cache/buildx"},
	})

	required := []string{
		"stamp='/cache/.gh-runner-scaler-prune.stamp'",
		"lock='/cache/.gh-runner-scaler-prune.lock'",
		"case \"$last\" in ''|*[!0-9]*) last=0 ;; esac",
		"if [ \"$last\" -gt 0 ] && [ $((now - last)) -lt 86400 ]; then exit 0; fi",
		"if [ -d \"$lock\" ]; then find \"$lock\" -maxdepth 0 -type d -mmin +1440 -exec rmdir {} \\; 2>/dev/null || true; fi",
		"if ! mkdir \"$lock\" 2>/dev/null; then exit 0; fi",
	}
	for _, snippet := range required {
		if !strings.Contains(script, snippet) {
			t.Fatalf("cache prune script missing %q\nscript:\n%s", snippet, script)
		}
	}
}

func TestCachePruneScript_PrunesOnlyConfiguredCacheDirectories(t *testing.T) {
	script := cachePruneScript(domain.CachePrunePolicy{
		Enabled:    true,
		Interval:   time.Hour,
		MaxAge:     14 * 24 * time.Hour,
		TempMaxAge: 6 * time.Hour,
		Paths:      []string{"/cache/buildx"},
	})

	required := []string{
		"if [ -d '/cache/buildx' ]; then",
		"find '/cache/buildx' -mindepth 3 -maxdepth 3 -type d -name '*-next' -mmin +360 -prune -exec rm -rf -- {} +",
		"find '/cache/buildx' -mindepth 3 -maxdepth 3 -type d -mmin +20160 -prune -exec rm -rf -- {} +",
		"find '/cache/buildx' -mindepth 1 -maxdepth 6 -type d -empty -delete",
	}
	for _, snippet := range required {
		if !strings.Contains(script, snippet) {
			t.Fatalf("cache prune script missing %q\nscript:\n%s", snippet, script)
		}
	}
	if strings.Contains(script, "blobs/") {
		t.Fatalf("cache prune script should not prune OCI blobs directly\nscript:\n%s", script)
	}
}

func TestCachePruneScript_DefaultsToBuildxCacheRoot(t *testing.T) {
	script := cachePruneScript(domain.CachePrunePolicy{})

	if !strings.Contains(script, "if [ -d '/cache/buildx' ]; then") {
		t.Fatalf("cache prune script should default to buildx root\nscript:\n%s", script)
	}
}

func TestCachePruneScript_QuotesConfiguredPaths(t *testing.T) {
	script := cachePruneScript(domain.CachePrunePolicy{
		Paths: []string{"/cache/build x/owner's"},
	})

	if !strings.Contains(script, "'/cache/build x/owner'\"'\"'s'") {
		t.Fatalf("cache prune script should shell-quote configured paths\nscript:\n%s", script)
	}
}
