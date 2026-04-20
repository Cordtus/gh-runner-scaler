// Package config handles TOML configuration loading with environment variable overrides.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the top-level configuration structure.
type Config struct {
	Scaler    ScalerConfig    `toml:"scaler"`
	Container ContainerConfig `toml:"container"`
	Cache     CacheConfig     `toml:"cache"`
	CI        CIConfig        `toml:"ci"`
	Webhook   WebhookConfig   `toml:"webhook"`
	Metrics   MetricsConfig   `toml:"metrics"`
	State     StateConfig     `toml:"state"`
}

// ScalerConfig controls the core scaling behavior.
type ScalerConfig struct {
	Prefix         string   `toml:"prefix"`
	MaxAutoRunners int      `toml:"max_auto_runners"`
	IdleTimeout    Duration `toml:"idle_timeout"`
	PollInterval   Duration `toml:"poll_interval"`
	Labels         string   `toml:"labels"`
	RunnerWorkDir  string   `toml:"runner_work_dir"`
}

// ContainerConfig selects and configures the container runtime provider.
type ContainerConfig struct {
	Provider string    `toml:"provider"`
	Template string    `toml:"template"`
	LXD      LXDConfig `toml:"lxd"`
}

// LXDConfig holds LXD-specific connection settings.
type LXDConfig struct {
	Socket     string `toml:"socket"`
	Remote     string `toml:"remote"`
	RemoteURL  string `toml:"remote_url"`
	RemoteCert string `toml:"remote_cert"`
	RemoteKey  string `toml:"remote_key"`
}

// CacheConfig controls the persistent cache volume.
type CacheConfig struct {
	Enabled  bool            `toml:"enabled"`
	Pool     string          `toml:"pool"`
	Volume   string          `toml:"volume"`
	Symlinks []SymlinkConfig `toml:"symlinks"`
}

// SymlinkConfig maps a cache volume path to a target path inside the container.
type SymlinkConfig struct {
	Source string `toml:"source"`
	Target string `toml:"target"`
}

// CIConfig selects and configures the CI provider.
type CIConfig struct {
	Provider string       `toml:"provider"`
	Org      string       `toml:"org"`
	GitHub   GitHubConfig `toml:"github"`
}

// GitHubConfig holds GitHub-specific settings.
// Token and webhook secret come from environment variables.
type GitHubConfig struct {
	Token         string `toml:"-"` // from GH_SCALER_GITHUB_TOKEN env
	WebhookSecret string `toml:"-"` // from GH_WEBHOOK_SECRET env
}

// WebhookConfig controls the webhook HTTP listener.
type WebhookConfig struct {
	Enabled   bool              `toml:"enabled"`
	Port      int               `toml:"port"`
	Debounce  Duration          `toml:"debounce"`
	SyncRepos map[string]string `toml:"sync_repos"`
}

// MetricsConfig controls the metrics collection and push.
type MetricsConfig struct {
	Enabled          bool       `toml:"enabled"`
	Interval         Duration   `toml:"interval"`
	CollectWorkflows bool       `toml:"collect_workflows"`
	CollectHost      bool       `toml:"collect_host"`
	Loki             LokiConfig `toml:"loki"`
}

// LokiConfig holds Grafana Loki connection settings.
// All values come from environment variables.
type LokiConfig struct {
	PushURL  string `toml:"-"` // from LOKI_PUSH_URL env
	Username string `toml:"-"` // from LOKI_USERNAME env
	APIKey   string `toml:"-"` // from GRAFANA_CLOUD_API_KEY env
}

// StateConfig selects and configures the state store provider.
type StateConfig struct {
	Provider   string          `toml:"provider"`
	Filesystem FilesystemState `toml:"filesystem"`
}

// FilesystemState holds filesystem state store settings.
type FilesystemState struct {
	Dir string `toml:"dir"`
}

// Duration wraps time.Duration for TOML unmarshaling of human-readable strings.
type Duration struct {
	time.Duration
}

// UnmarshalText parses a duration string like "30s" or "5m".
func (d *Duration) UnmarshalText(text []byte) error {
	var err error
	d.Duration, err = time.ParseDuration(string(text))
	return err
}

// MarshalText formats the duration for TOML output.
func (d Duration) MarshalText() ([]byte, error) {
	return []byte(d.Duration.String()), nil
}

// Load reads a TOML config file and applies environment variable overrides.
func Load(path string) (*Config, error) {
	cfg := defaults()

	if _, err := toml.DecodeFile(path, cfg); err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	applyEnvOverrides(cfg)

	if err := validate(cfg); err != nil {
		return nil, fmt.Errorf("config validation: %w", err)
	}

	return cfg, nil
}

