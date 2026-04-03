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

**Scale-down** handles four cases in priority order:

1. Stopped containers -- ephemeral job complete, immediate cleanup
2. Running containers with no registered runner -- orphaned, immediate cleanup
3. Running, registered, busy -- refresh last-active timestamp
4. Running, registered, idle past `idle_timeout` -- teardown

Deregistration is belt-and-suspenders: `config.sh remove` followed by a GitHub API DELETE.

**Webhook** is the primary event driver. `workflow_job.queued` triggers the scaler within 2 seconds (debounced). `push` events to tracked repos trigger cache volume syncs via `lxc exec` on a running container.

**Poll loop** runs every `poll_interval` as a safety net in case a webhook is missed.

---

## Environment Prerequisites

The binary is statically compiled with no runtime dependencies. The host needs:

| Dependency | Required | Purpose |
|------------|----------|---------|
| LXD (snap) | Y | Container runtime |
| ZFS | Y | Fast same-pool clones for scale-up |
| GitHub classic PAT | Y | Runner management, webhook events |
| Network access from GitHub | If webhook enabled | Receives `workflow_job` and `push` events |
| Grafana Cloud (Loki) | If metrics enabled | Dashboard visualization |

### LXD

```bash
sudo snap install lxd
sudo lxd init
```

The `lxd init` wizard configures storage and networking. Key choices:

- **Storage backend**: Select ZFS. If the host has existing ZFS pools, point LXD at one. Otherwise let the wizard create a pool.
- **Network bridge**: Accept default `lxdbr0` unless your network requires otherwise.
- **Clustering**: Not required.

Verify:

```bash
lxc list
```

#### Remote LXD (optional)

If the scaler runs on a different machine than LXD:

```bash
# On the LXD host
lxc config set core.https_address :8443

# On the scaler machine
lxc remote add <name> <host>:8443
```

The scaler reads the remote name from `container.lxd.remote` in `config.toml` and resolves the address + TLS certs from the standard LXD client config at `~/.config/lxc/`.

### ZFS Storage Pools

Same-pool ZFS clones are metadata-only (~0.4s). Cross-pool copies require full data transfer (~14s). The template and its clones **must share a pool**.

```bash
# Create a pool if needed
sudo zpool create <pool-name> <device>
sudo zpool create <pool-name> raidz <dev1> <dev2> <dev3> <dev4>

# Register with LXD
lxc storage create <pool-name> zfs source=<pool-name>
```

For the persistent cache volume (optional), a separate NVMe pool works well:

```bash
lxc storage volume create <cache-pool> <cache-volume>
```

### Template Container

A stopped LXC container with the GitHub Actions runner software pre-installed. Every ephemeral runner is cloned from it.

```bash
lxc launch ubuntu:24.04 gh-runner-template
lxc exec gh-runner-template -- bash
```

Inside the container:

```bash
# Base dependencies
apt-get update && apt-get install -y curl git jq build-essential

# Runner software (check github.com/actions/runner/releases for latest)
mkdir -p /home/runner && cd /home/runner
curl -o actions-runner.tar.gz -L \
  https://github.com/actions/runner/releases/download/v2.323.0/actions-runner-linux-x64-2.323.0.tar.gz
tar xzf actions-runner.tar.gz && rm actions-runner.tar.gz
./bin/installdependencies.sh

# Runner user
useradd -m -d /home/runner -s /bin/bash runner
chown -R runner:runner /home/runner

# Install additional tools your workflows need (node, python, etc.)
```

Then exit and stop:

```bash
exit
lxc stop gh-runner-template
```

Do **not** run `config.sh` on the template -- each ephemeral clone configures itself with a fresh registration token.

### GitHub PAT

Create a classic Personal Access Token at https://github.com/settings/tokens:

| Scope | Purpose |
|-------|---------|
| `repo` | Read workflow job status |
| `manage_runners:org` | Register and deregister runners |
| `admin:org_hook` | Create org webhooks (initial setup only) |

### Webhook Network Access

The host must be reachable from GitHub on the webhook port (default 9876).

Options:
- Direct port exposure (host has a public IP or port forward)
- Reverse proxy (nginx, caddy) with TLS termination
- Tunnel (Cloudflare Tunnel, ngrok, etc.)

