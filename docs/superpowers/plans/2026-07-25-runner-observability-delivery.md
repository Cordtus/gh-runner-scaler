# Runner Observability Delivery Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` or `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Preserve lifecycle metrics and complete runner logs without template replay, credentials in clones, or unbounded retry work.

**Architecture:** Existing daemon metrics remain unchanged. A testable bootstrapper validates an unauthenticated internal Loki endpoint, renders a per-container Promtail config, and starts a disabled template service after GitHub runner registration. A bootstrap failure emits one issue event and never blocks CI capacity.

**Tech Stack:** Go, LXD API, TOML, Promtail YAML, Loki HTTP API, Go `httptest`.

---

## File structure

- Create `internal/runnerobs/{config,render,preflight,bootstrap}.go` and focused tests.
- Modify `internal/config/config.go`, `cmd/scaler/main.go`, and `internal/engine/reconciler.go` plus their tests.
- Create `deploy/prepare-runner-template-observability.sh`; modify nodev2 config, README, and the Grafana dashboard.

### Task 1: Define a credential-free configuration contract

**Files:** `internal/config/config.go`, `internal/config/config_test.go`, `config.example.toml`, `deploy/nodev2.config.toml`

- [ ] **Step 1: Write failing validation tests.** Add cases proving enabled runner log delivery requires `RUNNER_LOG_LOKI_PUSH_URL` and `RUNNER_LOG_LOKI_HEALTH_URL`, permits disabled delivery without either, and rejects nonempty `RUNNER_LOG_LOKI_USERNAME`, `RUNNER_LOG_LOKI_PASSWORD`, or `RUNNER_LOG_LOKI_API_KEY`:

```go
func TestLoad_RejectsRunnerLogCredentials(t *testing.T) {
    t.Setenv("RUNNER_LOG_LOKI_PUSH_URL", "http://loki.internal/loki/api/v1/push")
    t.Setenv("RUNNER_LOG_LOKI_USERNAME", "forbidden")
    _, err := Load(writeConfig(t, "[runner_observability]\\nenabled = true\\n"))
    if err == nil || !strings.Contains(err.Error(), "must not use credentials") {
        t.Fatalf("Load error = %v, want credential rejection", err)
    }
}
```

This rejects the concrete bad behavior of forwarding host/Grafana credentials into a clone.

- [ ] **Step 2: Run the focused test.** `GOCACHE=/tmp/gh-runner-scaler-go-cache go test ./internal/config -run 'TestLoad_(RejectsRunnerLogCredentials|RequiresRunnerLogEndpointsWhenEnabled|AllowsRunnerLogsDisabledWithoutEndpoint)' -count=1` must fail because the configuration does not exist.

- [ ] **Step 3: Implement the smallest configuration.** Add this top-level configuration and defaults: 

```go
type RunnerObservabilityConfig struct {
    Enabled bool `toml:"enabled"`
    PushURL string `toml:"-"`
    HealthURL string `toml:"-"`
    MaxRetries int `toml:"max_retries"`
    InitialBackoff Duration `toml:"initial_backoff"`
    MaxBackoff Duration `toml:"max_backoff"`
    MaxSourceBytes int64 `toml:"max_source_bytes"`
    MaxLifecycleBytes int64 `toml:"max_lifecycle_bytes"`
}
```

Read only the two endpoint environment variables. Use defaults of disabled, three retries, one-second initial backoff, one-minute max backoff, 16 MiB per file, and 128 MiB per lifecycle. Validate URLs, positive limits, and ordered backoffs. Enable it only in `deploy/nodev2.config.toml`.

- [ ] **Step 4: Verify and commit.** Run `GOCACHE=/tmp/gh-runner-scaler-go-cache go test ./internal/config -count=1`; commit `feat: configure bounded runner log delivery`.

### Task 2: Build the bootstrapper test-first

**Files:** `internal/runnerobs/config.go`, `render.go`, `preflight.go`, `bootstrap.go`, and `*_test.go`

- [ ] **Step 1: Write failing renderer/classification tests.** For group `node`, container `gh-runner-node-1`, and target `Cordtus/poolbet`, assert YAML labels are:

```go
map[string]string{
    "job": "github-actions", "runner_group": "node",
    "runner": "gh-runner-node-1", "repo": "Cordtus/poolbet",
}
```

It must only watch `/home/runner/_diag/Runner_*.log`, `/home/runner/_diag/Worker_*.log`, and `/home/runner/_work/**/*.log`; use `/var/lib/promtail/positions.yaml`; use bounded batch/backoff settings; and never render `basic_auth` or a supplied secret. Assert 400/401/403/404 are permanent and 429/5xx are transient. This is the regression test for the observed 401 retry storm.

- [ ] **Step 2: Run the test.** `GOCACHE=/tmp/gh-runner-scaler-go-cache go test ./internal/runnerobs -count=1` must fail because the package is absent.

- [ ] **Step 3: Implement the boundary and behavior.** Keep LXD out of this package:

```go
type Executor interface {
    ExecCommand(context.Context, string, []string) (string, error)
}
type Bootstrapper interface {
    Prepare(context.Context, Runner) error
}
```

