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

func TestValidate_RemoteTLSPathsMustBePaired(t *testing.T) {
	cfg := validConfig()
	cfg.Container.LXD.RemoteCert = "/tmp/client.crt"

	err := validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "remote_cert and remote_key must be set together") {
		t.Fatalf("expected remote cert/key pairing error, got %v", err)
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
