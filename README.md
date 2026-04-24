# gh-runner-scaler

Auto-scaler for GitHub Actions self-hosted runners on LXC containers. Clones ephemeral containers from a stopped ZFS template when all runners are busy, tears them down after idle timeout or job completion.

Single Go binary, interface-driven architecture. Every external dependency (LXD, GitHub, Loki) is a swappable module behind an interface -- add new backends (Docker, GitLab, Supabase, gcloud gRPC, etc.) without changing core logic.

## Architecture

```
cmd/scaler/main.go            -- composition root (only file importing providers)
internal/
  iface/                      -- ContainerRuntime, CacheManager, CIProvider,
                                 MetricsBackend, StateStore interfaces
  engine/                     -- scaling logic (depends only on interfaces)
  daemon/                     -- goroutine orchestration, webhook server, metrics loop
  domain/                     -- shared value types
  config/                     -- TOML loader with env var overrides
provider/
  lxd/                        -- ContainerRuntime + CacheManager via LXD API
  github/                     -- CIProvider via go-github
  loki/                       -- MetricsBackend via Loki HTTP push
  fsstate/                    -- StateStore via filesystem timestamps
deploy/
  systemd/gh-runner-scaler.service
  grafana-dashboard.json
config.example.toml
```

Adding a provider: create a `provider/<name>/` package implementing the interface, add one `case` in `cmd/scaler/main.go`. Zero changes to `internal/`.

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

**Webhook** is the primary event driver. `workflow_job.queued` and `workflow_job.completed` events trigger the scaler within 2 seconds (debounced). `push` events to tracked repos trigger cache volume syncs via `lxc exec` on a running container when the push targets that repo's default branch.

**Poll loop** runs every `poll_interval` as a safety net in case a webhook is missed.

The listener also exposes additive read-only health endpoints: `GET /healthz` returns `200 OK`, and `GET /statusz` returns JSON with the latest webhook and reconcile timestamps plus current reconcile state.

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

Set `container.lxd.remote` in `config.toml` to the remote name (e.g. `"nodev2"`). The scaler resolves the address and TLS client certs from the standard LXD config at `~/.config/lxc/`.

Alternatively, set `container.lxd.remote_url` directly and provide cert/key paths via `container.lxd.remote_cert` and `container.lxd.remote_key`.

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

CI runs `gofmt`, `go vet`, `go test`, and `go build` on pushes and pull requests via `.github/workflows/ci.yml`.

---

## Configuration

### Config File (TOML)

Copy `config.example.toml` and edit. Every setting has a sensible default except `ci.org`.

```bash
cp config.example.toml config.toml
```

#### Scaler

| Key | Default | Description |
|-----|---------|-------------|
| `prefix` | `gh-runner-auto` | Container name prefix for auto-scaled runners |
| `max_auto_runners` | `6` | Max ephemeral containers |
| `idle_timeout` | `300s` | Idle time before teardown |
| `poll_interval` | `30s` | How often the reconciler checks state |
| `labels` | `self-hosted,linux,x64` | Runner labels (comma-separated) |
| `runner_work_dir` | `_work` | Working directory passed to `config.sh` |

#### Container

| Key | Default | Description |
|-----|---------|-------------|
| `provider` | `lxd` | Container runtime module (`lxd`) |
| `template` | `gh-runner-template` | Stopped template container to clone |

#### Container (LXD-specific): `[container.lxd]`

| Key | Default | Description |
|-----|---------|-------------|
| `socket` | (LXD default) | Unix socket path; empty uses LXD snap default |
| `remote` | (empty = local) | Named LXD remote from `~/.config/lxc/config.yml` |
| `remote_url` | | Direct HTTPS URL (alternative to named remote) |
| `remote_cert` | | TLS client cert path (if not using standard LXD config) |
| `remote_key` | | TLS client key path |

#### Cache: `[cache]`

| Key | Default | Description |
|-----|---------|-------------|
| `enabled` | `false` | Attach shared ZFS cache volume to each runner |
| `pool` | | ZFS storage pool name |
| `volume` | | ZFS volume name |

Symlinks are configured via `[[cache.symlinks]]` entries:

```toml
[[cache.symlinks]]
source = "/cache/npm"
target = "/home/runner/.npm"
```

Each entry creates a symlink inside the container mapping `target` to `source` on the cache volume.
The cache volume is intentionally shared and long-lived, so new ephemeral runners inherit the accumulated cache state from prior runners instead of starting cold each time.

#### CI: `[ci]`

| Key | Default | Description |
|-----|---------|-------------|
| `provider` | `github` | CI platform module (`github`) |
| `org` | (required) | GitHub organization name |

GitHub-specific settings under `[ci.github]` are currently empty -- token and webhook secret are set via environment variables.

#### Webhook: `[webhook]`

| Key | Default | Description |
|-----|---------|-------------|
| `enabled` | `true` | Run the webhook HTTP listener |
| `port` | `9876` | Listen port |
| `debounce` | `2s` | Collapse rapid events within this window |

Default-branch cache syncs are configured under `[webhook.sync_repos]`:

```toml
[webhook.sync_repos]
"Org/repo-name" = "/cache/path"
```

When a push to the repo's default branch is received for a listed repo, the scaler updates the shared cache checkout inside a running container at the given cache path.

#### Metrics: `[metrics]`

| Key | Default | Description |
|-----|---------|-------------|
| `enabled` | `true` | Push metrics to backend |
| `interval` | `60s` | Collection and push interval |
| `collect_workflows` | `true` | Include recent workflow run durations and outcomes |
| `workflow_repo_batch_size` | `25` | Max repos scanned per workflow-metrics interval (`0` = scan all repos) |
| `collect_host` | `true` | Include container counts and storage pool usage |

