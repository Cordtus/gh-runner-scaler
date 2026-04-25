# Load Testing

This directory contains a durable load-test harness for exercising
`gh-runner-scaler` with a mix of short burst jobs, cache-heavy dependency
installs, browser jobs, and polyglot workloads.

## What It Provides

- `create-test-repo.sh`: seeds a standalone test repo from `repo-template/`
  and can push it to GitHub once `gh` auth is valid.
- `dispatch-load.sh`: dispatches repeatable workload profiles against the test
  repo with different concurrency mixes.
- `collect-server-evidence.sh`: samples LXC and host resource snapshots every
  15 seconds for 15 minutes by default while a load test is in flight, then
  captures the scaler journal once at exit.
- `repo-template/`: a self-contained GitHub Actions repo with representative
  workflows for:
  - short queue bursts
  - cache-heavy Node/TypeScript work
  - Python dependency and CPU work
  - Go test/build loops
  - Playwright browser jobs

## Prerequisites

1. Valid GitHub CLI auth on the machine where you will create and dispatch the
   test repo:

```bash
gh auth login
```

2. A GitHub org/user where the self-hosted runners are visible.

3. The scaler host already running the latest `gh-runner-scaler` build.

## Recommended Test Sequence

1. Seed the test repo:

```bash
./loadtest/create-test-repo.sh Axionic-Labs/runner-load-lab /tmp/runner-load-lab --push
```

2. Start server-side evidence collection on the scaler host. It samples host
   state every 15 seconds for 15 minutes by default, then writes the scaler
   journal once when it exits. Set `DURATION_SECONDS=0` if you want it to keep
   running until `Ctrl+C`:

```bash
./loadtest/collect-server-evidence.sh
```

3. Warm caches:

```bash
./loadtest/dispatch-load.sh Axionic-Labs/runner-load-lab cache-warm
```

4. Run mixed profiles:

```bash
./loadtest/dispatch-load.sh Axionic-Labs/runner-load-lab queue-burst
./loadtest/dispatch-load.sh Axionic-Labs/runner-load-lab steady-polyglot
./loadtest/dispatch-load.sh Axionic-Labs/runner-load-lab browser-spike
./loadtest/dispatch-load.sh Axionic-Labs/runner-load-lab mixed-peak
```

## Profiles

- `cache-warm`: primes Node, Python, Go, and browser dependencies with a
  serialized, low-concurrency pass.
- `queue-burst`: many short jobs with minimal dependency work to stress queue
  depth, runner startup, and cleanup.
- `steady-polyglot`: parallel Node, Python, and Go jobs that keep runners busy
  without an extreme burst.
- `browser-spike`: Playwright-heavy jobs plus a light burst to expose image and
  browser provisioning costs.
- `mixed-peak`: intentionally overlaps burst, dependency, and browser workflows
  to create a more production-like mixed load.

## Evidence Collection Tuning

- Override `INTERVAL_SECONDS` to sample more or less frequently.
- Override `DURATION_SECONDS` to change the capture window.
- Set `DURATION_SECONDS=0` to keep sampling until you stop the script.
- Snapshot data is written under `snapshots/`; the scaler journal is written to
  `journalctl.log` when the script exits.

## What To Look For

- Time from `workflow_job.queued` to runner availability.
- Scale-up success/failure rate and cleanup behavior.
- Reuse quality of shared caches for npm, pip, hosted toolcache, and browser
  assets.
- Whether browser workloads deserve a separate base template from general CI.
- Whether short burst jobs and heavy dependency jobs interfere with each other
  enough to justify different labels or template classes.

## Tuning Hypotheses To Validate

- Add more shared-cache symlinks for workload-specific paths that are not
  currently covered, especially:
  - `/home/runner/.cache/ms-playwright`
  - `/home/runner/.cache/go-build`
  - `/home/runner/go/pkg/mod`
  - `/home/runner/.pnpm-store`
  - `/home/runner/.cache/pypoetry`
- Split the base runner image if browser-heavy jobs slow down general CI:
  - a lean general-purpose template
  - a browser template with Playwright browsers and extra UI dependencies
- Consider separate runner labels for bursty short jobs versus dependency-heavy
  jobs if mixed peaks create queueing or cache-thrash behavior.
