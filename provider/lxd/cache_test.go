package lxd

import (
	"strings"
	"testing"

	"github.com/Cordtus/gh-runner-scaler/internal/config"
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
