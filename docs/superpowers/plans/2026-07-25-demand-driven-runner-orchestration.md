# Demand-Driven Runner Orchestration and Local Distribution Cache Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the current idle-runner replacement loop with one persistent general-purpose baseline plus demand-driven overflow, while downloading and verifying the GitHub Actions runner distribution at most once per day and reusing it for every new runner.

**Architecture:** `gh-runner-scaler` becomes the sole source of truth for runner classes, queue demand, baseline policy, runner distribution maintenance, deployment units, documentation, and dashboards. One non-ephemeral Poolbet Node/Docker runner stays online as the baseline; queued-job demand creates class-matched ephemeral overflow, and completed overflow returns the system to that single baseline. A repository-owned daily systemd job downloads a changed runner release once, verifies its GitHub-published SHA-256 digest, installs it into the stopped LXD template through an atomic versioned symlink, and causes new clones to register with `--disableupdate`.

**Tech Stack:** Go, `go-github`, TOML, LXD/LXC, Bash, systemd services and timers, GitHub `workflow_job` webhooks, Grafana Loki, Grafana dashboard JSON.

---

## 1. Handoff status and repository boundary

This document was written on 2026-07-25 from:

- Repository: `/home/cordt/repos/gh-runner-scaler`
- Branch: `main`
- Starting commit: `72e5266` (`feat: prepare clean runner log templates`)
- Git state: `main` was eight commits ahead of `origin/main`; no uncommitted files were present.
- Runtime host: `nodev2`, reached on the LAN as `bv@192.168.0.170`
- Metrics/Loki host: `sv@192.168.0.157`

All permanent runner-related artifacts belong in this repository:

- Go orchestration logic
- Runner class definitions and nodev2 configuration
- Runner distribution download/cache logic
- Template maintenance scripts
- systemd units and timers
- Grafana dashboard definitions
- Tests, runbooks, and deployment instructions

Nodev2 may contain only deployed runtime artifacts:

- `/usr/local/bin/gh-runner-scaler`
- `/etc/gh-runner-scaler/config.toml`
- `/etc/gh-runner-scaler/env`
- `/var/lib/gh-runner-scaler/`
- deployed systemd units
- the stopped LXD template and live runner containers

Do not put runner orchestration in `routerUp`, hand-edit nodev2 configuration without changing `deploy/nodev2.config.toml`, or maintain an untracked nodev2-only script.

## 2. Evidence and underlying cause

### 2.1 Network investigation

The runner traffic was real but did not saturate the local network:

- WAN `eth0` negotiated 2.5 Gbit/s full duplex.
- The nodev2 router link negotiated 2.5 Gbit/s.
- The external WAP/switch link negotiated 1 Gbit/s and is on a different router port.
- No meaningful CRC, carrier, or physical-link errors were observed.
- Router CPU, memory, DNS, and interface health were normal.
- The observed runner traffic was roughly 473 MB, far below the available WAN and LAN capacity.

The selective Telegram/ThisVid slowness was therefore not explained by nodev2 runner traffic or local link saturation. No router or WAP change belongs in this implementation.

### 2.2 Excessive GitHub download

Router flow telemetry recorded two simultaneous GitHub downloads to nodev2:

- `185.199.111.133 -> 192.168.0.170`: 236,667,200 bytes
- `185.199.108.133 -> 192.168.0.170`: 236,517,600 bytes

One newly created runner reported approximately 235.66 MB received and logged:

```text
Current runner version: '2.333.0'
Current running runner version is 2.333.0
Downloading 2.336.0 runner
Downloading https://github.com/actions/runner/releases/download/v2.336.0/actions-runner-linux-x64-2.336.0.tar.gz
PackageDownloadTime: 7364ms
Current runner version: '2.336.0'
```

The two approximately 236 MB flows were two ephemeral runners independently self-updating from the stale template.

### 2.3 Churn

The current reconciler performs this scale-up test:

```go
if availableOnline == 0 && pendingCapacity == 0 && autoCount < r.cfg.MaxAutoRunners {
    r.scaleUp(...)
}
```

That test treats absence of idle capacity as proof of job demand. With:

```toml
idle_timeout = "300s"
poll_interval = "30s"
```

the loop is:

```text
idle runner reaches five minutes
  -> runner/container is removed
  -> next reconcile sees zero available runners
  -> replacement is created without a queued job
  -> replacement downloads ~236 MB to self-update
  -> repeat independently for every enabled class
```

### 2.4 Dashboard measurements

The live lifecycle metric window covered 2026-07-22T11:20:24Z through 2026-07-25T17:20:02Z:

| Metric | Observed value | Interpretation |
|---|---:|---|
| Queue-wait samples | 6 | Workload sample is small |
| Average queue wait | 2.63 s | Jobs normally found an already-warm runner |
| P95 queue wait | 2.79 s | Same warm-capacity effect |
| Lifecycle samples | 2,829 | The scaler was churning heavily |
| Jobs per lifecycle | 0 | Lifecycle accounting showed no useful reuse |
| Reused lifecycle | 0% | Idle runners were not creating measured reuse |
| Scale-down to scale-up samples | 2,588 | Most teardown events were followed by replacement |
| Average scale-down to scale-up gap | 20.69 s | Matches the 30-second reconcile cadence |

Observed provisioning took roughly 17–29 seconds from scale-up start to registered runner. A demand-only specialised job should therefore target a queue-to-ready SLO of 45 seconds, not the current 2–3 second warm-runner timing.

Completed workflow runs in the preceding 14 days were:

| Target/workload | Runs |
|---|---:|
| Poolbet | 33 |
| CAC | 29 |
| QMKUI | 26 |
| gh-runner-scaler | 25 |
| the-clearooor | 7 |

The common environment is Linux/x64/nodev2/Docker. Browser and Foundry workloads are specialised. Poolbet is the highest-volume target and its jobs are comparatively long, so its ordinary Node/Docker class is the best owner for the sole warm baseline.

## 3. Decisions and constraints

### 3.1 Baseline

Create exactly one scaler-managed baseline:

```text
GitHub target: Cordtus/poolbet
Runner class:  node
Container:     gh-runner-primary
Labels:        self-hosted,linux,x64,nodev2,docker,runner-class-node
Lifecycle:     persistent, non-ephemeral
```

The baseline does not advertise QMKUI, CAC, browser, Foundry, Clearooor, or scaler-specific labels.

GitHub personal-account runners are repository-scoped. One runner registration cannot serve the CAC organisation plus multiple `Cordtus/*` repository targets. “One baseline” therefore means one global baseline assigned to the busiest compatible target, not one runner pretending to cover unrelated registration scopes.

### 3.2 Overflow

- Poolbet Node jobs use the baseline immediately when it is idle.
- If a matching job queues while the baseline is busy, create only the required Node overflow.
- CAC, QMKUI, gh-runner-scaler, Clearooor, browser, and Foundry start at zero and create runners only for matching queued jobs.
- Overflow runners remain ephemeral and are removed after their single job.
- A provisioned overflow runner that never receives a job is removed after a ten-minute safety timeout.
- No class creates a runner solely because its available-runner count is zero.

### 3.3 Distribution cache

