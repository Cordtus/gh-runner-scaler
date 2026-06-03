// Package config handles TOML configuration loading with environment variable overrides.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/Cordtus/gh-runner-scaler/internal/domain"
)

// Config is the top-level configuration structure.
type Config struct {
	Scaler        ScalerConfig           `toml:"scaler"`
	Container     ContainerConfig        `toml:"container"`
	Cache         CacheConfig            `toml:"cache"`
	CacheProfiles map[string]CacheConfig `toml:"cache_profiles"`
	CI            CIConfig               `toml:"ci"`
	RunnerClasses []RunnerClassConfig    `toml:"runner_classes"`
	Webhook       WebhookConfig          `toml:"webhook"`
	Metrics       MetricsConfig          `toml:"metrics"`
	State         StateConfig            `toml:"state"`
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

// RunnerClassConfig describes one logical runner class managed by the router.
// Empty fields inherit from the legacy top-level configuration so existing
// single-class deployments keep working unchanged.
type RunnerClassConfig struct {
	ID             string   `toml:"id"`
	Org            string   `toml:"org"`
	Prefix         string   `toml:"prefix"`
	MaxAutoRunners *int     `toml:"max_auto_runners"`
	IdleTimeout    Duration `toml:"idle_timeout"`
	Labels         string   `toml:"labels"`
	MatchLabels    []string `toml:"match_labels"`
	RunnerWorkDir  string   `toml:"runner_work_dir"`
	Template       string   `toml:"template"`
	CacheProfile   string   `toml:"cache_profile"`
}

// RunnerClass is a fully resolved runner class used by the composition root.
type RunnerClass struct {
	ID             string
	Org            string
	Prefix         string
	MaxAutoRunners int
	IdleTimeout    time.Duration
	Labels         string
	MatchLabels    []string
	RunnerWorkDir  string
	Template       string
	Cache          CacheConfig
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
	Enabled  bool             `toml:"enabled"`
	Pool     string           `toml:"pool"`
	Volume   string           `toml:"volume"`
	Prune    CachePruneConfig `toml:"prune"`
	Symlinks []SymlinkConfig  `toml:"symlinks"`
}

// CachePruneConfig controls cleanup of bounded shared-cache paths.
type CachePruneConfig struct {
	Enabled    bool     `toml:"enabled"`
	Interval   Duration `toml:"interval"`
	MaxAge     Duration `toml:"max_age"`
	TempMaxAge Duration `toml:"temp_max_age"`
	Paths      []string `toml:"paths"`
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
	LogsToken string            `toml:"-"`
}

// MetricsConfig controls the metrics collection and push.
type MetricsConfig struct {
	Enabled               bool       `toml:"enabled"`
	Interval              Duration   `toml:"interval"`
	CollectWorkflows      bool       `toml:"collect_workflows"`
	WorkflowRepoBatchSize int        `toml:"workflow_repo_batch_size"`
	CollectHost           bool       `toml:"collect_host"`
	Loki                  LokiConfig `toml:"loki"`
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
		Cache: CacheConfig{
			Prune: CachePruneConfig{
				Enabled:    true,
				Interval:   Duration{24 * time.Hour},
				MaxAge:     Duration{14 * 24 * time.Hour},
				TempMaxAge: Duration{6 * time.Hour},
				Paths:      []string{"/cache/buildx"},
			},
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
			Enabled:               true,
			Interval:              Duration{60 * time.Second},
			CollectWorkflows:      true,
			WorkflowRepoBatchSize: 25,
			CollectHost:           true,
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
	if v := os.Getenv("GH_SCALER_LOG_TOKEN"); v != "" {
		cfg.Webhook.LogsToken = v
	} else if cfg.Webhook.LogsToken == "" {
		cfg.Webhook.LogsToken = cfg.CI.GitHub.WebhookSecret
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
	if len(cfg.RunnerClasses) == 0 {
		if cfg.Scaler.Prefix == "" {
			return fmt.Errorf("scaler.prefix is required")
		}
		if cfg.Scaler.MaxAutoRunners < 0 {
			return fmt.Errorf("scaler.max_auto_runners must be >= 0")
		}
		if cfg.Scaler.IdleTimeout.Duration <= 0 {
			return fmt.Errorf("scaler.idle_timeout must be > 0")
		}
	}
	if cfg.Scaler.PollInterval.Duration <= 0 {
		return fmt.Errorf("scaler.poll_interval must be > 0")
	}
	if (cfg.Container.LXD.RemoteCert == "") != (cfg.Container.LXD.RemoteKey == "") {
		return fmt.Errorf("container.lxd.remote_cert and remote_key must be set together")
	}
	if err := validateCache("cache", cfg.Cache); err != nil {
		return err
	}
	for name, profile := range cfg.CacheProfiles {
		if err := validateCache("cache_profiles."+name, profile); err != nil {
			return err
		}
	}
	classes, err := cfg.RunnerClassConfigs()
	if err != nil {
		return err
	}
	seenIDs := make(map[string]struct{}, len(classes))
	seenPrefixes := make(map[string]string, len(classes))
	for _, class := range classes {
		if _, exists := seenIDs[class.ID]; exists {
			return fmt.Errorf("runner class id must be unique: %s", class.ID)
		}
		seenIDs[class.ID] = struct{}{}
		if previous, exists := seenPrefixes[class.Prefix]; exists {
			return fmt.Errorf("runner class prefix must be unique: %s used by %s and %s", class.Prefix, previous, class.ID)
		}
		seenPrefixes[class.Prefix] = class.ID
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
	if cfg.Metrics.WorkflowRepoBatchSize < 0 {
		return fmt.Errorf("metrics.workflow_repo_batch_size must be >= 0")
	}
	if cfg.State.Filesystem.Dir == "" {
		return fmt.Errorf("state.filesystem.dir is required")
	}
	return nil
}

func validateCache(name string, cache CacheConfig) error {
	if !cache.Enabled {
		return nil
	}
	if cache.Pool == "" {
		return fmt.Errorf("%s.pool is required when cache is enabled", name)
	}
	if cache.Volume == "" {
		return fmt.Errorf("%s.volume is required when cache is enabled", name)
	}
	for _, sl := range cache.Symlinks {
		if !filepath.IsAbs(sl.Source) {
			return fmt.Errorf("%s.symlinks source must be absolute: %s", name, sl.Source)
		}
		if !filepath.IsAbs(sl.Target) {
			return fmt.Errorf("%s.symlinks target must be absolute: %s", name, sl.Target)
		}
	}
	if cache.Prune.Enabled {
		if cache.Prune.Interval.Duration <= 0 {
			return fmt.Errorf("%s.prune.interval must be > 0", name)
		}
		if cache.Prune.MaxAge.Duration <= 0 {
			return fmt.Errorf("%s.prune.max_age must be > 0", name)
		}
		if cache.Prune.TempMaxAge.Duration <= 0 {
			return fmt.Errorf("%s.prune.temp_max_age must be > 0", name)
		}
		for _, path := range cache.Prune.Paths {
			if !filepath.IsAbs(path) {
				return fmt.Errorf("%s.prune.paths entries must be absolute: %s", name, path)
			}
			if filepath.Clean(path) != path {
				return fmt.Errorf("%s.prune.paths entries must be clean paths: %s", name, path)
			}
			if !strings.HasPrefix(path, "/cache/") {
				return fmt.Errorf("%s.prune.paths entries must be specific subdirectories under /cache: %s", name, path)
			}
		}
	}
	return nil
}

// CachePrunePolicy returns the provider-neutral shared-cache cleanup settings.
func (cfg *Config) CachePrunePolicy() domain.CachePrunePolicy {
	return cachePrunePolicy(cfg.Cache)
}

// RunnerClassConfigs returns the resolved runner classes. With no
// [[runner_classes]] entries, it synthesizes one legacy-compatible class.
func (cfg *Config) RunnerClassConfigs() ([]RunnerClass, error) {
	if len(cfg.RunnerClasses) == 0 {
		if cfg.Container.Template == "" {
			return nil, fmt.Errorf("container.template is required")
		}
		return []RunnerClass{{
			ID:             "default",
			Org:            cfg.CI.Org,
			Prefix:         cfg.Scaler.Prefix,
			MaxAutoRunners: cfg.Scaler.MaxAutoRunners,
			IdleTimeout:    cfg.Scaler.IdleTimeout.Duration,
			Labels:         cfg.Scaler.Labels,
			MatchLabels:    labelsList(cfg.Scaler.Labels),
			RunnerWorkDir:  cfg.Scaler.RunnerWorkDir,
			Template:       cfg.Container.Template,
			Cache:          cfg.Cache,
		}}, nil
	}

	classes := make([]RunnerClass, 0, len(cfg.RunnerClasses))
	for _, raw := range cfg.RunnerClasses {
		class := RunnerClass{
			ID:             strings.TrimSpace(raw.ID),
			Org:            firstNonEmpty(raw.Org, cfg.CI.Org),
			Prefix:         firstNonEmpty(raw.Prefix, cfg.Scaler.Prefix),
			MaxAutoRunners: cfg.Scaler.MaxAutoRunners,
			IdleTimeout:    cfg.Scaler.IdleTimeout.Duration,
			Labels:         firstNonEmpty(raw.Labels, cfg.Scaler.Labels),
			MatchLabels:    cleanLabels(raw.MatchLabels),
			RunnerWorkDir:  firstNonEmpty(raw.RunnerWorkDir, cfg.Scaler.RunnerWorkDir),
			Template:       firstNonEmpty(raw.Template, cfg.Container.Template),
			Cache:          cfg.Cache,
		}
		if raw.MaxAutoRunners != nil {
			class.MaxAutoRunners = *raw.MaxAutoRunners
		}
		if raw.IdleTimeout.Duration > 0 {
			class.IdleTimeout = raw.IdleTimeout.Duration
		}
		if raw.CacheProfile != "" {
			profile, ok := cfg.CacheProfiles[raw.CacheProfile]
			if !ok {
				return nil, fmt.Errorf("runner class %s references unknown cache profile %q", class.ID, raw.CacheProfile)
			}
			class.Cache = profile
		}
		if len(class.MatchLabels) == 0 {
			class.MatchLabels = labelsList(class.Labels)
		}
		if class.ID == "" {
			return nil, fmt.Errorf("runner class id is required")
		}
		if class.Org == "" {
			return nil, fmt.Errorf("runner class %s org is required", class.ID)
		}
		if class.Prefix == "" {
			return nil, fmt.Errorf("runner class %s prefix is required", class.ID)
		}
		if class.MaxAutoRunners < 0 {
			return nil, fmt.Errorf("runner class %s max_auto_runners must be >= 0", class.ID)
		}
		if class.IdleTimeout <= 0 {
			return nil, fmt.Errorf("runner class %s idle_timeout must be > 0", class.ID)
		}
		if class.Template == "" {
			return nil, fmt.Errorf("runner class %s template is required", class.ID)
		}
		classes = append(classes, class)
	}
	return classes, nil
}

// CachePrunePolicyFor returns the provider-neutral cleanup settings for a resolved class.
func CachePrunePolicyFor(cache CacheConfig) domain.CachePrunePolicy {
	return cachePrunePolicy(cache)
}

func cachePrunePolicy(cache CacheConfig) domain.CachePrunePolicy {
	return domain.CachePrunePolicy{
		Enabled:    cache.Enabled && cache.Prune.Enabled,
		Interval:   cache.Prune.Interval.Duration,
		MaxAge:     cache.Prune.MaxAge.Duration,
		TempMaxAge: cache.Prune.TempMaxAge.Duration,
		Paths:      append([]string(nil), cache.Prune.Paths...),
	}
}

func labelsList(labels string) []string {
	parts := strings.Split(labels, ",")
	return cleanLabels(parts)
}

func cleanLabels(labels []string) []string {
	result := make([]string, 0, len(labels))
	for _, label := range labels {
		label = strings.TrimSpace(label)
		if label != "" {
			result = append(result, label)
		}
	}
	return result
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
