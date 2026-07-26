package config

import (
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestValidate_MetricsRequiresPushURLAndOptionalAuthPair(t *testing.T) {
	cfg := validConfig()
	cfg.Metrics.Enabled = true

	err := validate(cfg)
	if err == nil {
		t.Fatalf("expected missing push URL validation error, got %v", err)
	}

	cfg.Metrics.Loki.PushURL = "https://logs.example.com/loki/api/v1/push"
	if err := validate(cfg); err != nil {
		t.Fatalf("expected unauthenticated Loki config to validate, got %v", err)
	}

	cfg.Metrics.Loki.Username = "instance-id"
	err = validate(cfg)
	if err == nil {
		t.Fatalf("expected missing password validation error, got %v", err)
	}

	cfg.Metrics.Loki.APIKey = "api-key"
	if err := validate(cfg); err != nil {
		t.Fatalf("expected config to validate, got %v", err)
	}
}

func TestValidate_RunnerObservabilityRequiresEndpointsAndRejectsCredentials(t *testing.T) {
	cfg := validConfig()
	cfg.RunnerObservability.Enabled = true

	err := validate(cfg)
	if err == nil {
		t.Fatalf("validate error = %v, want missing push URL", err)
	}

	cfg.RunnerObservability.PushURL = "http://loki.internal/loki/api/v1/push"
	err = validate(cfg)
	if err == nil {
		t.Fatalf("validate error = %v, want missing health URL", err)
	}

	cfg.RunnerObservability.HealthURL = "http://loki.internal/ready"
	cfg.RunnerObservability.CredentialConfigured = true
	err = validate(cfg)
	if err == nil {
		t.Fatalf("validate error = %v, want credential rejection", err)
	}

	cfg.RunnerObservability.CredentialConfigured = false
	if err := validate(cfg); err != nil {
		t.Fatalf("validate configured runner observability: %v", err)
	}
}

func TestValidate_AllowsOneBaselineAndRejectsTwo(t *testing.T) {
	cfg := validConfig()
	cfg.RunnerClasses = []RunnerClassConfig{
		{ID: "node", Repo: "Cordtus/poolbet", Prefix: "node-auto", Baseline: true, BaselineName: "gh-runner-primary"},
		{ID: "browser", Repo: "Cordtus/poolbet", Prefix: "browser-auto"},
	}
	if err := validate(cfg); err != nil {
		t.Fatalf("validate one baseline: %v", err)
	}

	cfg.RunnerClasses[1].Baseline = true
	cfg.RunnerClasses[1].BaselineName = "browser-primary"
	err := validate(cfg)
	if err == nil {
		t.Fatalf("validate two baselines error = %v", err)
	}
}

func TestValidate_RejectsBaselineInsideManagedPrefix(t *testing.T) {
	cfg := validConfig()
	cfg.RunnerClasses = []RunnerClassConfig{{
		ID: "node", Repo: "Cordtus/poolbet", Prefix: "gh-runner-node",
		Baseline: true, BaselineName: "gh-runner-node-primary",
	}}
	err := validate(cfg)
	if err == nil {
		t.Fatalf("validate baseline name error = %v", err)
	}
}

func TestValidate_RejectsInvalidDemandAndDistributionPolicy(t *testing.T) {
	cfg := validConfig()
	cfg.Scaler.QueueAuditInterval.Duration = 0
	if err := validate(cfg); err == nil {
		t.Fatalf("queue audit validation error = %v", err)
	}

	cfg.Scaler.QueueAuditInterval.Duration = 5 * time.Minute
	cfg.RunnerDistribution.Enabled = true
	cfg.RunnerDistribution.CacheDir = "relative"
	if err := validate(cfg); err == nil {
		t.Fatalf("distribution path validation error = %v", err)
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
	if err == nil {
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
	if err == nil {
		t.Fatalf("expected prune interval validation error, got %v", err)
	}

	cfg.Cache.Prune.Interval.Duration = 24
	cfg.Cache.Prune.MaxAge.Duration = 0
	err = validate(cfg)
	if err == nil {
		t.Fatalf("expected prune max age validation error, got %v", err)
	}

	cfg.Cache.Prune.MaxAge.Duration = 24
	cfg.Cache.Prune.TempMaxAge.Duration = 0
	err = validate(cfg)
	if err == nil {
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
	if err == nil {
		t.Fatalf("expected absolute prune path validation error, got %v", err)
	}

	cfg.Cache.Prune.Paths = []string{"/tmp/buildx"}
	err = validate(cfg)
	if err == nil {
		t.Fatalf("expected under-cache prune path validation error, got %v", err)
	}

	cfg.Cache.Prune.Paths = []string{"/cache"}
	err = validate(cfg)
	if err == nil {
		t.Fatalf("expected specific prune path validation error, got %v", err)
	}

	cfg.Cache.Prune.Paths = []string{"/cache/../home"}
	err = validate(cfg)
	if err == nil {
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

func TestRunnerClassConfigs_SynthesizesLegacyDefault(t *testing.T) {
	cfg := validConfig()
	cfg.Scaler.Prefix = "legacy-auto"
	cfg.Scaler.Labels = "self-hosted, linux, x64"
	cfg.Container.Template = "legacy-template"

	classes, err := cfg.RunnerClassConfigs()
	if err != nil {
		t.Fatalf("RunnerClassConfigs failed: %v", err)
	}
	if len(classes) != 1 {
		t.Fatalf("expected one synthesized class, got %d", len(classes))
	}
	class := classes[0]
	if class.ID != "default" || class.Prefix != "legacy-auto" || class.Template != "legacy-template" {
		t.Fatalf("unexpected synthesized class: %+v", class)
	}
	if strings.Join(class.MatchLabels, ",") != "self-hosted,linux,x64" {
		t.Fatalf("unexpected match labels: %v", class.MatchLabels)
	}
}

func TestRunnerClassConfigs_ResolvesClassOverridesAndCacheProfile(t *testing.T) {
	max := 4
	cfg := validConfig()
	cfg.CacheProfiles = map[string]CacheConfig{
		"rust": {
			Enabled: true,
			Pool:    "fast",
			Volume:  "rust-cache",
			Prune:   cfg.Cache.Prune,
			Symlinks: []SymlinkConfig{{
				Source: "/cache/cargo",
				Target: "/home/runner/.cargo",
			}},
		},
	}
	cfg.RunnerClasses = []RunnerClassConfig{{
		ID:             "rust",
		Org:            "OtherOrg",
		Prefix:         "rust-auto",
		MaxAutoRunners: &max,
		Labels:         "self-hosted,linux,x64,rust",
		MatchLabels:    []string{"self-hosted", "rust"},
		Template:       "runner-template-rust",
		CacheProfile:   "rust",
	}}

	classes, err := cfg.RunnerClassConfigs()
	if err != nil {
		t.Fatalf("RunnerClassConfigs failed: %v", err)
	}
	class := classes[0]
	if class.Org != "OtherOrg" || class.MaxAutoRunners != 4 || class.Template != "runner-template-rust" {
		t.Fatalf("unexpected class resolution: %+v", class)
	}
	if !class.Cache.Enabled || class.Cache.Volume != "rust-cache" {
		t.Fatalf("expected rust cache profile, got %+v", class.Cache)
	}
}

func TestRunnerClassConfigs_SkipsDisabledClasses(t *testing.T) {
	disabled := false
	cfg := validConfig()
	cfg.RunnerClasses = []RunnerClassConfig{
		{
			Enabled: &disabled,
			ID:      "old-customer-disabled",
			Org:     "OldCustomer",
			Prefix:  "old-customer-auto",
			Labels:  "self-hosted,linux,x64,runner-class-old-customer",
		},
		{
			ID:     "cac-group",
			Org:    "cac-group",
			Prefix: "cac-auto",
			Labels: "self-hosted,linux,x64,runner-class-cac",
		},
	}

	classes, err := cfg.RunnerClassConfigs()
	if err != nil {
		t.Fatalf("RunnerClassConfigs failed: %v", err)
	}
	if len(classes) != 1 {
		t.Fatalf("expected one enabled class, got %d: %+v", len(classes), classes)
	}
	if classes[0].ID != "cac-group" || classes[0].Org != "cac-group" {
		t.Fatalf("unexpected enabled class: %+v", classes[0])
	}
}

func TestRunnerClassConfigs_ResolvesRepositoryTarget(t *testing.T) {
	cfg := validConfig()
	cfg.CI.Org = ""
	cfg.CI.Repo = "Cordtus/gh-runner-scaler"

	classes, err := cfg.RunnerClassConfigs()
	if err != nil {
		t.Fatalf("RunnerClassConfigs failed: %v", err)
	}
	class := classes[0]
	if class.Org != "" || class.Repo != "Cordtus/gh-runner-scaler" {
		t.Fatalf("expected repo target, got %+v", class)
	}
	if class.TargetName() != "Cordtus/gh-runner-scaler" || !class.RepoScoped() {
		t.Fatalf("unexpected repo target helpers: target=%q repoScoped=%v", class.TargetName(), class.RepoScoped())
	}
}

func TestRunnerClassConfigs_BlankMatchLabelsFallBackToRunnerLabels(t *testing.T) {
	cfg := validConfig()
	cfg.RunnerClasses = []RunnerClassConfig{{
		ID:          "rust",
		Prefix:      "rust-auto",
		Labels:      "self-hosted, linux, rust",
		MatchLabels: []string{"", "   "},
		Template:    "runner-template-rust",
	}}

	classes, err := cfg.RunnerClassConfigs()
	if err != nil {
		t.Fatalf("RunnerClassConfigs failed: %v", err)
	}
	if got := strings.Join(classes[0].MatchLabels, ","); got != "self-hosted,linux,rust" {
		t.Fatalf("match labels = %q, want self-hosted,linux,rust", got)
	}
}

func TestValidate_RunnerClassPrefixesMustBeUnique(t *testing.T) {
	cfg := validConfig()
	cfg.RunnerClasses = []RunnerClassConfig{
		{ID: "one", Prefix: "auto-one", Template: "template-one"},
		{ID: "two", Prefix: "auto-one", Template: "template-two"},
	}

	err := validate(cfg)
	if err == nil {
		t.Fatalf("expected duplicate prefix validation error, got %v", err)
	}
}

func TestValidate_CITargetMustNotBeAmbiguous(t *testing.T) {
	cfg := validConfig()
	cfg.CI.Org = "cac-group"
	cfg.CI.Repo = "Cordtus/gh-runner-scaler"

	err := validate(cfg)
	if err == nil {
		t.Fatalf("expected ambiguous target validation error, got %v", err)
	}
}

func TestValidate_RunnerClassTargetMustNotBeAmbiguous(t *testing.T) {
	cfg := validConfig()
	cfg.RunnerClasses = []RunnerClassConfig{{
		ID:     "personal",
		Org:    "cac-group",
		Repo:   "Cordtus/gh-runner-scaler",
		Prefix: "personal-auto",
	}}

	err := validate(cfg)
	if err == nil {
		t.Fatalf("expected ambiguous runner class target validation error, got %v", err)
	}
}

func TestValidate_RepositoryTargetsMustUseOwnerNameForm(t *testing.T) {
	cfg := validConfig()
	cfg.CI.Org = ""
	cfg.CI.Repo = "Cordtus"

	err := validate(cfg)
	if err == nil {
		t.Fatalf("expected repo full-name validation error, got %v", err)
	}

	cfg = validConfig()
	cfg.CI.Org = ""
	cfg.CI.Repo = "Cordtus /gh-runner-scaler"

	err = validate(cfg)
	if err == nil {
		t.Fatalf("expected repo whitespace validation error, got %v", err)
	}

	cfg = validConfig()
	cfg.CI.Org = ""
	cfg.CI.Repo = "Cordtus/ gh-runner-scaler"

	err = validate(cfg)
	if err == nil {
		t.Fatalf("expected repo whitespace validation error, got %v", err)
	}

	cfg = validConfig()
	cfg.RunnerClasses = []RunnerClassConfig{{
		ID:     "personal",
		Repo:   "Cordtus/nested/repo",
		Prefix: "personal-auto",
	}}

	err = validate(cfg)
	if err == nil {
		t.Fatalf("expected runner class repo full-name validation error, got %v", err)
	}
}

func TestValidate_RunnerClassRequiresKnownCacheProfile(t *testing.T) {
	cfg := validConfig()
	cfg.RunnerClasses = []RunnerClassConfig{{
		ID:           "rust",
		Prefix:       "rust-auto",
		Template:     "runner-template-rust",
		CacheProfile: "missing",
	}}

	err := validate(cfg)
	if err == nil {
		t.Fatalf("expected unknown cache profile validation error, got %v", err)
	}
}

func TestValidate_RemoteTLSPathsMustBePaired(t *testing.T) {
	cfg := validConfig()
	cfg.Container.LXD.RemoteCert = "/tmp/client.crt"

	err := validate(cfg)
	if err == nil {
		t.Fatalf("expected remote cert/key pairing error, got %v", err)
	}
}

func TestValidate_WorkflowRepoBatchSizeMustBeNonNegative(t *testing.T) {
	cfg := validConfig()
	cfg.Metrics.WorkflowRepoBatchSize = -1

	err := validate(cfg)
	if err == nil {
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

func TestApplyEnvOverrides_UsesGenericLokiCredentialBeforeGrafanaCloudFallback(t *testing.T) {
	t.Setenv("LOKI_API_KEY", "loki-key")
	t.Setenv("GRAFANA_CLOUD_API_KEY", "grafana-key")

	cfg := defaults()
	applyEnvOverrides(cfg)

	if cfg.Metrics.Loki.APIKey != "loki-key" {
		t.Fatalf("expected generic Loki API key, got %q", cfg.Metrics.Loki.APIKey)
	}
}

func TestApplyEnvOverrides_KeepsGrafanaCloudCredentialFallback(t *testing.T) {
	t.Setenv("GRAFANA_CLOUD_API_KEY", "grafana-key")

	cfg := defaults()
	applyEnvOverrides(cfg)

	if cfg.Metrics.Loki.APIKey != "grafana-key" {
		t.Fatalf("expected Grafana Cloud fallback API key, got %q", cfg.Metrics.Loki.APIKey)
	}
}

func TestLoad_ConfigExampleIsGenericStarter(t *testing.T) {
	t.Setenv("GH_SCALER_GITHUB_TOKEN", "token")
	t.Setenv("GH_WEBHOOK_SECRET", "webhook-secret")

	cfg, err := Load("../../config.example.toml")
	if err == nil {
		t.Fatalf("expected config.example.toml to require a deployer target, got cfg=%+v err=%v", cfg, err)
	}

	raw, err := os.ReadFile("../../config.example.toml")
	if err != nil {
		t.Fatalf("ReadFile config.example.toml failed: %v", err)
	}
	configured := strings.Replace(string(raw), `org = ""`, `org = "ExampleOrg"`, 1)
	path := t.TempDir() + "/config.toml"
	if err := os.WriteFile(path, []byte(configured), 0o644); err != nil {
		t.Fatalf("WriteFile configured example failed: %v", err)
	}

	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("Load configured config.example.toml failed: %v", err)
	}
	if cfg.Metrics.Enabled {
		t.Fatal("generic config.example.toml should keep metrics disabled until Loki is configured")
	}
	if cfg.State.Filesystem.Dir != "/var/lib/gh-runner-scaler/state" {
		t.Fatalf("expected starter state dir to match systemd install path, got %q", cfg.State.Filesystem.Dir)
	}
	classes, err := cfg.RunnerClassConfigs()
	if err != nil {
		t.Fatalf("RunnerClassConfigs failed: %v", err)
	}
	if len(classes) != 1 {
		t.Fatalf("expected one enabled runner class, got %d: %+v", len(classes), classes)
	}
	class := classes[0]
	if class.ID != "default" || class.Org != "ExampleOrg" || class.RepoScoped() {
		t.Fatalf("expected generic default org class, got %+v", class)
	}
	if !hasLabel(class.Labels, "runner-class-default") {
		t.Fatalf("expected starter labels to include runner-class-default, got %q", class.Labels)
	}
}

func TestLoad_Nodev2ConfigIncludesRequiredRunnerTargets(t *testing.T) {
	t.Setenv("GH_SCALER_GITHUB_TOKEN", "token")
	t.Setenv("GH_WEBHOOK_SECRET", "webhook-secret")
	t.Setenv("LOKI_PUSH_URL", "https://logs.example/loki/api/v1/push")
	t.Setenv("LOKI_USERNAME", "")
	t.Setenv("LOKI_API_KEY", "")
	t.Setenv("LOKI_PASSWORD", "")
	t.Setenv("GRAFANA_CLOUD_API_KEY", "")
	t.Setenv("RUNNER_LOG_LOKI_PUSH_URL", "http://192.168.0.157:3100/loki/api/v1/push")
	t.Setenv("RUNNER_LOG_LOKI_HEALTH_URL", "http://192.168.0.157:3100/ready")
	t.Setenv("RUNNER_LOG_LOKI_USERNAME", "")
	t.Setenv("RUNNER_LOG_LOKI_PASSWORD", "")
	t.Setenv("RUNNER_LOG_LOKI_API_KEY", "")

	cfg, err := Load("../../deploy/nodev2.config.toml")
	if err != nil {
		t.Fatalf("Load nodev2 config failed: %v", err)
	}
	classes, err := cfg.RunnerClassConfigs()
	if err != nil {
		t.Fatalf("RunnerClassConfigs failed: %v", err)
	}
	byID := make(map[string]RunnerClass, len(classes))
	for _, class := range classes {
		if _, exists := byID[class.ID]; exists {
			t.Fatalf("duplicate nodev2 runner class ID %q", class.ID)
		}
		byID[class.ID] = class
	}

	expected := map[string]struct {
		org        string
		repo       string
		repoScoped bool
	}{
		"cac-group": {
			org: "CAC-Group",
		},
		"gh-runner-scaler": {
			repo:       "Cordtus/gh-runner-scaler",
			repoScoped: true,
		},
		"the-clearooor": {
			repo:       "Cordtus/the-clearooor",
			repoScoped: true,
		},
		"qmkui": {
			repo:       "Cordtus/qmkui",
			repoScoped: true,
		},
		"node": {
			repo:       "Cordtus/poolbet",
			repoScoped: true,
		},
		"node-foundry": {
			repo:       "Cordtus/poolbet",
			repoScoped: true,
		},
		"node-browser": {
			repo:       "Cordtus/poolbet",
			repoScoped: true,
		},
	}

	for id, want := range expected {
		class, ok := byID[id]
		if !ok {
			t.Fatalf("missing required nodev2 runner class %q; got IDs %v", id, sortedClassIDs(byID))
		}
		if class.Org != want.org || class.Repo != want.repo || class.RepoScoped() != want.repoScoped {
			t.Fatalf("nodev2 runner class %q = org %q repo %q repoScoped %v; want org %q repo %q repoScoped %v", id, class.Org, class.Repo, class.RepoScoped(), want.org, want.repo, want.repoScoped)
		}
	}
	qmkui := byID["qmkui"]
	if qmkui.Repo != "Cordtus/qmkui" || !qmkui.RepoScoped() {
		t.Fatalf("expected qmkui repo class, got %+v", qmkui)
	}
	if qmkui.Prefix != "gh-runner-qmkui" || qmkui.MaxAutoRunners != 2 || qmkui.RunnerWorkDir != "_work" || qmkui.Template != "gh-runner-template" {
		t.Fatalf("unexpected qmkui runner settings: %+v", qmkui)
	}
	if qmkui.Labels != "self-hosted,linux,x64,nodev2,docker,runner-class-qmkui" {
		t.Fatalf("unexpected qmkui labels: %q", qmkui.Labels)
	}
	if got := strings.Join(qmkui.MatchLabels, ","); got != "self-hosted,linux,x64,nodev2,docker,runner-class-qmkui" {
		t.Fatalf("unexpected qmkui match labels: %q", got)
	}
}

func sortedClassIDs(classes map[string]RunnerClass) []string {
	ids := make([]string, 0, len(classes))
	for id := range classes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func hasLabel(labels, want string) bool {
	for _, label := range labelsList(labels) {
		if label == want {
			return true
		}
	}
	return false
}

func validConfig() *Config {
	cfg := defaults()
	cfg.CI.Org = "test-org"
	cfg.CI.GitHub.Token = "token"
	cfg.Webhook.Enabled = false
	cfg.Metrics.Enabled = false
	return cfg
}
