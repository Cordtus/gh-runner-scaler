package main

import (
	"testing"

	"github.com/Cordtus/gh-runner-scaler/internal/config"
	ghprovider "github.com/Cordtus/gh-runner-scaler/provider/github"
)

func TestDaemonMetricsTarget_UsesSingleTarget(t *testing.T) {
	classes := []config.RunnerClass{
		{ID: "cac-main", Org: "cac-group"},
		{ID: "cac-large", Org: "CAC-GROUP"},
	}

	if got := daemonMetricsTarget(classes); got != "cac-group" {
		t.Fatalf("daemonMetricsTarget = %q, want cac-group", got)
	}
}

func TestDaemonMetricsTarget_UsesNeutralLabelForMultipleTargets(t *testing.T) {
	classes := []config.RunnerClass{
		{ID: "cac", Org: "cac-group"},
		{ID: "personal", Repo: "Cordtus/gh-runner-scaler"},
	}

	if got := daemonMetricsTarget(classes); got != multiTargetMetricsLabel {
		t.Fatalf("daemonMetricsTarget = %q, want %q", got, multiTargetMetricsLabel)
	}
}

func TestGitHubTargetCacheKey_SeparatesOrgAndRepoScopes(t *testing.T) {
	orgClass := config.RunnerClass{Org: "Cordtus"}
	repoClass := config.RunnerClass{Repo: "Cordtus/gh-runner-scaler"}

	if got := githubTargetCacheKey(orgClass); got != "org:cordtus" {
		t.Fatalf("org cache key = %q, want org:cordtus", got)
	}
	if got := githubTargetCacheKey(repoClass); got != "repo:cordtus/gh-runner-scaler" {
		t.Fatalf("repo cache key = %q, want repo:cordtus/gh-runner-scaler", got)
	}
}

func TestWireCIProvider_UsesOrgOrRepoRegistrationURL(t *testing.T) {
	cfg := &config.Config{
		CI: config.CIConfig{
			Provider: "github",
			GitHub:   config.GitHubConfig{Token: "token"},
		},
	}
	providers := make(map[string]*ghprovider.Provider)

	orgCI, err := wireCIProvider(cfg, config.RunnerClass{Org: "cac-group", Prefix: "cac-auto"}, providers)
	if err != nil {
		t.Fatalf("wireCIProvider org target failed: %v", err)
	}
	if got := orgCI.RegistrationURL(); got != "https://github.com/cac-group" {
		t.Fatalf("org RegistrationURL = %q, want https://github.com/cac-group", got)
	}

	repoCI, err := wireCIProvider(cfg, config.RunnerClass{Repo: "Cordtus/gh-runner-scaler", Prefix: "personal-auto"}, providers)
	if err != nil {
		t.Fatalf("wireCIProvider repo target failed: %v", err)
	}
	if got := repoCI.RegistrationURL(); got != "https://github.com/Cordtus/gh-runner-scaler" {
		t.Fatalf("repo RegistrationURL = %q, want https://github.com/Cordtus/gh-runner-scaler", got)
	}

	if providers["org:cac-group"] == nil {
		t.Fatal("expected org target provider cached under org:cac-group")
	}
	if providers["repo:cordtus/gh-runner-scaler"] == nil {
		t.Fatal("expected repo target provider cached under repo:cordtus/gh-runner-scaler")
	}
}