`Prepare` probes `HealthURL` with a five-second timeout, at most `MaxRetries + 1` attempts, capped exponential backoff, and context cancellation. Treat 2xx as ready; 4xx except 429 as permanent; transport/429/5xx as transient. On success write YAML atomically through a base64 payload, clear only this clone's `_diag` and `_work` old contents, then run `systemctl enable --now promtail.service`. Do not interpolate YAML or labels into shell text.

- [ ] **Step 4: Add component tests.** With `httptest.NewServer`: 204 starts Promtail; 401 makes one request and no executor command; two 503s then 204 retry and start. A fake executor proves write-before-service-start.

- [ ] **Step 5: Verify and commit.** Run `GOCACHE=/tmp/gh-runner-scaler-go-cache go test ./internal/runnerobs -count=1`; commit `feat: bootstrap bounded runner log shipping`.

### Task 3: Integrate without reducing capacity

**Files:** `internal/engine/reconciler.go`, `internal/engine/reconciler_test.go`, `cmd/scaler/main.go`, `cmd/scaler/main_test.go`

- [ ] **Step 1: Write failing scale-up tests.** Add:

```go
func TestScaleUp_StartsObservabilityOnlyAfterRunnerRegistration(t *testing.T) {}
func TestScaleUp_CompletesWhenObservabilityBootstrapFails(t *testing.T) {}
```

The first must assert `config.sh --ephemeral`, bootstrap, then `svc.sh start`; the second must assert a permanent delivery error still reaches `scaled up`. These prevent both missing telemetry and log delivery suppressing CI capacity.

- [ ] **Step 2: Run focused failures.** `GOCACHE=/tmp/gh-runner-scaler-go-cache go test ./internal/engine -run 'TestScaleUp_(StartsObservabilityOnlyAfterRunnerRegistration|CompletesWhenObservabilityBootstrapFails)' -count=1` must fail because there is no bootstrap dependency.

- [ ] **Step 3: Implement injection.** Add `Observability runnerobs.Bootstrapper` to `engine.ReconcilerConfig`. After `config.sh` succeeds, call it with class ID, target, and container name. On failure log `event_type=runner_log_delivery`, `action=disabled`, and classified reason, then start the runner service. Existing retained daemon logs and issue-event analytics deliver that warning to Loki. Construct one bootstrapper per runner class in `cmd/scaler/main.go`; pass nil when disabled.

- [ ] **Step 4: Verify and commit.** Run `GOCACHE=/tmp/gh-runner-scaler-go-cache go test ./internal/engine ./cmd/scaler -count=1`; commit `feat: keep runner capacity independent of log delivery`.

### Task 4: Make template state explicit and clone-safe

**Files:** `deploy/prepare-runner-template-observability.sh`, `internal/runnerobs/template_contract_test.go`, `README.md`, `deploy/update-server.sh`

- [ ] **Step 1: Write a failing template contract test.** Against a temporary fake root, prove the script creates a disabled unit, clears `_diag` and `_work`, creates `/var/lib/promtail`, and leaves no `basic_auth` in generated files.

- [ ] **Step 2: Implement the idempotent script.** Require stopped `gh-runner-template`; fail before mutation if running. Stop/disable inherited Promtail, remove only template runner log contents, create state storage, and install the no-credential unit. Do not invoke it automatically during server deploy.

- [ ] **Step 3: Verify and commit.** Run `GOCACHE=/tmp/gh-runner-scaler-go-cache go test ./internal/runnerobs -run TestTemplatePreparation -count=1 && bash -n deploy/prepare-runner-template-observability.sh`; commit `feat: prepare clean observable runner templates`.

### Task 5: Surface failures and verify the live lifecycle

**Files:** `deploy/grafana-dashboard.json`, `README.md`, `docs/superpowers/specs/2026-07-25-runner-observability-delivery-design.md`

- [ ] **Step 1: Write a dashboard contract test.** Treat dashboard JSON as a published artifact; assert a runner-log issue table queries `{job="gh-runner-scaler",service="issue-events"} | json | event_type="runner_log_delivery"` and distinguishes `disabled`, `retry_exhausted`, and `truncated`.

- [ ] **Step 2: Implement panel and operational instructions.** Document normal flow, deliberate rejected-endpoint test, expected one issue event, and rollback: disable `runner_observability`, reload the daemon, and keep CI capacity active.

- [ ] **Step 3: Run repository verification.** `GOCACHE=/tmp/gh-runner-scaler-go-cache go test ./... && go vet ./... && git diff --check`.

- [ ] **Step 4: Conduct controlled nodev2 validation.** Deploy with `sudo ./deploy/update-server.sh`; explicitly prepare the stopped template; set only non-secret internal endpoints; trigger a disposable workflow; verify current diagnostics/job logs and lifecycle metrics. Then test a controlled 401 health endpoint: runner must register, exactly one issue event must appear, Promtail must not start, and CPU/load must stay stable. Restore endpoint and prove a second workflow delivers logs.

- [ ] **Step 5: Commit.** Commit dashboard/docs with `feat: expose runner log delivery guardrails`.