// defaults returns a Config populated with sensible defaults.
func defaults() *Config {
	return &Config{
		Scaler: ScalerConfig{
			Prefix:         "gh-runner-auto",
			MaxAutoRunners: 6,
			IdleTimeout:    Duration{300 * time.Second},
			PollInterval:   Duration{30 * time.Second},
			Labels:         "self-hosted,linux,x64",
			RunnerWorkDir:  "_work",
		},
		Container: ContainerConfig{
			Provider: "lxd",
			Template: "gh-runner-template",
		},
		CI: CIConfig{
			Provider: "github",
		},
		Webhook: WebhookConfig{
			Enabled:  true,
			Port:     9876,
			Debounce: Duration{2 * time.Second},
		},
		Metrics: MetricsConfig{
			Enabled:          true,
			Interval:         Duration{60 * time.Second},
			CollectWorkflows: true,
			CollectHost:      true,
		},
		State: StateConfig{
			Provider:   "filesystem",
			Filesystem: FilesystemState{Dir: ".state"},
		},
	}
}

// applyEnvOverrides reads secrets and optional overrides from environment variables.
// Env vars always take precedence over TOML values.
func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("GH_SCALER_GITHUB_TOKEN"); v != "" {
		cfg.CI.GitHub.Token = v
	}
	if v := os.Getenv("GH_WEBHOOK_SECRET"); v != "" {
		cfg.CI.GitHub.WebhookSecret = v
	}
	if v := os.Getenv("LOKI_PUSH_URL"); v != "" {
		cfg.Metrics.Loki.PushURL = v
	}
	if v := os.Getenv("LOKI_USERNAME"); v != "" {
		cfg.Metrics.Loki.Username = v
	}
	if v := os.Getenv("GRAFANA_CLOUD_API_KEY"); v != "" {
		cfg.Metrics.Loki.APIKey = v
	}
}

// validate checks that required fields are present.
func validate(cfg *Config) error {
	if cfg.CI.GitHub.Token == "" && cfg.CI.Provider == "github" {
		return fmt.Errorf("GH_SCALER_GITHUB_TOKEN env var is required")
	}
	if cfg.Scaler.Prefix == "" {
		return fmt.Errorf("scaler.prefix is required")
	}
	if cfg.Scaler.MaxAutoRunners < 0 {
		return fmt.Errorf("scaler.max_auto_runners must be >= 0")
	}
	if cfg.Scaler.IdleTimeout.Duration <= 0 {
		return fmt.Errorf("scaler.idle_timeout must be > 0")
	}
	if cfg.Scaler.PollInterval.Duration <= 0 {
		return fmt.Errorf("scaler.poll_interval must be > 0")
	}
	if cfg.Container.Template == "" {
		return fmt.Errorf("container.template is required")
	}
	if (cfg.Container.LXD.RemoteCert == "") != (cfg.Container.LXD.RemoteKey == "") {
		return fmt.Errorf("container.lxd.remote_cert and remote_key must be set together")
	}
	if cfg.Cache.Enabled {
		if cfg.Cache.Pool == "" {
			return fmt.Errorf("cache.pool is required when cache is enabled")
		}
		if cfg.Cache.Volume == "" {
			return fmt.Errorf("cache.volume is required when cache is enabled")
		}
		for _, sl := range cfg.Cache.Symlinks {
			if !filepath.IsAbs(sl.Source) {
				return fmt.Errorf("cache.symlinks source must be absolute: %s", sl.Source)
			}
			if !filepath.IsAbs(sl.Target) {
				return fmt.Errorf("cache.symlinks target must be absolute: %s", sl.Target)
			}
		}
	}
	if cfg.Webhook.Enabled && cfg.CI.GitHub.WebhookSecret == "" && cfg.CI.Provider == "github" {
		return fmt.Errorf("GH_WEBHOOK_SECRET env var is required when webhook is enabled")
	}
	if cfg.Webhook.Enabled {
		if cfg.Webhook.Port <= 0 || cfg.Webhook.Port > 65535 {
			return fmt.Errorf("webhook.port must be between 1 and 65535")
		}
		if cfg.Webhook.Debounce.Duration < 0 {
			return fmt.Errorf("webhook.debounce must be >= 0")
		}
	}
	if cfg.Metrics.Enabled && cfg.Metrics.Loki.PushURL == "" {
		// Only validate Loki config if the metrics backend is loki (currently the only one)
		return fmt.Errorf("LOKI_PUSH_URL env var is required when metrics are enabled")
	}
	if cfg.Metrics.Enabled && cfg.Metrics.Loki.Username == "" {
		return fmt.Errorf("LOKI_USERNAME env var is required when metrics are enabled")
	}
	if cfg.Metrics.Enabled && cfg.Metrics.Loki.APIKey == "" {
		return fmt.Errorf("GRAFANA_CLOUD_API_KEY env var is required when metrics are enabled")
	}
	if cfg.Metrics.Enabled && cfg.Metrics.Interval.Duration <= 0 {
		return fmt.Errorf("metrics.interval must be > 0")
	}
	if cfg.State.Filesystem.Dir == "" {
		return fmt.Errorf("state.filesystem.dir is required")
	}
	return nil
}