Loki-specific settings under `[metrics.loki]` are set via environment variables.

#### State: `[state]`

| Key | Default | Description |
|-----|---------|-------------|
| `provider` | `filesystem` | State tracking module (`filesystem`) |

Filesystem-specific: `[state.filesystem]`

| Key | Default | Description |
|-----|---------|-------------|
| `dir` | `.state` | Directory for per-container timestamp files |

For production, use an absolute path like `/var/lib/gh-runner-scaler/state`.

### Secrets (Environment Variables)

Secrets are **never** stored in the config file. Set them as environment variables or in an env file read by systemd.

| Variable | Required | Purpose |
|----------|----------|---------|
| `GH_SCALER_GITHUB_TOKEN` | Y | GitHub PAT for runner management |
| `GH_WEBHOOK_SECRET` | If webhook enabled | HMAC secret for signature verification |
| `LOKI_PUSH_URL` | If metrics enabled | Grafana Loki push endpoint |
| `LOKI_USERNAME` | If metrics enabled | Loki instance ID |
| `GRAFANA_CLOUD_API_KEY` | If metrics enabled | Loki write API key |

---

## Deploy

### 1. Install binary and config

```bash
sudo cp gh-runner-scaler /usr/local/bin/
sudo mkdir -p /etc/gh-runner-scaler /var/lib/gh-runner-scaler/state
sudo cp config.toml /etc/gh-runner-scaler/config.toml
```

### 2. Create the secrets env file

```bash
sudo tee /etc/gh-runner-scaler/env > /dev/null << 'EOF'
GH_SCALER_GITHUB_TOKEN=ghp_...
GH_WEBHOOK_SECRET=your-webhook-secret
LOKI_PUSH_URL=https://logs-prod-XXX.grafana.net/loki/api/v1/push
LOKI_USERNAME=your-loki-instance-id
GRAFANA_CLOUD_API_KEY=glc_...
EOF
sudo chmod 600 /etc/gh-runner-scaler/env
```

### 3. Install systemd unit

```bash
sudo cp deploy/systemd/gh-runner-scaler.service /etc/systemd/system/
```

The unit reads secrets from `/etc/gh-runner-scaler/env` via `EnvironmentFile=`.

### 4. Remove old services (if upgrading from bash/python version)

```bash
sudo systemctl disable --now gh-runner-scaler.timer 2>/dev/null
sudo systemctl disable --now gh-runner-webhook.service 2>/dev/null
sudo systemctl disable --now gh-runner-metrics.timer 2>/dev/null
sudo systemctl disable --now gh-runner-ui-sync.timer 2>/dev/null
sudo rm -f /etc/systemd/system/gh-runner-scaler.timer
sudo rm -f /etc/systemd/system/gh-runner-webhook.service
sudo rm -f /etc/systemd/system/gh-runner-metrics.service
sudo rm -f /etc/systemd/system/gh-runner-metrics.timer
sudo rm -f /etc/systemd/system/gh-runner-ui-sync.service
sudo rm -f /etc/systemd/system/gh-runner-ui-sync.timer
```

### 5. Start

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now gh-runner-scaler.service
```

### 6. Verify

```bash
sudo systemctl status gh-runner-scaler
journalctl -u gh-runner-scaler -f
```

Expected output on a healthy start:

```
level=INFO msg="daemon started" poll_interval=30s webhook=true metrics=true
level=INFO msg="webhook server listening" addr=:9876
level=INFO msg="runner state" total=1 busy=0 idle=1 auto=0 permanent=1
```

### One-shot test

Verify LXD and GitHub connectivity before enabling the daemon:

```bash
sudo -E ./gh-runner-scaler reconcile --config /etc/gh-runner-scaler/config.toml
```

### Manual / foreground run

For debugging, run the daemon in the foreground:

```bash
set -a; source /etc/gh-runner-scaler/env; set +a
sudo -E /usr/local/bin/gh-runner-scaler daemon --config /etc/gh-runner-scaler/config.toml
```

### File layout after install

```
/usr/local/bin/gh-runner-scaler          -- binary
/etc/gh-runner-scaler/config.toml        -- configuration
/etc/gh-runner-scaler/env                -- secrets (mode 600)
/etc/systemd/system/gh-runner-scaler.service -- systemd unit
/var/lib/gh-runner-scaler/state/         -- container state files
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

Import `deploy/grafana-dashboard.json` into Grafana. Requires a Loki datasource receiving metrics from the scaler. Shows:

- Runner pool state (total, busy, idle, auto-scaled)
- Utilization over time
- Workflow run durations and outcomes
- Container counts and storage pool usage

---

## Design Notes

**ZFS cloning**: The template lives on a ZFS pool. Same-pool clones are metadata-only (~0.4s) vs cross-pool copies (~14s). Template and runners must share a pool. NVMe pools suit the persistent cache volume where sequential write throughput matters more.

**Idle timeout**: `idle_timeout = "300s"` balances warm-runner availability for bursty workloads against resource consumption.

**Concurrency**: All three subsystems run as goroutines in one process. A channel-based trigger with `time.AfterFunc` debounce replaces the bash flock + systemd timer approach. The daemon still allows only one reconcile at a time, but webhook-triggered demand is tracked while that reconcile runs so another pass starts immediately afterward instead of waiting for the next poll tick.

**Orphan detection**: Containers matching the auto-scale prefix but with no registered GitHub runner are cleaned up immediately. This catches containers left behind by crashed scalers, failed `config.sh`, or manual intervention.

**MAC handling**: When cloning the template, the scaler clears inherited `volatile.*.hwaddr` entries so LXD assigns a fresh MAC address. Without this, clones fail to start due to MAC collisions.