- Check GitHub for the latest `actions/runner` Linux x64 release once daily.
- Keep versioned archives under `/var/lib/gh-runner-scaler/runner-distributions/`.
- Download only when the latest version differs from the locally recorded version.
- Require the release asset’s `sha256:` digest and verify it before installation.
- Install into a versioned directory in the stopped LXD template.
- Switch `/opt/actions-runner/current` atomically.
- Register every baseline and overflow runner with `--disableupdate`.
- Keep the two newest verified archives and versions for rollback.
- Never mutate a busy live runner.
- Rotate an outdated baseline only after GitHub reports it idle.

GitHub requires self-hosted runner software to remain current; disabled self-update transfers responsibility to this daily maintenance path. A refresh failure must retain the last verified template and surface an issue event without reducing existing capacity.

### 3.4 Timing

Use these initial values:

```toml
[scaler]
poll_interval = "30s"
queue_audit_interval = "5m"
demand_ttl = "30m"

[metrics]
interval = "60s"

[runner_distribution]
check_interval = "24h"
retain_versions = 2
```

Keep the existing two-second webhook debounce. The 30-second poll maintains active containers and the baseline; it must not scan and scale every empty class. The five-minute queue audit is only a recovery path for a missed webhook. Metrics at 60 seconds are sufficient for lifecycle tuning and reduce repeated GitHub inventory reads from the current 20-second deployment.

## 4. Desired state flow

```text
GitHub workflow_job webhook
  |
  +-- queued ------> persisted demand tracker ------> targeted reconcile
  |                                                   |
  |                                                   +-- compatible idle runner exists
  |                                                   |     -> do not provision
  |                                                   |
  |                                                   +-- queued demand exceeds
  |                                                         idle + provisioning capacity
  |                                                         -> create exact overflow
  |
  +-- in_progress --> remove job from queued demand
  |
  +-- completed ----> remove job from queued demand
                      trigger cleanup reconcile

30-second active maintenance
  +-- ensure Poolbet baseline exists and is online
  +-- clean stopped/orphaned/expired managed containers
  +-- never infer demand from zero idle capacity

5-minute queue audit
  +-- recover queued jobs whose webhook was missed
  +-- merge by job ID into the persisted demand tracker

daily runner distribution refresh
  +-- fetch latest release metadata
  +-- no version change: exit without download or LXD mutation
  +-- changed version: download once, verify digest, install stopped template
  +-- restart scaler so it learns the desired version
  +-- rotate baseline only after it is idle
```

## 5. File ownership map

### Create

- `internal/demand/tracker.go`: concurrency-safe, persisted queued-job demand
- `internal/demand/tracker_test.go`: deduplication, clearing, expiry, persistence tests
- `internal/runnerdist/cache.go`: release metadata, download, checksum, retention
- `internal/runnerdist/cache_test.go`: HTTP and filesystem behavior tests
- `deploy/refresh-runner-template.sh`: guarded installation into the stopped template
- `deploy/systemd/gh-runner-distribution-refresh.service`: one-shot refresh service
- `deploy/systemd/gh-runner-distribution-refresh.timer`: daily persistent timer
- `docs/runner-orchestration-runbook.md`: operator-facing normal flow, diagnosis, rollback

### Modify

- `internal/domain/types.go`: in-progress event and demand/metrics value types
- `internal/config/config.go`: baseline, demand, queue-audit, and distribution configuration
- `internal/config/config_test.go`: defaults, validation, and nodev2 contract
- `internal/iface/ci.go`: optional queued-job audit interface
- `provider/github/webhook.go`: preserve `in_progress` as a first-class event
- `provider/github/webhook_test.go`: queued/in-progress/completed parsing
- `provider/github/ci.go`: bounded queued-job audit for repo and organisation targets
- `provider/github/ci_test.go`: pagination, label preservation, and failure tests
- `internal/daemon/daemon.go`: demand-aware targeted reconcile and active-group maintenance
- `internal/daemon/webhook.go`: record and clear demand before triggering groups
- `internal/daemon/daemon_test.go`: routing, restart, missed-webhook, and no-demand tests
- `internal/engine/reconciler.go`: baseline lifecycle and demand-based overflow decisions
- `internal/engine/reconciler_test.go`: churn regression, baseline, overflow, and rotation tests
- `internal/iface/container.go`: template/instance version inspection required for rotation
- `provider/lxd/runtime.go`: implement version inspection without shell interpolation
- `provider/lxd/runtime_test.go`: version and error behavior
- `cmd/scaler/main.go`: wire demand, distribution version, baseline policy, and refresh command
- `cmd/scaler/main_test.go`: composition and command tests
- `deploy/nodev2.config.toml`: select the sole Poolbet baseline and new timings
- `config.example.toml`: document generic baseline/demand/distribution settings
- `deploy/update-server.sh`: install all repository-owned units and enable the timer
- `deploy/apply-nodev2-config.sh`: keep nodev2 deploy as a one-command source-of-truth path
- `deploy/grafana-dashboard.json`: baseline, demand, overflow, and distribution panels
- `README.md`: replace warm-pool semantics and manual stale runner download instructions

## 6. Implementation tasks

### Task 1: Define explicit demand and baseline configuration

**Files:**

- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `config.example.toml`

- [ ] **Step 1: Write failing configuration tests.**

Add behavior tests with these cases:

```go
func TestValidate_AllowsExactlyOneBaselineAcrossRunnerClasses(t *testing.T) {
    cfg := validConfig()
    cfg.RunnerClasses = []RunnerClassConfig{
        {
            ID: "node", Repo: "Cordtus/poolbet", Prefix: "gh-runner-node",
            Labels: "self-hosted,linux,x64,nodev2,docker,runner-class-node",
            Baseline: true, BaselineName: "gh-runner-primary",
        },
        {
            ID: "browser", Repo: "Cordtus/poolbet", Prefix: "gh-runner-browser",
            Labels: "self-hosted,linux,x64,nodev2,docker,runner-class-browser",
        },
    }
    if err := validate(cfg); err != nil {
        t.Fatalf("validate() error = %v", err)
    }
}

func TestValidate_RejectsMultipleBaselines(t *testing.T) {
    cfg := validConfig()
    cfg.RunnerClasses = []RunnerClassConfig{
        {ID: "one", Org: "Acme", Prefix: "one-auto", Baseline: true, BaselineName: "primary-one"},
        {ID: "two", Org: "Acme", Prefix: "two-auto", Baseline: true, BaselineName: "primary-two"},
    }
    err := validate(cfg)
    if err == nil || !strings.Contains(err.Error(), "only one runner class may enable baseline") {
        t.Fatalf("validate() error = %v, want one-baseline rejection", err)
    }
}

func TestValidate_RejectsBaselineNameInsideAutoPrefix(t *testing.T) {
    cfg := validConfig()
    cfg.RunnerClasses = []RunnerClassConfig{{
        ID: "node", Repo: "Cordtus/poolbet", Prefix: "gh-runner-node",
        Baseline: true, BaselineName: "gh-runner-node-primary",
    }}
    err := validate(cfg)
    if err == nil || !strings.Contains(err.Error(), "must not start with runner class prefix") {
        t.Fatalf("validate() error = %v, want prefix rejection", err)
    }
}
```

Also cover positive durations, absolute cache/version paths, `retain_versions >= 2`, and baseline name uniqueness.

