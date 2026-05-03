package lxd

import (
	"strings"
	"testing"

	"github.com/Cordtus/gh-runner-scaler/internal/config"
)

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
		"grep -v -E '^(AGENT_TOOLSDIRECTORY|RUNNER_TOOL_CACHE)='",
	}
	for _, snippet := range required {
		if !strings.Contains(script, snippet) {
			t.Fatalf("runner env script missing %q\nscript:\n%s", snippet, script)
		}
	}
}