GitHub publishes webhook source IPs via the [meta API](https://api.github.com/meta) under the `hooks` key.

### Grafana Cloud (optional)

For the metrics + dashboard integration:

| Value | Source |
|-------|--------|
| Loki push URL | Grafana Cloud > your stack > Loki > Details |
| Loki username | Instance ID shown on the same page |
| API key | Grafana Cloud > API Keys > create with Loki write scope |

---

## Build

Requires Go 1.23+.

```bash
go build -o gh-runner-scaler ./cmd/scaler/
```

Cross-compile:

```bash
GOOS=linux GOARCH=amd64 go build -o gh-runner-scaler ./cmd/scaler/
```

---

## Configuration

### Config File (TOML)

Copy `config.example.toml` to `config.toml` and edit. Every setting has a sensible default except `ci.org` which must be set.

```bash
cp config.example.toml config.toml
```

| Section | Key | Default | Description |
|---------|-----|---------|-------------|
| `scaler` | `prefix` | `gh-runner-auto` | Container name prefix |
| `scaler` | `max_auto_runners` | `6` | Max ephemeral containers |
| `scaler` | `idle_timeout` | `300s` | Idle time before teardown |
| `scaler` | `poll_interval` | `30s` | Reconciler poll frequency |
| `scaler` | `labels` | `self-hosted,linux,x64` | Runner labels |
| `container` | `provider` | `lxd` | Container runtime module |
| `container` | `template` | `gh-runner-template` | Stopped template to clone |
| `container.lxd` | `remote` | (empty = local) | LXD remote name from `~/.config/lxc/` |
| `cache` | `enabled` | `false` | Attach shared cache volume |
| `cache` | `pool` | | ZFS pool for cache volume |
| `cache` | `volume` | | ZFS volume name |
| `ci` | `provider` | `github` | CI platform module |
| `ci` | `org` | | GitHub org name |
| `webhook` | `enabled` | `true` | Run webhook HTTP listener |
| `webhook` | `port` | `9876` | Listen port |
| `webhook` | `debounce` | `2s` | Collapse rapid events |
| `webhook.sync_repos` | | | Repo -> cache path map for push-triggered syncs |
| `metrics` | `enabled` | `true` | Push metrics to backend |
| `metrics` | `interval` | `60s` | Collection interval |
| `metrics` | `collect_workflows` | `true` | Include workflow run data |
| `metrics` | `collect_host` | `true` | Include container/storage data |
| `state` | `provider` | `filesystem` | State tracking module |
| `state.filesystem` | `dir` | `.state` | Directory for timestamp files |

### Secrets (Environment Variables)

Secrets are **never** stored in the config file. Set them as environment variables.

| Variable | Required | Purpose |
|----------|----------|---------|
| `GH_SCALER_GITHUB_TOKEN` | Y | GitHub PAT for runner management |
| `GH_WEBHOOK_SECRET` | If webhook enabled | HMAC secret for webhook signature verification |
| `LOKI_PUSH_URL` | If metrics enabled | Grafana Loki push endpoint |
| `LOKI_USERNAME` | If metrics enabled | Loki instance ID |
| `GRAFANA_CLOUD_API_KEY` | If metrics enabled | Loki write API key |

### Cache Symlinks

When `cache.enabled = true`, the scaler mounts a shared ZFS volume at `/cache` in every ephemeral container and creates symlinks from standard tool paths. Configure via `[[cache.symlinks]]` entries:

```toml
[[cache.symlinks]]
source = "/cache/npm"
target = "/home/runner/.npm"

[[cache.symlinks]]
source = "/cache/tool-cache"
target = "/opt/hostedtoolcache"
```

This eliminates cold caches on every job without sacrificing ephemeral isolation.

---

## Deploy

### Systemd (recommended)

```bash
# Copy binary
sudo cp gh-runner-scaler /usr/local/bin/

# Copy config
sudo mkdir -p /etc/gh-runner-scaler
sudo cp config.toml /etc/gh-runner-scaler/

# Install service unit
sudo cp systemd/gh-runner-scaler.service /etc/systemd/system/

# Set secrets via systemd override
sudo systemctl edit gh-runner-scaler
```

In the override editor, add:

```ini
[Service]
Environment=GH_SCALER_GITHUB_TOKEN=ghp_...
Environment=GH_WEBHOOK_SECRET=...
Environment=LOKI_PUSH_URL=https://...
Environment=LOKI_USERNAME=...
Environment=GRAFANA_CLOUD_API_KEY=glc_...
```

Then start:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now gh-runner-scaler
journalctl -u gh-runner-scaler -f
```

### Manual / testing

Source secrets from an env file and run directly:

```bash
set -a; source .env; set +a
sudo -E ./gh-runner-scaler daemon --config config.toml
```

### One-shot test

Verify connectivity before enabling the daemon:

```bash
sudo -E ./gh-runner-scaler reconcile --config config.toml
```

This runs a single reconcile pass and exits. Expected output:

```
level=INFO msg="runner state" total=1 busy=0 idle=1 auto=0 permanent=1
```

---

## CLI

```
gh-runner-scaler daemon      # run all subsystems (default)
gh-runner-scaler reconcile   # one-shot scale check
gh-runner-scaler version     # print version
```

`daemon` and `reconcile` accept `--config <path>` (default: `config.toml`).

---

## Grafana Dashboard

Import `grafana-dashboard.json` into Grafana. Requires a Loki datasource receiving metrics from the scaler. Shows:

- Runner pool state (total, busy, idle, auto-scaled)
- Utilization over time
- Workflow run durations and outcomes
- Container counts and storage pool usage

---

## Design Notes

**ZFS cloning**: The template lives on a ZFS pool. Same-pool clones are metadata-only (~0.4s) vs cross-pool copies (~14s). Template and runners must share a pool. NVMe pools suit the persistent cache volume where sequential write throughput matters more.

**Idle timeout**: `idle_timeout = "300s"` balances warm-runner availability for bursty workloads against resource consumption.

**Concurrency**: Reconciler, webhook, and metrics run as goroutines in one process. A channel-based trigger with `time.AfterFunc` debounce replaces the bash flock + systemd timer approach. `sync.Mutex` with `TryLock` prevents concurrent reconciles -- if one is running, the next trigger is skipped (correct because the running pass already sees the latest state).

**Orphan detection**: Containers matching the auto-scale prefix but with no registered GitHub runner are cleaned up immediately. This catches containers left behind by crashed scalers, failed `config.sh`, or manual intervention.