- [ ] **Step 2: Run the focused tests and verify RED.**

Run:

```bash
GOCACHE=/tmp/gh-runner-scaler-go-cache \
go test ./internal/config \
  -run 'TestValidate_(AllowsExactlyOneBaselineAcrossRunnerClasses|RejectsMultipleBaselines|RejectsBaselineNameInsideAutoPrefix|RejectsInvalidDemandTiming|RejectsInvalidRunnerDistribution)' \
  -count=1
```

Expected: build failure because the new configuration fields do not exist.

- [ ] **Step 3: Add the configuration contract.**

Use these resolved fields:

```go
type ScalerConfig struct {
    Prefix            string   `toml:"prefix"`
    MaxAutoRunners    int      `toml:"max_auto_runners"`
    IdleTimeout       Duration `toml:"idle_timeout"`
    PollInterval      Duration `toml:"poll_interval"`
    QueueAuditInterval Duration `toml:"queue_audit_interval"`
    DemandTTL         Duration `toml:"demand_ttl"`
    Labels            string   `toml:"labels"`
    RunnerWorkDir     string   `toml:"runner_work_dir"`
}

type RunnerClassConfig struct {
    // Preserve existing fields.
    Baseline     bool   `toml:"baseline"`
    BaselineName string `toml:"baseline_name"`
}

type RunnerDistributionConfig struct {
    Enabled        bool     `toml:"enabled"`
    Repository     string   `toml:"repository"`
    Platform       string   `toml:"platform"`
    CacheDir       string   `toml:"cache_dir"`
    VersionFile    string   `toml:"version_file"`
    CheckInterval  Duration `toml:"check_interval"`
    RetainVersions int      `toml:"retain_versions"`
}
```

Add `RunnerDistribution RunnerDistributionConfig` to `Config`, and `Baseline` plus `BaselineName` to the resolved `RunnerClass`.

Defaults:

```go
QueueAuditInterval: Duration{5 * time.Minute},
DemandTTL:          Duration{30 * time.Minute},
RunnerDistribution: RunnerDistributionConfig{
    Enabled:        false,
    Repository:     "actions/runner",
    Platform:       "linux-x64",
    CacheDir:       "/var/lib/gh-runner-scaler/runner-distributions",
    VersionFile:    "/var/lib/gh-runner-scaler/runner-distributions/current-version",
    CheckInterval:  Duration{24 * time.Hour},
    RetainVersions: 2,
},
```

Validation must enforce:

- At most one enabled class has `baseline = true`.
- A baseline class has a nonempty `baseline_name`.
- `baseline_name` does not start with any managed auto prefix.
- Queue audit and demand TTL are positive.
- Distribution cache and version paths are absolute when enabled.
- Repository is exactly `owner/name`.
- Platform is `linux-x64`.
- Check interval is at least one hour.
- Retention is at least two.

- [ ] **Step 4: Update the generic example.**

Show `baseline = false` by default. Explain that personal repositories are separate GitHub registration targets and only one class in one scaler deployment may own the global baseline.

- [ ] **Step 5: Verify and commit.**

Run:

```bash
GOCACHE=/tmp/gh-runner-scaler-go-cache go test ./internal/config -count=1
git diff --check
git add internal/config/config.go internal/config/config_test.go config.example.toml
git commit -m "feat: define demand-driven runner policy"
```

Expected: tests pass and the commit contains only the configuration contract.

### Task 2: Persist queued-job demand

**Files:**

- Create: `internal/demand/tracker.go`
- Create: `internal/demand/tracker_test.go`
- Modify: `internal/domain/types.go`

- [ ] **Step 1: Define the observable behavior in failing tests.**

The tracker must:

- deduplicate repeated `queued` deliveries by GitHub job ID;
- maintain demand independently per runner group;
- remove demand on `in_progress` and `completed`;
- atomically persist demand and load it after restart;
- expire entries older than `demand_ttl`;
- use an injected clock so tests never sleep.

Use this test shape:

```go
func TestTracker_DeduplicatesAndClearsJobDemand(t *testing.T) {
    now := time.Date(2026, 7, 25, 18, 0, 0, 0, time.UTC)
    tracker, err := NewTracker(t.TempDir()+"/demand.json", 30*time.Minute, func() time.Time { return now })
    if err != nil {
        t.Fatal(err)
    }
    job := domain.QueuedJob{
        ID: 42, Repo: "Cordtus/poolbet",
        Labels: []string{"self-hosted", "linux", "runner-class-node"},
        QueuedAt: now,
    }

    if err := tracker.Queue("node", job); err != nil {
        t.Fatal(err)
    }
    if err := tracker.Queue("node", job); err != nil {
        t.Fatal(err)
    }
    if got := tracker.Snapshot("node").QueuedJobs; got != 1 {
        t.Fatalf("queued jobs = %d, want 1", got)
    }

    if err := tracker.Clear("node", 42); err != nil {
        t.Fatal(err)
    }
    if got := tracker.Snapshot("node").QueuedJobs; got != 0 {
        t.Fatalf("queued jobs = %d, want 0", got)
    }
}
```

Add restart and expiry tests that assert the JSON state, not private map structure.

- [ ] **Step 2: Run the tests and verify RED.**

```bash
GOCACHE=/tmp/gh-runner-scaler-go-cache go test ./internal/demand -count=1
```

Expected: package does not exist.

- [ ] **Step 3: Add provider-neutral domain values.**

```go
type QueuedJob struct {
    ID        int64
    Repo      string
    Labels    []string
    QueuedAt  time.Time
}

type CapacityDemand struct {
    QueuedJobs int
    OldestAge  time.Duration
}

const (
    EventUnknown WebhookEventType = iota
    EventJobQueued
    EventJobInProgress
    EventJobCompleted
    EventPush
)
```

- [ ] **Step 4: Implement atomic persistence.**

`Tracker` should own one mutex and persist a JSON document through `os.CreateTemp`, `Sync`, `Close`, and `os.Rename`. A failed write must return an error and retain the in-memory state needed to serve the current webhook; log delivery will expose the persistence error.

Public methods:

```go
type Clock func() time.Time

func NewTracker(path string, ttl time.Duration, now Clock) (*Tracker, error)
func (t *Tracker) Queue(groupID string, job domain.QueuedJob) error
func (t *Tracker) Clear(groupID string, jobID int64) error
func (t *Tracker) Replace(groupID string, jobs []domain.QueuedJob) error
func (t *Tracker) Snapshot(groupID string) domain.CapacityDemand
func (t *Tracker) ActiveGroups() []string
```

`Replace` is used by the missed-webhook queue audit and must not overwrite another group.

- [ ] **Step 5: Verify and commit.**

```bash
GOCACHE=/tmp/gh-runner-scaler-go-cache go test ./internal/demand ./internal/domain -count=1
git diff --check
git add internal/demand internal/domain/types.go
git commit -m "feat: persist queued runner demand"
```

### Task 3: Treat workflow-job transitions as demand events

**Files:**

- Modify: `provider/github/webhook.go`
- Modify: `provider/github/webhook_test.go`
- Modify: `internal/daemon/webhook.go`
- Modify: `internal/daemon/daemon.go`
- Modify: `internal/daemon/daemon_test.go`

- [ ] **Step 1: Write the parser regression test.**

