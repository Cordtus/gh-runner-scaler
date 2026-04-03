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

## Host Environment Setup

The scaler binary is statically compiled with no runtime dependencies, but the host it runs on needs LXD, storage, and a runner template container configured before first use.

### 1. LXD

Install and initialize LXD. The `init` wizard sets up storage pools and networking.

```bash
sudo snap install lxd
sudo lxd init
```

During `lxd init`, the relevant choices:

- **Storage backend**: ZFS is required for fast same-pool clones. If the host already has ZFS pools, select "use existing" and point LXD at one. Otherwise, let the wizard create a new pool.
- **Network bridge**: Accept the default `lxdbr0` unless you need a specific network layout.
- **Clustering**: Not required. Single-node is fine.

Verify LXD is working:

```bash
lxc list
```

If running the scaler on a different machine than LXD, configure a remote:

```bash
# On the LXD host: expose the API
lxc config set core.https_address :8443
lxc config trust add <client-cert>

# On the scaler machine: add the remote
lxc remote add <name> <host>:8443
```

Then set `container.lxd.remote` and `container.lxd.remote_url` in `config.toml`.

### 2. Storage Pools

The scaler clones containers via ZFS. Same-pool clones are metadata-only (~0.4s); cross-pool copies require full data transfer (~14s). The template container and its clones **must live on the same pool**.

If you need to create a pool manually:

```bash
# ZFS on a block device
sudo zpool create <pool-name> <device>       # single disk
sudo zpool create <pool-name> raidz <dev1> <dev2> <dev3> <dev4>  # RAIDZ1

# Register it with LXD
lxc storage create <pool-name> zfs source=<pool-name>
```

For the persistent cache volume (optional), a separate NVMe pool works well since cache workloads favor sequential write throughput over clone speed:

```bash
lxc storage volume create <cache-pool> <cache-volume>
```

### 3. Template Container

The template is a stopped LXC container with the GitHub Actions runner software pre-installed. Every ephemeral runner is cloned from it.

```bash
# Launch a base container
lxc launch ubuntu:24.04 gh-runner-template

# Enter it
lxc exec gh-runner-template -- bash

# Inside the container: install runner prerequisites
apt-get update && apt-get install -y curl git jq build-essential

# Download and extract the runner (check github.com/actions/runner/releases for latest)
mkdir -p /home/runner && cd /home/runner
curl -o actions-runner.tar.gz -L https://github.com/actions/runner/releases/download/v2.323.0/actions-runner-linux-x64-2.323.0.tar.gz
tar xzf actions-runner.tar.gz && rm actions-runner.tar.gz
./bin/installdependencies.sh

# Create the runner user and fix ownership
useradd -m -d /home/runner -s /bin/bash runner
chown -R runner:runner /home/runner

# Install any additional tools your workflows need (node, python, etc.)
# ...

# Exit and stop the container
exit
lxc stop gh-runner-template
```

Verify the template is stopped and on the correct storage pool:

```bash
lxc list gh-runner-template
lxc storage volume list <pool-name> | grep gh-runner-template
```

Do **not** run `config.sh` on the template -- each ephemeral clone configures itself at boot with a fresh registration token.

### 4. GitHub PAT

Create a classic Personal Access Token at https://github.com/settings/tokens with these scopes:

- `repo` -- read workflow job status
- `manage_runners:org` -- register/deregister runners
- `admin:org_hook` -- create org webhooks (only needed for initial webhook setup)

### 5. Grafana Cloud (optional)

If using metrics, create a Grafana Cloud API key with Loki write access. You need:

- **Push URL**: e.g. `https://logs-prod-042.grafana.net/loki/api/v1/push`
- **Username**: your Grafana Cloud Loki instance ID
- **API key**: a cloud API key or service account token with write permissions

### 6. Webhook Network Access

The host must be reachable from GitHub's webhook IPs on the configured port (default 9876). Options:

- Direct port exposure if the host has a public IP
- Reverse proxy (nginx/caddy) with TLS termination
- Cloudflare Tunnel or similar

GitHub publishes its webhook IP ranges via the [meta API](https://api.github.com/meta) under the `hooks` key.

## Build

```bash
go build -o gh-runner-scaler ./cmd/scaler/
```

Requires Go 1.23+. The output is a statically linked binary -- copy it to the target host.

Cross-compile for a different architecture:

```bash
GOOS=linux GOARCH=amd64 go build -o gh-runner-scaler ./cmd/scaler/
```

## Deploy

```bash
# 1. Copy binary and config
sudo cp gh-runner-scaler /usr/local/bin/
sudo mkdir -p /etc/gh-runner-scaler
cp config.example.toml config.toml
# Edit config.toml -- set org, template, cache settings, etc.
sudo cp config.toml /etc/gh-runner-scaler/

# 2. Install systemd unit
sudo cp systemd/gh-runner-scaler.service /etc/systemd/system/

# 3. Set secrets
sudo systemctl edit gh-runner-scaler
# In the override file, add:
#   [Service]
#   Environment=GH_SCALER_GITHUB_TOKEN=ghp_...
#   Environment=GH_WEBHOOK_SECRET=your-secret
#   Environment=LOKI_PUSH_URL=https://...
#   Environment=LOKI_USERNAME=...
#   Environment=GRAFANA_CLOUD_API_KEY=glc_...

# 4. Start
sudo systemctl daemon-reload
sudo systemctl enable --now gh-runner-scaler

# 5. Verify
journalctl -u gh-runner-scaler -f
```

### One-shot test

Before enabling the daemon, verify the scaler can talk to LXD and GitHub:

```bash
sudo GH_SCALER_GITHUB_TOKEN=ghp_... ./gh-runner-scaler reconcile --config config.toml
```

This runs a single scale check and exits. It should log the current runner state without errors.

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
