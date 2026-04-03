# gh-runner-scaler

Auto-scaler for GitHub Actions self-hosted runners on LXC containers. Clones ephemeral containers from a stopped ZFS template when all runners are busy, tears them down after idle timeout or job completion.

Single Go binary, interface-driven architecture. Every external dependency (LXD, GitHub, Loki) is a swappable module behind an interface -- add new backends (Docker, GitLab, Supabase, gcloud gRPC, etc.) without changing core logic.

## Architecture

```
cmd/scaler/main.go         -- composition root (only file importing providers)
internal/
  iface/                   -- ContainerRuntime, CacheManager, CIProvider,
                              MetricsBackend, StateStore interfaces
  engine/                  -- scaling logic (depends only on interfaces)
  daemon/                  -- goroutine orchestration, webhook server, metrics loop
  domain/                  -- shared value types
  config/                  -- TOML loader with env var overrides
provider/
  lxd/                     -- ContainerRuntime + CacheManager via LXD API
  github/                  -- CIProvider via go-github
  loki/                    -- MetricsBackend via Loki HTTP push
  fsstate/                 -- StateStore via filesystem timestamps
```

Adding a provider: create a `provider/<name>/` package implementing the interface, add one `case` in `main.go`. Zero changes to `internal/`.

## Runner Lifecycle

```
clone template -> start -> wait for boot (90s max)
  -> config.sh --ephemeral -> svc.sh install+start
  -> [runs one job] -> container stops -> scaler cleanup
```

Scale-down handles three cases: stopped ephemeral containers (immediate), idle runners past `idle_timeout`, and orphaned containers. Deregistration is belt-and-suspenders: `config.sh remove` followed by an API DELETE.

The webhook is the primary event driver. When GitHub fires a `workflow_job.queued` event, the scaler runs within 2 seconds (debounced to collapse concurrent bursts). Push events to tracked repos trigger cache volume syncs via `lxc exec` on a running container.

## Prerequisites

- LXC/LXD (snap install)
- Go 1.23+ (build only)
- A stopped LXC container with the GitHub Actions runner software installed (the template)
- GitHub classic PAT with `repo`, `manage_runners:org`, and `admin:org_hook` scopes

## Build

```bash
go build -o gh-runner-scaler ./cmd/scaler/
```

## Setup

```bash
# 1. Create config from template
cp config.example.toml config.toml
# Edit config.toml -- set org, template, cache settings

# 2. Install binary and systemd unit
sudo cp gh-runner-scaler /usr/local/bin/
sudo mkdir -p /etc/gh-runner-scaler
sudo cp config.toml /etc/gh-runner-scaler/
sudo cp systemd/gh-runner-scaler.service /etc/systemd/system/

# 3. Set secrets in the service file
sudo systemctl edit gh-runner-scaler
# Add Environment= lines for GH_SCALER_GITHUB_TOKEN, GH_WEBHOOK_SECRET,
# LOKI_PUSH_URL, LOKI_USERNAME, GRAFANA_CLOUD_API_KEY

# 4. Start
sudo systemctl enable --now gh-runner-scaler
```

## CLI

```
gh-runner-scaler daemon      # run all subsystems (default)
gh-runner-scaler reconcile   # one-shot scale check
gh-runner-scaler version     # print version
```

Both `daemon` and `reconcile` accept `--config <path>` (default: `config.toml`).

## Configuration

TOML config with env var overrides for secrets. See `config.example.toml` for all options.

| Section | Key | Default | Description |
|---------|-----|---------|-------------|
| `scaler` | `prefix` | `gh-runner-auto` | Container name prefix for auto-scaled runners |
| `scaler` | `max_auto_runners` | `6` | Cap on ephemeral containers |
| `scaler` | `idle_timeout` | `300s` | Time before idle runner teardown |
| `scaler` | `poll_interval` | `30s` | Reconciler poll frequency (webhook bypasses this) |
| `scaler` | `labels` | `self-hosted,linux,x64` | Runner labels |
| `container` | `provider` | `lxd` | Container runtime module |
| `container` | `template` | `gh-runner-template` | Stopped LXC template to clone |
| `cache` | `enabled` | `false` | Mount shared cache volume |
| `cache` | `pool` / `volume` | | ZFS pool and volume for cache |
| `ci` | `provider` | `github` | CI platform module |
| `ci` | `org` | | GitHub org name |
| `webhook` | `enabled` | `true` | Run webhook HTTP listener |
| `webhook` | `port` | `9876` | Listen port |
| `webhook.sync_repos` | | | Map of repo -> cache path for push-triggered syncs |
| `metrics` | `enabled` | `true` | Push metrics to backend |
| `metrics` | `interval` | `60s` | Metrics collection interval |
| `state` | `provider` | `filesystem` | State tracking module |

**Secrets** (env vars, not in config file): `GH_SCALER_GITHUB_TOKEN`, `GH_WEBHOOK_SECRET`, `LOKI_PUSH_URL`, `LOKI_USERNAME`, `GRAFANA_CLOUD_API_KEY`.

## Persistent Cache

When `cache.enabled = true`, the scaler attaches a shared ZFS volume to every ephemeral container at `/cache` and creates symlinks mapping standard tool paths into the volume:

```
/cache/npm         -> ~/.npm
/cache/yarn        -> ~/.cache/yarn
/cache/pip         -> ~/.cache/pip
/cache/tool-cache  -> /opt/hostedtoolcache
/cache/axionic-ui  -> /opt/axionic-ui
```

Symlinks are configurable via `[[cache.symlinks]]` entries in the TOML config.

## Grafana Dashboard

Import `grafana-dashboard.json` into Grafana. Requires a Loki datasource. The dashboard shows runner pool state, utilization trends, workflow run durations/outcomes, and host metrics.

## Design Notes

The template container lives on a ZFS RAIDZ1 pool. Same-pool ZFS clones are metadata-only operations (~0.4s) compared to cross-pool copies (~14s), so keeping the template co-located with the runner pool is critical for scale-up latency. NVMe pools are better suited for the persistent cache volume where sequential write throughput matters more than clone speed.

The 5-minute idle timeout (`idle_timeout = "300s"`) balances keeping warm runners available for bursty workloads against resource consumption. The webhook provides sub-second scale-up response for queued jobs regardless of the poll interval.

All three subsystems (reconciler, webhook, metrics) run as goroutines in a single process, coordinated by context cancellation and a channel-based trigger mechanism. A mutex with TryLock replaces flock -- if a reconcile is already running when a second trigger arrives, the second is skipped.