Change the existing in-progress expectation to:

```go
if event.Type != domain.EventJobInProgress {
    t.Fatalf("event type = %v, want EventJobInProgress", event.Type)
}
```

Preserve job ID, target repo, labels, runner name, run ID, and run attempt for queued, in-progress, and completed actions.

- [ ] **Step 2: Write daemon demand-routing tests.**

Cover:

```go
func TestHandleWebhook_QueuedJobRecordsDemandBeforeTrigger(t *testing.T) {}
func TestHandleWebhook_InProgressClearsDemand(t *testing.T) {}
func TestHandleWebhook_CompletedClearsDemand(t *testing.T) {}
func TestHandleWebhook_DuplicateDeliveryDoesNotIncreaseDemand(t *testing.T) {}
func TestHandleWebhook_OnlyMatchingTargetAndLabelsReceiveDemand(t *testing.T) {}
```

The queued test’s oracle is a demand snapshot of one for the matched group when its reconcile begins. It must reject an implementation that triggers before recording demand.

- [ ] **Step 3: Run focused tests and verify RED.**

```bash
GOCACHE=/tmp/gh-runner-scaler-go-cache \
go test ./provider/github ./internal/daemon \
  -run 'Test(ParseWorkflowJob|HandleWebhook_.*Demand|HandleWebhook_.*Clears|HandleWebhook_Duplicate)' \
  -count=1
```

- [ ] **Step 4: Implement transition handling.**

Before scheduling a reconcile:

```go
switch event.Type {
case domain.EventJobQueued:
    for _, group := range groups {
        err := d.demand.Queue(group.ID, domain.QueuedJob{
            ID: event.JobID, Repo: event.Repo,
            Labels: append([]string(nil), event.Labels...),
            QueuedAt: time.Now().UTC(),
        })
        if err != nil {
            d.log.Error("failed to persist queued demand", "runner_group", group.ID, "job_id", event.JobID, "error", err)
        }
    }
case domain.EventJobInProgress, domain.EventJobCompleted:
    for _, group := range groups {
        if err := d.demand.Clear(group.ID, event.JobID); err != nil {
            d.log.Error("failed to clear queued demand", "runner_group", group.ID, "job_id", event.JobID, "error", err)
        }
    }
}
```

Use the daemon’s injected clock rather than calling `time.Now` directly in the final implementation.

- [ ] **Step 5: Verify and commit.**

```bash
GOCACHE=/tmp/gh-runner-scaler-go-cache go test ./provider/github ./internal/daemon -count=1
git diff --check
git add provider/github/webhook.go provider/github/webhook_test.go internal/daemon/webhook.go internal/daemon/daemon.go internal/daemon/daemon_test.go
git commit -m "feat: route workflow demand by job lifecycle"
```

### Task 4: Replace zero-idle scaling with demand-based overflow

**Files:**

- Modify: `internal/engine/reconciler.go`
- Modify: `internal/engine/reconciler_test.go`
- Modify: `internal/daemon/daemon.go`
- Modify: `internal/daemon/daemon_test.go`

- [ ] **Step 1: Add the churn regression test.**

```go
func TestReconcile_ZeroAvailableWithoutDemandDoesNotScaleUp(t *testing.T) {
    runtime := &mockRuntime{}
    ci := &mockCI{}
    state := newMockState()
    r := newTestReconciler(runtime, ci, state, nil)

    if err := r.Reconcile(context.Background(), domain.CapacityDemand{}); err != nil {
        t.Fatal(err)
    }
    if len(runtime.containers) != 0 {
        t.Fatalf("containers = %v, want no scale-up without queued demand", runtime.containers)
    }
}
```

This is the direct regression for the production churn.

- [ ] **Step 2: Add capacity arithmetic tests.**

Cover:

- one queued job and one compatible idle runner creates zero overflow;
- one queued job and a busy baseline creates one overflow;
- two queued jobs and one provisioning runner creates one additional overflow;
- demand cannot exceed `max_auto_runners`;
- an idle runner with different labels does not satisfy demand;
- stopped and orphaned overflow is cleaned even when demand is zero;
- an unclaimed idle overflow is deleted after ten minutes without being replaced.

- [ ] **Step 3: Run focused tests and verify RED.**

```bash
GOCACHE=/tmp/gh-runner-scaler-go-cache \
go test ./internal/engine \
  -run 'TestReconcile_(ZeroAvailableWithoutDemandDoesNotScaleUp|QueuedDemand|DemandRespectsCap|CleansUnusedOverflow)' \
  -count=1
```

- [ ] **Step 4: Change the reconcile contract.**

```go
func (r *Reconciler) Reconcile(ctx context.Context, demand domain.CapacityDemand) error
```

After cleanup, calculate:

```go
missing := demand.QueuedJobs - availableOnline - pendingCapacity
remainingCap := r.cfg.MaxAutoRunners - autoCount
if missing > remainingCap {
    missing = remainingCap
}
if missing < 0 {
    missing = 0
}
```

Provision exactly `missing` runners. Keep provisioning bounded to two concurrent clones so a backlog does not serialize 20–30 second cold starts, while current class caps remain authoritative. Aggregate provisioning errors with `errors.Join`; successful siblings stay available.

Delete the unconditional `availableOnline == 0` scale-up branch.

- [ ] **Step 5: Pass demand from the daemon.**

Targeted webhook reconciles use the latest group snapshot:

```go
demand := d.demand.Snapshot(group.ID)
err := group.Reconciler.Reconcile(ctx, demand)
```

The CLI `reconcile` command passes an empty demand snapshot; it performs maintenance and baseline repair but cannot create overflow without evidence of a queued job.

- [ ] **Step 6: Verify and commit.**

```bash
GOCACHE=/tmp/gh-runner-scaler-go-cache go test ./internal/engine ./internal/daemon ./cmd/scaler -count=1
git diff --check
git add internal/engine/reconciler.go internal/engine/reconciler_test.go internal/daemon/daemon.go internal/daemon/daemon_test.go cmd/scaler/main.go cmd/scaler/main_test.go
git commit -m "fix: scale overflow only for queued jobs"
```

### Task 5: Manage the one persistent baseline

**Files:**

- Modify: `internal/engine/reconciler.go`
- Modify: `internal/engine/reconciler_test.go`
- Modify: `internal/iface/container.go`
- Modify: `provider/lxd/runtime.go`
- Modify: `provider/lxd/runtime_test.go`
- Modify: `cmd/scaler/main.go`
- Modify: `cmd/scaler/main_test.go`

- [ ] **Step 1: Write baseline lifecycle tests.**

Required cases:

```go
func TestReconcile_CreatesConfiguredBaselineWithoutDemand(t *testing.T) {}
func TestReconcile_RegistersBaselineWithoutEphemeralFlag(t *testing.T) {}
func TestReconcile_BaselineUsesDisableUpdate(t *testing.T) {}
func TestReconcile_DoesNotIdleDeleteBaseline(t *testing.T) {}
func TestReconcile_DoesNotCreateSecondBaselineWhenBusy(t *testing.T) {}
func TestReconcile_RepairsStoppedBaselineContainer(t *testing.T) {}
func TestReconcile_DefersOutdatedBaselineRotationWhileBusy(t *testing.T) {}
func TestReconcile_RotatesOutdatedBaselineWhenIdle(t *testing.T) {}
```

