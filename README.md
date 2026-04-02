# gh-runner-scaler

Auto-scaler for GitHub Actions self-hosted runners on LXC containers. Clones ephemeral containers from a stopped ZFS template when all runners are busy, tears them down after idle timeout or job completion. Built for the Axionic-Labs org.

## Components

Three systemd services run on the runner host:

| Service | Description | Schedule |
|---------|-------------|----------|
| `gh-runner-scaler` | Polls GitHub API, scales containers up/down | Every 30s (timer) |
| `gh-runner-webhook` | Listens for `workflow_job` events, triggers scaler immediately | Persistent (long-running) |
| `gh-runner-metrics` | Pushes runner state to Grafana Cloud Loki | Every 60s (timer) |

An optional `gh-runner-ui-sync` timer keeps a shared `axionic-ui` checkout current on the cache volume.

## Runner Lifecycle

```
lxc copy template -> lxc start -> wait for boot (90s max)
  -> config.sh --ephemeral -> svc.sh install+start
  -> [runs one job] -> container stops -> scaler cleanup
```

Scale-down handles three cases: stopped ephemeral containers (immediate), idle runners past `IDLE_TIMEOUT`, and orphaned containers. Deregistration is belt-and-suspenders: `config.sh remove` followed by an API DELETE.

The webhook bypasses poll latency -- when GitHub fires a `workflow_job.queued` event, the scaler runs within 2 seconds (debounced to collapse concurrent bursts).

## Prerequisites

- LXC/LXD (snap install)
- `jq`, `curl`
- Python 3 (stdlib only for webhook; `requests` for metrics)
- A stopped LXC container with the GitHub Actions runner software installed (the template)
- GitHub classic PAT with `repo`, `manage_runners:org`, and `admin:org_hook` scopes

## Setup

```bash
# 1. Create config from template
cp config.example config
# Edit config -- set GITHUB_TOKEN at minimum

# 2. Install systemd units (scaler timer + ui-sync timer)
sudo ./install.sh

# 3. Install webhook + metrics manually (they need secrets in Environment= lines)
# Edit gh-runner-webhook.service: set GH_WEBHOOK_SECRET
# Edit gh-runner-metrics.service: set GRAFANA_CLOUD_API_KEY, LOKI_PUSH_URL, LOKI_USERNAME
sudo cp gh-runner-webhook.service gh-runner-metrics.service gh-runner-metrics.timer /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now gh-runner-webhook.service gh-runner-metrics.timer

# 4. Create the org webhook (webhook listener must be reachable)
./setup-webhook.sh https://your-host:9876 your-webhook-secret
```

## Configuration

All scaler config lives in a `config` file (bash key=value). See `config.example` for defaults and descriptions.

| Variable | Default | Description |
|----------|---------|-------------|
| `GITHUB_TOKEN` | (required) | Classic PAT with org runner scopes |
| `TEMPLATE` | `gh-runner-template` | Stopped LXC container to clone from |
| `ORG` | `Axionic-Labs` | GitHub organization |
| `PREFIX` | `gh-runner-auto` | Name prefix for auto-scaled containers |
| `MAX_AUTO_RUNNERS` | `6` | Cap on ephemeral containers |
| `IDLE_TIMEOUT` | `300` | Seconds before idle runner teardown |
| `LABELS` | `self-hosted,linux,x64` | Runner labels (comma-separated) |
| `LXC_REMOTE` | (empty) | LXC remote name; empty = local host |
| `CACHE_POOL` | (empty) | ZFS pool for persistent cache volume |
| `CACHE_VOLUME` | (empty) | ZFS volume name for shared cache |

Webhook and metrics secrets are set via `Environment=` in their respective `.service` files, not in the config file.

## Persistent Cache

When `CACHE_POOL` and `CACHE_VOLUME` are set, the scaler attaches a shared ZFS volume to every ephemeral container at `/cache`. Symlinks map standard tool paths into the volume:

- `/cache/npm` -> `~/.npm`
- `/cache/yarn` -> `~/.cache/yarn`
- `/cache/pip` -> `~/.cache/pip`
- `/cache/tool-cache` -> `/opt/hostedtoolcache`
- `/cache/axionic-ui` -> `/opt/axionic-ui`

This eliminates cold caches on every job without sacrificing ephemeral container isolation.

## Grafana Dashboard

Import `grafana-dashboard.json` into Grafana. Requires a Loki datasource receiving the metrics pushed by `metrics.py`. The dashboard shows total/busy/idle/auto-scaled runner counts, utilization over time, and a scaler event log.

## Design Notes

The template container lives on a ZFS RAIDZ1 pool. Same-pool ZFS clones are metadata-only operations (~0.4s) compared to cross-pool copies (~14s), so keeping the template co-located with the runner pool is critical for scale-up latency. NVMe pools are better suited for the persistent cache volume where sequential write throughput matters more than clone speed.

The 5-minute idle timeout (`IDLE_TIMEOUT=300`) balances keeping warm runners available for bursty workloads against resource consumption. The webhook provides sub-second scale-up response for queued jobs regardless of the poll interval.
