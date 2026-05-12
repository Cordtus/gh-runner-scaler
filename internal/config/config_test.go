package config

import (
	"strings"
	"testing"
)

func TestValidate_MetricsRequiresAllCredentials(t *testing.T) {
	cfg := validConfig()
	cfg.Metrics.Enabled = true
	cfg.Metrics.Loki.PushURL = "https://logs.example.com/loki/api/v1/push"

	err := validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "LOKI_USERNAME") {
		t.Fatalf("expected missing username validation error, got %v", err)
	}

	cfg.Metrics.Loki.Username = "instance-id"
	err = validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "GRAFANA_CLOUD_API_KEY") {
		t.Fatalf("expected missing api key validation error, got %v", err)
	}

	cfg.Metrics.Loki.APIKey = "api-key"
	if err := validate(cfg); err != nil {
		t.Fatalf("expected config to validate, got %v", err)
	}
}

func TestValidate_CacheRequiresAbsolutePaths(t *testing.T) {
	cfg := validConfig()
	cfg.Cache.Enabled = true
	cfg.Cache.Pool = "fast"
	cfg.Cache.Volume = "runner-cache"
	cfg.Cache.Symlinks = []SymlinkConfig{{
		Source: "cache/npm",
		Target: "/home/runner/.npm",
	}}

	err := validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "cache.symlinks source must be absolute") {
		t.Fatalf("expected absolute-path validation error, got %v", err)
	}
}

func TestValidate_CachePruneRequiresPositiveDurations(t *testing.T) {
	cfg := validConfig()
	cfg.Cache.Enabled = true
	cfg.Cache.Pool = "fast"
	cfg.Cache.Volume = "runner-cache"
	cfg.Cache.Prune.Enabled = true
	cfg.Cache.Prune.Interval.Duration = 0

	err := validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "cache.prune.interval must be > 0") {
		t.Fatalf("expected prune interval validation error, got %v", err)
	}

	cfg.Cache.Prune.Interval.Duration = 24
	cfg.Cache.Prune.MaxAge.Duration = 0
	err = validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "cache.prune.max_age must be > 0") {
		t.Fatalf("expected prune max age validation error, got %v", err)
	}

	cfg.Cache.Prune.MaxAge.Duration = 24
	cfg.Cache.Prune.TempMaxAge.Duration = 0
	err = validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "cache.prune.temp_max_age must be > 0") {
		t.Fatalf("expected prune temp max age validation error, got %v", err)
	}
}

func TestValidate_CachePrunePathsMustBeAbsoluteAndUnderCache(t *testing.T) {
	cfg := validConfig()
	cfg.Cache.Enabled = true
	cfg.Cache.Pool = "fast"
	cfg.Cache.Volume = "runner-cache"
	cfg.Cache.Prune.Enabled = true
	cfg.Cache.Prune.Paths = []string{"cache/buildx"}

	err := validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "cache.prune.paths entries must be absolute") {
		t.Fatalf("expected absolute prune path validation error, got %v", err)
	}

	cfg.Cache.Prune.Paths = []string{"/tmp/buildx"}
	err = validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "cache.prune.paths entries must be specific subdirectories under /cache") {
		t.Fatalf("expected under-cache prune path validation error, got %v", err)
	}

	cfg.Cache.Prune.Paths = []string{"/cache"}
	err = validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "cache.prune.paths entries must be specific subdirectories under /cache") {
		t.Fatalf("expected specific prune path validation error, got %v", err)
	}

	cfg.Cache.Prune.Paths = []string{"/cache/../home"}
	err = validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "cache.prune.paths entries must be clean paths") {
		t.Fatalf("expected clean prune path validation error, got %v", err)
	}
}

func TestCachePrunePolicy_MatchesConfigAndCopiesPaths(t *testing.T) {
	cfg := validConfig()
	cfg.Cache.Enabled = true
	cfg.Cache.Pool = "fast"
	cfg.Cache.Volume = "runner-cache"
	cfg.Cache.Prune.Paths = []string{"/cache/buildx"}

	policy := cfg.CachePrunePolicy()
	if !policy.Enabled {
		t.Fatal("expected cache prune policy to be enabled")
	}
	if policy.Paths[0] != "/cache/buildx" {
		t.Fatalf("unexpected prune paths: %v", policy.Paths)
	}
	policy.Paths[0] = "/cache/other"
	if cfg.Cache.Prune.Paths[0] != "/cache/buildx" {
		t.Fatalf("CachePrunePolicy should copy paths, config got %v", cfg.Cache.Prune.Paths)
	}

	cfg.Cache.Enabled = false
	if cfg.CachePrunePolicy().Enabled {
		t.Fatal("cache prune policy should be disabled when cache is disabled")
	}
}

func TestValidate_RemoteTLSPathsMustBePaired(t *testing.T) {
	cfg := validConfig()
	cfg.Container.LXD.RemoteCert = "/tmp/client.crt"

	err := validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "remote_cert and remote_key must be set together") {
		t.Fatalf("expected remote cert/key pairing error, got %v", err)
	}
}

func TestValidate_WorkflowRepoBatchSizeMustBeNonNegative(t *testing.T) {
	cfg := validConfig()
	cfg.Metrics.WorkflowRepoBatchSize = -1

	err := validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "metrics.workflow_repo_batch_size must be >= 0") {
		t.Fatalf("expected workflow repo batch size validation error, got %v", err)
	}
}

func TestApplyEnvOverrides_UsesDedicatedLogsTokenWhenPresent(t *testing.T) {
	t.Setenv("GH_WEBHOOK_SECRET", "webhook-secret")
	t.Setenv("GH_SCALER_LOG_TOKEN", "logs-token")

	cfg := defaults()
	applyEnvOverrides(cfg)

	if cfg.Webhook.LogsToken != "logs-token" {
		t.Fatalf("expected dedicated logs token, got %q", cfg.Webhook.LogsToken)
	}
}

func TestApplyEnvOverrides_FallsBackToWebhookSecretForLogsToken(t *testing.T) {
	t.Setenv("GH_WEBHOOK_SECRET", "webhook-secret")

	cfg := defaults()
	applyEnvOverrides(cfg)

	if cfg.Webhook.LogsToken != "webhook-secret" {
		t.Fatalf("expected webhook secret fallback, got %q", cfg.Webhook.LogsToken)
	}
}

func validConfig() *Config {
	cfg := defaults()
	cfg.CI.Org = "test-org"
	cfg.CI.GitHub.Token = "token"
	cfg.Webhook.Enabled = false
	cfg.Metrics.Enabled = false
	return cfg
}