Assert observable runner/container lifecycle and complete `config.sh` arguments. Do not test private helper calls.

- [ ] **Step 2: Run focused tests and verify RED.**

```bash
GOCACHE=/tmp/gh-runner-scaler-go-cache \
go test ./internal/engine \
  -run 'TestReconcile_.*Baseline' \
  -count=1
```

- [ ] **Step 3: Add baseline settings to the reconciler.**

```go
type ReconcilerConfig struct {
    // Preserve existing fields.
    BaselineEnabled bool
    BaselineName    string
    DesiredVersion  string
}
```

Provisioning must use one shared method with an explicit lifecycle:

```go
type runnerLifecycle int

const (
    lifecycleEphemeral runnerLifecycle = iota
    lifecyclePersistent
)

func (r *Reconciler) provision(ctx context.Context, name string, lifecycle runnerLifecycle) error
```

Both forms use:

```text
--disableupdate --replace --unattended
```

Only overflow adds:

```text
--ephemeral
```

Run configuration from:

```text
/opt/actions-runner/current/config.sh
```

Use the absolute work path:

```text
/home/runner/_work
```

- [ ] **Step 4: Keep the baseline outside auto cleanup.**

`gh-runner-primary` must not match any auto prefix. Reconcile it before overflow cleanup:

- if its runner and container are online, leave it unchanged;
- if container exists but is stopped, start it and wait for the runner;
- if an offline GitHub registration has no container, delete that registration and recreate;
- if the instance version differs from `DesiredVersion`, rotate only when the runner is not busy;
- never apply overflow idle timeout to it.

- [ ] **Step 5: Add safe version inspection.**

Extend `ContainerRuntime`:

```go
ReadFile(ctx context.Context, name, path string) ([]byte, error)
```

The LXD provider should use the LXD file API rather than `bash -c`. Read:

```text
/opt/actions-runner/current/.runner-dist-version
```

Trim whitespace and treat a missing marker as outdated.

- [ ] **Step 6: Verify and commit.**

```bash
GOCACHE=/tmp/gh-runner-scaler-go-cache go test ./internal/engine ./provider/lxd ./cmd/scaler -count=1
git diff --check
git add internal/engine internal/iface/container.go provider/lxd cmd/scaler
git commit -m "feat: manage one persistent primary runner"
```

### Task 6: Add a bounded missed-webhook queue audit

**Files:**

- Modify: `internal/iface/ci.go`
- Modify: `provider/github/ci.go`
- Modify: `provider/github/ci_test.go`
- Modify: `internal/daemon/daemon.go`
- Modify: `internal/daemon/daemon_test.go`

- [ ] **Step 1: Write provider contract tests.**

Use `httptest.Server` and cover:

- repo target lists queued runs, then jobs, and returns only jobs with status `queued`;
- job IDs and labels are preserved;
- pagination is bounded;
- a partial API failure returns an error and does not replace known demand with empty demand;
- organisation audit uses the provider’s existing bounded repository enumeration;
- completed/in-progress jobs never appear in the queued result.

- [ ] **Step 2: Add an optional capability.**

```go
type QueuedJobPage struct {
    Jobs       []domain.QueuedJob
    NextCursor string
    Complete   bool
}

type QueuedJobProvider interface {
    ListQueuedJobs(ctx context.Context, cursor string, repoLimit int) (QueuedJobPage, error)
}
```

Do not add it to the base `CIProvider`; tests and future providers that rely on webhook-only scaling remain valid.

- [ ] **Step 3: Implement bounded GitHub queries.**

For repo targets:

1. list recent workflow runs with `status=queued`;
2. list recent workflow runs with `status=in_progress`, because an active run may still contain queued jobs;
3. list the latest jobs for the union of those run IDs;
4. retain jobs whose status is `queued`;
5. deduplicate by job ID.

For organisation targets, reuse the existing repository enumeration and process no more than `workflow_repo_batch_size` repositories per audit pass. Carry the cursor and a staging set of audited jobs between passes in the daemon state directory. Return `Complete=false` until every repository in the cycle has been inspected.

- [ ] **Step 4: Add the recovery loop.**

Run queue audits every five minutes, separately from the 30-second active maintenance loop. Route audited jobs through the same target and label matcher as webhooks. Merge incomplete pages into the staging set without clearing current demand; only call `Tracker.Replace` after `Complete=true` closes a full audit cycle. On failure, retain persisted demand and the prior completed audit, discard no jobs, and emit an issue event.

- [ ] **Step 5: Verify and commit.**

```bash
GOCACHE=/tmp/gh-runner-scaler-go-cache go test ./provider/github ./internal/daemon -count=1
git diff --check
git add internal/iface/ci.go provider/github internal/daemon
git commit -m "feat: recover demand after missed webhooks"
```

### Task 7: Stop polling empty classes needlessly

**Files:**

- Modify: `internal/daemon/daemon.go`
- Modify: `internal/daemon/daemon_test.go`
- Modify: `cmd/scaler/main.go`

- [ ] **Step 1: Add scheduler tests.**

Prove:

- startup reconciles the baseline class;
- startup does not provision or inventory-scan every empty demand-only class;
- the 30-second tick reconciles groups with a baseline, live managed containers, or queued demand;
- a webhook immediately reconciles only matched groups;
- the five-minute audit is the only cold-group polling path;
- a trigger arriving during reconciliation causes one rerun and is not lost.

- [ ] **Step 2: Introduce group activity state.**

Track whether each group has:

- baseline enabled;
- persisted queued demand;
- known managed containers from its last reconciliation;
- an explicit webhook trigger.

The maintenance selector must be deterministic and testable:

```go
func activeGroupIDs(groups []RunnerGroup, demand *demand.Tracker, knownContainers map[string]int) []string
```

- [ ] **Step 3: Preserve cleanup correctness.**

After a group scales its final overflow down, retain it as active for one additional successful pass so GitHub deregistration and LXD deletion errors are retried. Remove it from active maintenance only when the reconciler reports zero managed containers and no unresolved cleanup error.

- [ ] **Step 4: Verify and commit.**

```bash
GOCACHE=/tmp/gh-runner-scaler-go-cache go test ./internal/daemon ./cmd/scaler -count=1
git diff --check
git add internal/daemon cmd/scaler
git commit -m "perf: poll only active runner groups"
```

### Task 8: Cache and verify the runner distribution once daily

**Files:**

- Create: `internal/runnerdist/cache.go`
- Create: `internal/runnerdist/cache_test.go`
- Modify: `cmd/scaler/main.go`
- Modify: `cmd/scaler/main_test.go`

- [ ] **Step 1: Write HTTP/cache tests.**

Use `httptest.Server`; no test may call GitHub. Cover:

```go
func TestRefresh_DoesNotDownloadWhenVersionIsCurrent(t *testing.T) {}
func TestRefresh_DownloadsChangedVersionOnce(t *testing.T) {}
func TestRefresh_RejectsMissingDigest(t *testing.T) {}
func TestRefresh_RejectsDigestMismatch(t *testing.T) {}
func TestRefresh_AtomicallyPublishesVerifiedVersion(t *testing.T) {}
func TestRefresh_RetainsTwoNewestVerifiedArchives(t *testing.T) {}
func TestRefresh_KeepsCurrentVersionWhenDownloadFails(t *testing.T) {}
```

