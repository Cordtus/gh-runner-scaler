// Package runnerobs builds bounded, credential-free Promtail configuration for
// one ephemeral GitHub Actions runner.
package runnerobs

import (
	"fmt"
	"strings"
)

type Config struct {
	PushURL    string
	MaxRetries int
}

type Runner struct {
	GroupID   string
	Container string
	Target    string
}

type FailureClass string

const (
	Permanent FailureClass = "permanent"
	Transient FailureClass = "transient"
)

func ClassifyStatus(status int) FailureClass {
	if status == 429 || status >= 500 {
		return Transient
	}
	return Permanent
}

func RenderConfig(cfg Config, runner Runner) (string, error) {
	if cfg.PushURL == "" || runner.GroupID == "" || runner.Container == "" || runner.Target == "" {
		return "", fmt.Errorf("runner log configuration requires endpoint and runner identity")
	}
	for _, value := range []string{cfg.PushURL, runner.GroupID, runner.Container, runner.Target} {
		if strings.ContainsAny(value, "\n\r") {
			return "", fmt.Errorf("runner log configuration contains a newline")
		}
	}
	return fmt.Sprintf(`server:
  http_listen_port: 9080
  grpc_listen_port: 0
positions:
  filename: /var/lib/promtail/positions.yaml
clients:
  - url: %s
    backoff_config:
      min_period: 1s
      max_period: 1m
      max_retries: %d
scrape_configs:
  - job_name: github-actions-runner
    static_configs:
      - targets: [localhost]
        labels:
          job: github-actions
          runner_group: %s
          runner: %s
          repo: %s
          log_kind: diagnostics
          __path__: /home/runner/_diag/Runner_*.log
      - targets: [localhost]
        labels:
          job: github-actions
          runner_group: %s
          runner: %s
          repo: %s
          log_kind: diagnostics
          __path__: /home/runner/_diag/Worker_*.log
  - job_name: github-actions-jobs
    static_configs:
      - targets: [localhost]
        labels:
          job: github-actions
          runner_group: %s
          runner: %s
          repo: %s
          log_kind: job
          __path__: /home/runner/_work/**/*.log
`, cfg.PushURL, cfg.MaxRetries, runner.GroupID, runner.Container, runner.Target, runner.GroupID, runner.Container, runner.Target, runner.GroupID, runner.Container, runner.Target), nil
}