The digest mismatch test must assert that neither `current-version` nor the published archive changes.

- [ ] **Step 2: Run tests and verify RED.**

```bash
GOCACHE=/tmp/gh-runner-scaler-go-cache go test ./internal/runnerdist -count=1
```

- [ ] **Step 3: Implement the release boundary.**

Use these public types:

```go
type Release struct {
    Version     string
    AssetName   string
    DownloadURL string
    SHA256      string
}

type Cache struct {
    Client     *http.Client
    APIBaseURL string
    Repository string
    Platform   string
    Dir        string
    Retain     int
}

type RefreshResult struct {
    Changed     bool
    Version     string
    ArchivePath string
}

func (c *Cache) Refresh(ctx context.Context) (RefreshResult, error)
```

Request:

```text
GET /repos/actions/runner/releases/latest
```

Select exactly:

```text
actions-runner-linux-x64-<version>.tar.gz
```

Require the asset `digest` to begin with `sha256:`. Download into a temporary file in the cache directory, stream through `sha256.New`, compare in constant time, `fsync`, rename to the versioned archive, and atomically replace `current-version`.

Use an exclusive lock file so timer overlap or a manual refresh cannot double-download.

- [ ] **Step 4: Add the command.**

Add:

```text
gh-runner-scaler runner-dist-refresh --config /etc/gh-runner-scaler/config.toml
```

With `--json`, stdout is a stable machine-readable contract:

```json
{"changed":true,"version":"2.336.0","archive_path":"/var/lib/gh-runner-scaler/runner-distributions/actions-runner-linux-x64-2.336.0.tar.gz"}
```

Exit behavior:

- `0`, unchanged: print the current version and do not touch LXD;
- `0`, changed and installed: print old/new version;
- nonzero: preserve the old cache/template and print the classified failure.

- [ ] **Step 5: Verify and commit.**

```bash
GOCACHE=/tmp/gh-runner-scaler-go-cache go test ./internal/runnerdist ./cmd/scaler -count=1
go vet ./internal/runnerdist ./cmd/scaler
git diff --check
git add internal/runnerdist cmd/scaler
git commit -m "feat: cache verified runner distributions"
```

### Task 9: Atomically refresh the stopped LXD template

**Files:**

- Create: `deploy/refresh-runner-template.sh`
- Create: `deploy/refresh-runner-template_test.sh`
- Modify: `deploy/prepare-runner-template-observability.sh`
- Modify: `internal/runnerobs/render.go`
- Modify: `internal/runnerobs/render_test.go`

- [ ] **Step 1: Build a fake-LXC shell test.**

The test must provide temporary fake `lxc` and scaler commands through `PATH` and verify:

- a running template is rejected before mutation;
- an unchanged release never starts the template;
- a changed verified release starts and stops the template exactly once;
- failure leaves `/opt/actions-runner/current` pointing to the previous version;
- a successful install writes `.runner-dist-version`;
- no baseline or overflow container is touched.

- [ ] **Step 2: Implement the guarded installer.**

The repository-owned script must:

1. run `runner-dist-refresh --json` and parse the stable result with `jq`;
2. exit without LXD work when unchanged;
3. require `gh-runner-template` status `STOPPED`;
4. start the template with a trap that always stops it;
5. push the archive to a temporary template path;
6. extract into `/opt/actions-runner/versions/<version>.new`;
7. validate `config.sh`, `run.sh`, and `bin/Runner.Listener`;
8. chown the version directory to `runner:runner`;
9. rename it to `/opt/actions-runner/versions/<version>`;
10. atomically replace `/opt/actions-runner/current`;
11. write `.runner-dist-version`;
12. clear only template `_diag` and `_work` contents;
13. stop the template;
14. restart the scaler daemon only after a changed version was installed.

Use `flock` and literal positional arguments. Do not interpolate archive paths, versions, or container names into an unquoted remote shell string.

- [ ] **Step 3: Update runner paths.**

The reconciler runs configuration and service installation from `/opt/actions-runner/current`. Update runner observability to collect:

```text
/opt/actions-runner/current/_diag/Runner_*.log
/opt/actions-runner/current/_diag/Worker_*.log
/home/runner/_work/**/*.log
```

Update the template observability script to clear the new diagnostic directory.

- [ ] **Step 4: Verify and commit.**

```bash
bash deploy/refresh-runner-template_test.sh
bash -n deploy/refresh-runner-template.sh
bash -n deploy/prepare-runner-template-observability.sh
GOCACHE=/tmp/gh-runner-scaler-go-cache go test ./internal/runnerobs -count=1
git diff --check
git add deploy/refresh-runner-template.sh deploy/refresh-runner-template_test.sh deploy/prepare-runner-template-observability.sh internal/runnerobs
git commit -m "feat: atomically refresh the runner template"
```

### Task 10: Install daily maintenance through repository-owned systemd units

**Files:**

- Create: `deploy/systemd/gh-runner-distribution-refresh.service`
- Create: `deploy/systemd/gh-runner-distribution-refresh.timer`
- Modify: `deploy/update-server.sh`
- Modify: `deploy/apply-nodev2-config.sh`

- [ ] **Step 1: Add the one-shot service.**

Use:

```ini
[Unit]
Description=Refresh cached GitHub Actions runner distribution
After=network-online.target
Wants=network-online.target
ConditionPathExists=/etc/gh-runner-scaler/config.toml

[Service]
Type=oneshot
EnvironmentFile=/etc/gh-runner-scaler/env
ExecStart=/usr/local/lib/gh-runner-scaler/refresh-runner-template.sh gh-runner-template
User=root
Nice=10
IOSchedulingClass=best-effort
IOSchedulingPriority=7
```

- [ ] **Step 2: Add the daily timer.**

Use:

```ini
[Unit]
Description=Daily GitHub Actions runner distribution check

[Timer]
OnCalendar=*-*-* 04:15:00
RandomizedDelaySec=30m
Persistent=true
Unit=gh-runner-distribution-refresh.service

[Install]
WantedBy=timers.target
```

- [ ] **Step 3: Extend deployment.**

`deploy/update-server.sh` must install:

- the scaler binary;
- the daemon unit;
- both distribution refresh units;
- `refresh-runner-template.sh` under `/usr/local/lib/gh-runner-scaler/`;
- cache/state directories with root ownership;
- nodev2 config only through `GH_RUNNER_SCALER_CONFIG_SOURCE`.

Run `systemctl daemon-reload`, enable the timer, and restart only the daemon. Do not invoke a network download during ordinary binary deployment.

- [ ] **Step 4: Verify and commit.**

```bash
bash -n deploy/update-server.sh deploy/apply-nodev2-config.sh
systemd-analyze verify \
  deploy/systemd/gh-runner-scaler.service \
  deploy/systemd/gh-runner-distribution-refresh.service \
  deploy/systemd/gh-runner-distribution-refresh.timer
git diff --check
git add deploy/systemd deploy/update-server.sh deploy/apply-nodev2-config.sh
git commit -m "feat: schedule daily runner cache refresh"
```

### Task 11: Make nodev2 configuration the authoritative policy

**Files:**

- Modify: `deploy/nodev2.config.toml`
- Modify: `internal/config/config_test.go`

- [ ] **Step 1: Strengthen the nodev2 config contract test.**

Assert:

- there are still seven expected classes;
- exactly one baseline exists;
- it is class `node`;
- its target is `Cordtus/poolbet`;
- its name is `gh-runner-primary`;
- browser and Foundry have no baseline;
- distribution cache is enabled;
- metrics interval is 60 seconds;
- queue audit is five minutes;
- every class uses `gh-runner-template`;
- every runner work directory is `/home/runner/_work`.

- [ ] **Step 2: Apply the configuration.**

The relevant values must become:

```toml
[scaler]
prefix = "gh-runner-auto"
max_auto_runners = 6
idle_timeout = "10m"
poll_interval = "30s"
queue_audit_interval = "5m"
demand_ttl = "30m"
labels = "self-hosted,linux,x64,nodev2,docker"
runner_work_dir = "/home/runner/_work"

[runner_distribution]
enabled = true
repository = "actions/runner"
platform = "linux-x64"
cache_dir = "/var/lib/gh-runner-scaler/runner-distributions"
version_file = "/var/lib/gh-runner-scaler/runner-distributions/current-version"
check_interval = "24h"
retain_versions = 2

[metrics]
enabled = true
interval = "60s"
collect_workflows = true
workflow_repo_batch_size = 25
collect_host = true
```

For class `node` only:

```toml
baseline = true
baseline_name = "gh-runner-primary"
```

Do not add baseline fields to the other six classes.

- [ ] **Step 3: Verify and commit.**

```bash
GOCACHE=/tmp/gh-runner-scaler-go-cache \
go test ./internal/config -run TestLoad_Nodev2Config -count=1
git diff --check
git add deploy/nodev2.config.toml internal/config/config_test.go
git commit -m "config: set nodev2 single-runner baseline"
```

### Task 12: Expose demand, baseline, overflow, and distribution health

**Files:**

- Modify: `internal/domain/types.go`
- Modify: `internal/daemon/daemon.go`
- Modify: `internal/daemon/metrics_test.go`
- Modify: `provider/loki/metrics.go`
- Modify: `provider/loki/metrics_test.go`
- Modify: `deploy/grafana-dashboard.json`

- [ ] **Step 1: Add metric behavior tests.**

Add these fields to runner metrics:

```go
QueuedJobs            int    `json:"queued_jobs"`
DesiredOverflow       int    `json:"desired_overflow"`
BaselineConfigured    bool   `json:"baseline_configured"`
BaselineOnline        bool   `json:"baseline_online"`
BaselineBusy          bool   `json:"baseline_busy"`
BaselineVersion       string `json:"baseline_version,omitempty"`
DesiredRunnerVersion  string `json:"desired_runner_version,omitempty"`
RunnerVersionCurrent  bool   `json:"runner_version_current"`
```

Tests must assert semantic JSON values for a baseline-idle, baseline-busy-with-queue, and demand-only class. Do not assert arbitrary source text.

- [ ] **Step 2: Add dashboard panels.**

Add:

- queued jobs by `group_id`;
- desired versus actual overflow;
- sole baseline state and busy state;
- installed versus desired runner version;
- scale-up duration and queue-to-ready latency;
- distribution refresh success/failure events;
- managed runner count by baseline versus overflow.

Retain the existing lifecycle panels so the before/after churn change remains visible.

- [ ] **Step 3: Add issue events.**

Emit classified issue events for:

- queue audit failed;
- demand persistence failed;
- release metadata failed;
- digest missing or mismatched;
- template refresh failed;
- baseline rotation deferred because busy;
- baseline repair failed.

Routine unchanged daily checks and zero-demand reconciles are debug events, not warnings.

- [ ] **Step 4: Verify and commit.**

```bash
GOCACHE=/tmp/gh-runner-scaler-go-cache go test ./internal/daemon ./provider/loki -count=1
jq -e . deploy/grafana-dashboard.json >/dev/null
git diff --check
git add internal/domain/types.go internal/daemon provider/loki deploy/grafana-dashboard.json
git commit -m "feat: observe demand-driven runner capacity"
```

### Task 13: Synchronize README and the operator runbook

**Files:**

- Modify: `README.md`
- Create: `docs/runner-orchestration-runbook.md`

- [ ] **Step 1: Replace stale behavior descriptions.**

README must say:

- zero available runners does not imply scale-up;
- queued demand is authoritative;
- exactly one configured baseline may exist;
- overflow is ephemeral;
- registration uses `--disableupdate`;
- the daily cache owns runner updates;
- the template distribution lives at `/opt/actions-runner/current`;
- repo-scoped runners cannot serve unrelated personal repositories;
- all permanent deployment artifacts live in this repository.

Remove the pinned manual `v2.323.0` curl example. Replace it with the refresh command:

```bash
sudo systemctl start gh-runner-distribution-refresh.service
sudo journalctl -u gh-runner-distribution-refresh.service -n 100 --no-pager
```

- [ ] **Step 2: Write the operational runbook.**

Include paste-ready commands for:

- service, timer, and cache status;
- desired and template versions;
- baseline status;
- queued demand and recent scale events;
- forcing a safe daily refresh;
- identifying a digest failure;
- inspecting LXD containers without mutation;
- Grafana/Loki queries;
- controlled baseline rotation;
- rollback.

The rollback section must restore a captured binary/config/unit set, disable the new timer, reload systemd, and restart the prior service. It must warn that rollback re-enables the old warm-runner churn.

- [ ] **Step 3: Verify and commit.**

```bash
rg -n 'v2\.323\.0|no available runners, scaling up|keeps a configurable pool' README.md docs/runner-orchestration-runbook.md
git diff --check
git add README.md docs/runner-orchestration-runbook.md
git commit -m "docs: hand off demand-driven runner operations"
```

Expected: the search returns no stale `v2.323.0` or unconditional warm-pool guidance.

### Task 14: Run full local verification

**Files:** all files changed by Tasks 1–13.

- [ ] **Step 1: Format code.**

```bash
gofmt -w \
  cmd/scaler \
  internal/config \
  internal/daemon \
  internal/demand \
  internal/domain \
  internal/engine \
  internal/iface \
  internal/runnerdist \
  internal/runnerobs \
  provider/github \
  provider/loki \
  provider/lxd
```

- [ ] **Step 2: Run focused behavior suites.**

```bash
GOCACHE=/tmp/gh-runner-scaler-go-cache go test ./internal/demand ./internal/engine ./internal/runnerdist -count=1
GOCACHE=/tmp/gh-runner-scaler-go-cache go test ./internal/daemon ./provider/github ./provider/lxd -count=1
```

- [ ] **Step 3: Run the complete repository checks.**

```bash
GOCACHE=/tmp/gh-runner-scaler-go-cache go test ./... -count=1
GOCACHE=/tmp/gh-runner-scaler-go-cache go vet ./...
bash deploy/refresh-runner-template_test.sh
bash -n deploy/*.sh
jq -e . deploy/grafana-dashboard.json >/dev/null
git diff --check
```

- [ ] **Step 4: Review test value.**

Confirm the tests reject these plausible defects:

- zero-idle capacity recreates a runner without demand;
- a duplicate webhook creates duplicate overflow;
- in-progress jobs remain counted as queued;
- a busy baseline is replaced;
- an overflow runner omits `--ephemeral`;
- any runner omits `--disableupdate`;
- digest mismatch changes the current version;
- unchanged daily checks mutate LXD;
- two classes become baseline;
- an empty class is polled every 30 seconds.

Delete or rewrite any test whose only evidence is an arbitrary file, line, symbol, heading, or unconstrained value.

- [ ] **Step 5: Commit final mechanical corrections.**

```bash
git status --short
git add --update
git commit -m "test: verify demand-driven runner lifecycle"
```

Skip this commit when formatting and verification produced no tracked changes.

## 7. Controlled nodev2 rollout

Do not begin rollout while a self-hosted job is busy.

### 7.1 Capture rollback material

- [ ] Record the deployed version and runtime inventory:

```bash
ssh bv@192.168.0.170 \
  'date -Is; /usr/local/bin/gh-runner-scaler version; systemctl is-active gh-runner-scaler; /snap/bin/lxc list --format compact'
```

- [ ] On nodev2, create a timestamped backup directory under `/var/lib/gh-runner-scaler/backups/` and copy:

```text
/usr/local/bin/gh-runner-scaler
/etc/gh-runner-scaler/config.toml
/etc/systemd/system/gh-runner-scaler.service
```

Record the exact backup path in the deployment log. Do not include `/etc/gh-runner-scaler/env` in command output or commit it.

### 7.2 Deploy without refreshing

- [ ] From the nodev2 checkout of this repository:

```bash
sudo ./deploy/apply-nodev2-config.sh
```

- [ ] Verify:

```bash
systemctl is-active gh-runner-scaler.service
systemctl is-enabled gh-runner-distribution-refresh.timer
systemctl list-timers gh-runner-distribution-refresh.timer --no-pager
curl -fsS http://127.0.0.1:9876/healthz
curl -fsS http://127.0.0.1:9876/statusz | jq .
```

### 7.3 Seed the verified cache and template

- [ ] Start one controlled refresh:

```bash
sudo systemctl start gh-runner-distribution-refresh.service
sudo journalctl -u gh-runner-distribution-refresh.service -n 100 --no-pager
```

- [ ] Verify:

```bash
sudo cat /var/lib/gh-runner-scaler/runner-distributions/current-version
/snap/bin/lxc file pull gh-runner-template/opt/actions-runner/current/.runner-dist-version -
/snap/bin/lxc info gh-runner-template | sed -n '1,12p'
```

Expected: host and template versions match, and the template is stopped.

### 7.4 Converge to one baseline

- [ ] Let existing busy runners finish.
- [ ] Stop and delete only idle containers whose names match configured auto prefixes, using the scaler’s normal deregistration path.
- [ ] Do not delete unrelated LXD instances or manually registered runners.
- [ ] Trigger a reconcile and verify:

```bash
/snap/bin/lxc list --format csv -c ns |
  rg '^gh-runner-' |
  rg -v '^gh-runner-template,'
journalctl -u gh-runner-scaler.service --since '-10 min' --no-pager
```

Expected at idle:

```text
gh-runner-primary,RUNNING
```

There should be no class-specific ephemeral container without queued work.

### 7.5 Functional tests

- [ ] Trigger one ordinary Poolbet Node/Docker job.

Expected:

- queue wait remains near the current 2–3 second warm baseline;
- the job runs on `gh-runner-primary`;
- no overflow is created when the baseline is idle;
- the baseline remains online after completion.

- [ ] While the baseline is busy, trigger a second ordinary Poolbet Node job.

Expected:

- one `gh-runner-node-*` overflow appears;
- it uses the cached runner version without a 236 MB self-update;
- it stops after one job and is deleted;
- the system returns to `gh-runner-primary` only.

- [ ] Trigger one QMKUI job and one Poolbet browser or Foundry job.

Expected:

- only the matching class starts;
- queue-to-ready is at most 45 seconds under normal nodev2 load;
- each runner is ephemeral and disappears after completion;
- no unrelated class starts.

### 7.6 Network and metrics acceptance

- [ ] Query router flow telemetry during the controlled jobs.

Expected:

- no per-clone approximately 236 MB download from GitHub release asset addresses;
- one distribution download only when the cached release actually changed.

- [ ] Observe at least one idle hour and then 24 hours.

Acceptance:

- managed idle state remains exactly one live runner;
- no repeated five-minute scale-down/20-second scale-up cycle;
- no zero-demand `scale_up requested` event;
- runner lifecycle count falls sharply;
- baseline remains online;
- distribution timer runs once daily;
- no class inventories are polled every 30 seconds while completely inactive;
- Grafana reports queued demand and baseline/overflow state correctly.

## 8. Rollback

Rollback is warranted if the baseline cannot stay online, queued webhooks do not create matching overflow, queue audits create incorrect demand, or template refresh cannot preserve the last verified version.

1. Stop the new timer:

```bash
sudo systemctl disable --now gh-runner-distribution-refresh.timer
```

2. Stop the current scaler:

```bash
sudo systemctl stop gh-runner-scaler.service
```

3. Restore the timestamped binary, config, and daemon unit captured before rollout.

4. Reload and start:

```bash
sudo systemctl daemon-reload
sudo systemctl start gh-runner-scaler.service
sudo systemctl --no-pager --full status gh-runner-scaler.service
```

5. Leave verified archives in `/var/lib/gh-runner-scaler/runner-distributions/`; they are inert while the timer is disabled.

6. Verify registered runners and LXD containers before removing any failed-rollout container.

Rollback restores service but also restores the old unconditional warm-runner behavior and its download/churn risk. Record the reason, logs, and exact restored backup path before further work.

## 9. Completion criteria

Implementation is complete only when all of the following are true:

- All runner logic, classes, scripts, units, dashboard definitions, and docs are committed in `/home/cordt/repos/gh-runner-scaler`.
- `deploy/nodev2.config.toml` is the deployed configuration source.
- Exactly one baseline is configured, for Poolbet’s `node` class.
- Empty demand-only classes stay at zero.
- Zero available runners without queued demand never causes scale-up.
- Overflow count is derived from queued demand minus available and provisioning capacity.
- Queued demand survives daemon restart and clears on in-progress/completed events.
- The missed-webhook audit is bounded and runs separately from active maintenance.
- Baseline registration is persistent and uses `--disableupdate`.
- Overflow registration is ephemeral and uses `--disableupdate`.
- A changed runner archive is downloaded once, SHA-256 verified, cached, and atomically installed.
- An unchanged daily check performs no archive download and no LXD mutation.
- A failed refresh preserves the previous cache and template.
- A busy baseline is never rotated.
- Metrics interval is 60 seconds and empty classes are not reconciled every 30 seconds.
- Local tests, vet, shell syntax, systemd verification, dashboard JSON validation, and `git diff --check` pass.
- Controlled Poolbet, QMKUI, and specialised-class workflows pass on nodev2.
- An idle 24-hour observation shows one managed live runner and no replacement churn.
- Router flow telemetry shows no repeated approximately 236 MB runner self-update downloads.

Do not report completion based only on local tests. Live nodev2 convergence, workflow execution, a daily timer result, dashboard evidence, and the idle observation are required.
