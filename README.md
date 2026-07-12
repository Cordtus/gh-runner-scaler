# gh-runner-scaler

Auto-scaler for GitHub Actions self-hosted runners on LXC containers. It keeps a configurable pool of runners, clones stopped LXD template containers when more capacity is needed, and tears managed runners down after job completion or an idle timeout.

## Deploy Your Own Instance

Use this path for a first deployment. The detailed reference sections below explain each part.

1. Prepare an Ubuntu Linux host with LXD, ZFS storage, Git, curl, and the Go version from `go.mod`.
2. Create a stopped LXD template named `gh-runner-template` with the GitHub Actions runner files installed under `/home/runner`.
3. Create a GitHub token with runner-management access for your target organization or repository.
4. Copy `config.example.toml` to `config.toml`, then set exactly one GitHub target:
   - `ci.org = "YourOrg"` for organization-scoped runners
   - `ci.repo = "owner/repo"` for one repository-scoped runner target
5. Keep `[metrics].enabled = false` until you have a Loki push endpoint. Metrics are optional.
6. Build and install the service:

```bash
go build -o gh-runner-scaler ./cmd/scaler/
sudo install -D -m 0755 gh-runner-scaler /usr/local/bin/gh-runner-scaler
sudo install -d /etc/gh-runner-scaler /var/lib/gh-runner-scaler/state
sudo install -m 0644 config.toml /etc/gh-runner-scaler/config.toml
sudo install -m 0644 deploy/systemd/gh-runner-scaler.service /etc/systemd/system/gh-runner-scaler.service
```

7. Create `/etc/gh-runner-scaler/env` with the required secrets:

```bash
sudo install -m 0600 /dev/null /etc/gh-runner-scaler/env
sudoedit /etc/gh-runner-scaler/env
```

At minimum:

```text
GH_SCALER_GITHUB_TOKEN=...
GH_WEBHOOK_SECRET=...
```

8. Run one active reconcile before enabling the daemon:

```bash
sudo sh -c 'set -a; . /etc/gh-runner-scaler/env; set +a; exec /usr/local/bin/gh-runner-scaler reconcile --config /etc/gh-runner-scaler/config.toml'
```

`reconcile` is not a dry-run. It may create or clean up managed runners and LXD containers according to the configured target, labels, and scale limits.

9. Start and verify:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now gh-runner-scaler.service
sudo systemctl --no-pager --full status gh-runner-scaler.service
curl http://127.0.0.1:9876/healthz
```

10. Add the same labels from your config to workflow jobs, for example:

```yaml
runs-on: [self-hosted, linux, x64, runner-class-default]
```

For this repository, the default is:

```yaml
runs-on: [self-hosted, linux, x64, runner-class-gh-runner-scaler]
```

This repo currently provisions nodev2-based runners, so include those labels as well:

```yaml
runs-on: [self-hosted, linux, x64, nodev2, docker, runner-class-gh-runner-scaler]
```

## Nodev2 Targets

The checked-in nodev2 target config lives at `deploy/nodev2.config.toml`. It
serves `CAC-Group` as an organization-scoped target and `Cordtus/the-clearooor`
as a repo-scoped personal target. It also includes a dedicated repo-scoped class
for `Cordtus/gh-runner-scaler` so this repository can use scaler runners
without extra workflow wiring. No former customer organizations are configured
for this deployment.

GitHub does not provide one self-hosted runner pool for every repository owned
by a personal user account. Personal repositories must be added as repo-scoped
runner classes, one `owner/name` repository at a time. Use the `the-clearooor`
class in `deploy/nodev2.config.toml` as the template for additional
`Cordtus/*` projects.

The live service reads `/etc/gh-runner-scaler/config.toml`; after changing the
tracked nodev2 config, install it to that path and restart
`gh-runner-scaler.service`.

```bash
GH_RUNNER_SCALER_CONFIG_SOURCE=deploy/nodev2.config.toml ./deploy/update-server.sh
```

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
  grafana-dashboard.json          -- repo-maintained dashboard baseline
  grafana-dashboard-old.json      -- exported/reference dashboard snapshot
loadtest/                         -- synthetic workload repo + capacity tools
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

Deregistration uses two cleanup paths: `config.sh remove` inside the runner, then a GitHub API DELETE when GitHub no longer reports the runner as busy. Cleanup keeps going after service-stop or deregistration errors because deleting the container is the authoritative local cleanup step. If the container delete succeeds, the reconciler treats that runner as removed for capacity and next-name decisions, so a replacement can still be created even when cleanup logged warnings.

**Webhook** is the primary event driver. `workflow_job.queued` and `workflow_job.completed` events trigger the scaler within 2 seconds (debounced). When `[[runner_classes]]` are configured, the daemon first filters classes by the event repository's org or exact repo target, then routes the trigger to classes whose `match_labels` are present in the job's `runs-on` labels. `push` events to tracked repos trigger cache volume syncs via `lxc exec` on a running container when the push targets that repo's default branch.

**Poll loop** runs every `poll_interval` as a safety net in case a webhook is missed.

The listener also exposes additive read-only health endpoints: `GET /healthz` returns `200 OK`, and `GET /statusz` returns JSON with the latest webhook and reconcile timestamps plus current reconcile state.

---

## Environment Prerequisites

The binary is statically compiled with no runtime dependencies. The host needs:

| Dependency | Required | Purpose |
|------------|----------|---------|
| LXD (snap) | Y | Container runtime |
| ZFS | Y | Fast same-pool clones for scale-up |
| GitHub token | Y | Runner management, webhook events |
| Network access from GitHub | If webhook enabled | Receives `workflow_job` and `push` events |
| Grafana + Loki, or Grafana Cloud with Loki enabled | If metrics enabled | Dashboard visualization |

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

Set `container.lxd.remote` in `config.toml` to the remote name. The scaler resolves the address and TLS client certs from the standard LXD config at `~/.config/lxc/`.

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
#
# If any jobs use Playwright on self-hosted runners, pre-install the pinned
# browser bundle on the template so clones do not fail on first use. The
# bundled load-test repo currently pins Playwright 1.58.2 plus Chromium:
su - runner -c \
  'PLAYWRIGHT_BROWSERS_PATH=/home/runner/.cache/ms-playwright npx playwright@1.58.2 install --with-deps chromium'
```

Then exit and stop:

```bash
exit
lxc stop gh-runner-template
```

Do **not** run `config.sh` on the template -- each ephemeral clone configures itself with a fresh registration token.

### GitHub Token

Use a GitHub App installation token, GitHub App user token, fine-grained PAT, or classic PAT that can call the runner endpoints for the target you configure.

For fine-grained tokens:

| Target | Permission | Access | Purpose |
|--------|------------|--------|---------|
| Organization runners | Self-hosted runners | Read and write | List, register, and deregister org runners |
| Repository runners | Repository administration | Write | List, register, and deregister repo runners |
| Workflow metrics | Repository actions | Read | Read workflow run and job status |
| Org workflow metrics | Repository metadata | Read | List repositories in the org |

For a classic PAT against org-scoped runners, use `admin:org`, plus `repo` when private repository workflow metrics must be read. For repo-scoped runners, the token must have repository admin access; a classic PAT needs `repo`.

### Webhook Network Access

The host must be reachable from GitHub on the webhook port (default 9876).

Options:
- Direct port exposure (host has a public IP or port forward)
- Reverse proxy (nginx, caddy) with TLS termination
- Tunnel (Cloudflare Tunnel, ngrok, etc.)

GitHub publishes webhook source IPs via the [meta API](https://api.github.com/meta) under the `hooks` key.

Configure the webhook on the same org or repository targeted by `ci.org`, `ci.repo`, or each runner class:

| GitHub webhook field | Value |
|----------------------|-------|
| Payload URL | `https://runner-host.example.com/` or a proxy path that rewrites to `/` |
| Content type | `application/json` |
| Secret | The same value as `GH_WEBHOOK_SECRET` |
| Events | `workflow_job`; also `push` if `[webhook.sync_repos]` is used |

The daemon accepts webhook POSTs at `/`. `GET /healthz`, `GET /statusz`, and authenticated `GET /logs` share the same listener.

### GitHub API Use

The scaler uses the GitHub Actions API for platform state that the runner host cannot infer locally:

- `GET /orgs/<org>/actions/runners` or `GET /repos/<owner>/<repo>/actions/runners`: runner inventory for reconcile decisions and runner-capacity metrics. This is the only authoritative source for whether GitHub currently sees a runner as online, busy, idle, or offline, and it provides runner IDs needed for API cleanup.
- Organization or repository registration/removal token APIs: short-lived tokens for registering new ephemeral runners and removing completed ones.
- Workflow run/job APIs: optional completed-workflow metrics and failure enrichment for the Grafana dashboard.

Reconcile, metrics, and runner-class providers for the same GitHub target share a very short runner-inventory cache. That collapses webhook bursts, immediate metrics collection, and multi-class reconcile passes into a single runner inventory request when they happen within the same few seconds, while regular poll intervals still refresh from GitHub before making scale-up/scale-down decisions. During runner registration or removal, reconcile cache hits are suspended and mutation-period snapshots are cleared so lifecycle decisions do not use inventory fetched before GitHub has caught up. Metrics may reuse a bounded stale runner snapshot during transient GitHub API or rate-limit failures, and the metrics payload marks `runner_inventory_stale`, `runner_inventory_age_s`, `runner_inventory_at`, and `runner_inventory_error` so the dashboard can distinguish stale capacity data from live GitHub state.

Registration and removal tokens are also cached until close to their GitHub-provided `expires_at` time. This avoids repeated token POSTs during adjacent scale-ups or scale-downs; each runner still receives the same valid short-lived token through the normal `config.sh` flow.

Because the runners are created and removed over and over during regular operation, GitHub's runner/audit logs will contain significant and unavoidable noise related to this.

### Grafana + Loki (optional)

Metrics require a Loki push endpoint and a Grafana dashboard pointed at that Loki datasource. Either a self-managed Grafana + Loki deployment or Grafana Cloud with Loki enabled is fine.

For Grafana Cloud:

| Value | Source |
|-------|--------|
| Loki push URL | Grafana Cloud > your stack > Loki > Details |
| Loki username | Instance ID shown on the same page |
| API key | Grafana Cloud > API Keys > create with Loki write scope |

For self-managed Loki, use the Loki push URL and credentials configured for your deployment. If your local Loki has `auth_enabled: false`, set only `LOKI_PUSH_URL`.

Import the repo-maintained baseline dashboard with the available service-account key:

```bash
export GRAFANA_API_URL="https://your-grafana-host"
export GRAFANA_SERVICE_ACCOUNT_TOKEN="${GRAFANA_CLOUD_API_KEY}"

jq -c '{dashboard: ., overwrite: true}' deploy/grafana-dashboard.json | \
  curl -sS -X POST "${GRAFANA_API_URL}/api/dashboards/db" \
    -H "Authorization: Bearer ${GRAFANA_SERVICE_ACCOUNT_TOKEN}" \
    -H "Content-Type: application/json" \
    --data-binary @-
```

If your Grafana Loki datasource UID is not `loki`, replace every datasource UID in
the dashboard JSON or remap it in Grafana after import; otherwise no data will
render in panels.

---

## Build

Requires the Go version declared in `go.mod`.

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

Copy `config.example.toml` and edit. Every setting has a sensible default except the GitHub target: set either `ci.org` for org-scoped runners or `ci.repo` for one repository-scoped runner target, unless each configured runner class sets its own target.

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

Shared-cache pruning is configured under `[cache.prune]`:

| Key | Default | Description |
|-----|---------|-------------|
| `enabled` | `true` | Run opportunistic cleanup when a managed runner starts with cache enabled |
| `interval` | `24h` | Minimum time between real prune passes, tracked by a stamp file on `/cache` |
| `max_age` | `336h` | Remove stale cache directories older than this retention window by directory modification time |
| `temp_max_age` | `6h` | Remove abandoned temporary `*-next` cache exports older than this; keep it longer than expected build duration |
| `paths` | `["/cache/buildx"]` | Specific shared-cache roots to prune; entries must be subdirectories under `/cache` |

Symlinks are configured via `[[cache.symlinks]]` entries:

```toml
[[cache.symlinks]]
source = "/cache/npm"
target = "/home/runner/.npm"
```

Each entry creates a symlink inside the container mapping `target` to `source` on the cache volume.
The cache volume is intentionally shared and long-lived, so new ephemeral runners inherit the accumulated cache state from prior runners instead of starting cold each time.
On scale-up, the cache setup step now migrates any pre-existing local directory at each `target` into the shared cache volume before replacing it with the symlink. This avoids the common `ln -sfn` trap where a populated directory like `/home/runner/.cache/pip` keeps absorbing writes and only gets a nested symlink entry.

If one of the cache targets is `/opt/hostedtoolcache`, the scaler also writes `AGENT_TOOLSDIRECTORY` and `RUNNER_TOOL_CACHE` into `/home/runner/.env` before the runner service is installed. That keeps `actions/setup-*` downloads in the shared tool cache instead of falling back to `RUNNER_DIR/_work/_tool`.

When cache is enabled, the scaler also prepares `/cache/buildx` and writes `RUNNER_BUILDX_CACHE_ROOT=/cache/buildx` plus `DOCKER_BUILDKIT=1` into `/home/runner/.env`. Docker Buildx does not use external cache storage implicitly; workflows must still pass `--cache-from type=local,src=...` and `--cache-to type=local,dest=...`. Use a stable cache path per repo, branch, and image so parallel jobs do not overwrite each other:

```bash
CACHE_ROOT="${RUNNER_BUILDX_CACHE_ROOT:-/cache/buildx}/${GITHUB_REPOSITORY#*/}"
CACHE_BRANCH="${GITHUB_REF_NAME//\//-}"
CACHE_IMAGE="example-image"
CACHE_DIR="$CACHE_ROOT/$CACHE_BRANCH/$CACHE_IMAGE"
CACHE_NEXT="$CACHE_DIR-next"
mkdir -p "$CACHE_ROOT/$CACHE_BRANCH"
rm -rf "$CACHE_NEXT"

cache_from=()
if [ -f "$CACHE_DIR/index.json" ]; then
  cache_from+=(--cache-from "type=local,src=$CACHE_DIR")
fi
if [ "$CACHE_BRANCH" != "main" ] && [ -f "$CACHE_ROOT/main/$CACHE_IMAGE/index.json" ]; then
  cache_from+=(--cache-from "type=local,src=$CACHE_ROOT/main/$CACHE_IMAGE")
fi

docker buildx build \
  "${cache_from[@]}" \
  --cache-to "type=local,dest=$CACHE_NEXT,mode=max,compression=zstd" \
  -t "$IMAGE" \
  .

rm -rf "$CACHE_DIR"
mv "$CACHE_NEXT" "$CACHE_DIR"
```

Keep the workflow's existing output behavior: add `--load` if later steps need the built image in the local Docker image store, or `--push` if the build step should publish directly. The local Buildx cache backend stores an OCI image layout on disk. Exporting replaces the active `index.json`, but old blobs remain under the cache directory. Standard Docker image cleanup does not prune `/cache/buildx`; the scaler's shared-cache prune pass removes stale branch/image cache directories and abandoned `*-next` exports from configured `[cache.prune].paths` instead of deleting OCI blob files inside active cache directories.

The scaler writes these cache env values only when it configures a managed runner before installing the runner service. Existing long-lived runners and already-running containers do not get rewritten retroactively; recycle them or patch `/home/runner/.env` manually if they must use the same tool-cache and Buildx cache paths.

#### CI: `[ci]`

| Key | Default | Description |
|-----|---------|-------------|
| `provider` | `github` | CI platform module (`github`) |
| `org` | | GitHub organization name for org-scoped runners |
| `repo` | | GitHub `owner/name` repository target for repo-scoped runners |

GitHub-specific settings under `[ci.github]` are currently empty -- token and webhook secret are set via environment variables.

#### Runner Classes: `[[runner_classes]]`

Runner classes let one daemon manage multiple logical runner types without splitting into separate deployments. If no `[[runner_classes]]` entries are present, the daemon synthesizes one `default` class from the existing `[scaler]`, `[container]`, `[cache]`, and `[ci]` settings, so single-pool configs do not need a runner class block.

Each class can override the inherited target, runner prefix, labels, scale cap, idle timeout, work directory, LXD template, and cache profile. Set `enabled = false` to keep a class in config without polling its GitHub APIs or creating runners.

```toml
[[runner_classes]]
id = "typescript-standard"
org = "ExampleOrg"
prefix = "ts-runner"
labels = "self-hosted,linux,x64,runner-class-typescript"
match_labels = ["self-hosted", "linux", "x64", "runner-class-typescript"]
max_auto_runners = 8
idle_timeout = "300s"
runner_work_dir = "_work"
template = "gh-runner-template-typescript"
cache_profile = "node"

[[runner_classes]]
id = "rust-standard"
org = "ExampleOrg"
prefix = "rust-runner"
labels = "self-hosted,linux,x64,runner-class-rust"
match_labels = ["self-hosted", "linux", "x64", "runner-class-rust"]
max_auto_runners = 6
idle_timeout = "300s"
runner_work_dir = "_work"
template = "gh-runner-template-rust"
cache_profile = "rust"

[[runner_classes]]
id = "personal-gh-runner-scaler"
repo = "octo-user/example-repo"
prefix = "personal-runner"
labels = "self-hosted,linux,x64,runner-class-personal"
match_labels = ["self-hosted", "linux", "x64", "runner-class-personal"]
max_auto_runners = 2
```

Use distinctive labels in both `labels` and workflow `runs-on` values so GitHub schedules jobs onto the intended class:

```yaml
runs-on: [self-hosted, linux, x64, runner-class-rust]
```

`match_labels` controls router wakeups. If a queued job has no labels or no class matches, the daemon falls back to triggering all classes for that GitHub target rather than dropping the event.

Webhook routing filters by target before label matching. An org-scoped class only receives events from repositories owned by that org, and a repo-scoped class only receives events from that exact repository. Personal GitHub repositories cannot share one owner-wide runner pool; add one repo-scoped runner class per personal repository that should use this scaler.

#### Cache Profiles: `[cache_profiles.<name>]`

Cache profiles are reusable cache definitions for runner classes. They use the same shape as `[cache]`, including `[cache_profiles.<name>.prune]` and repeated `[[cache_profiles.<name>.symlinks]]` entries:

```toml
[cache_profiles.rust]
enabled = true
pool = "runner-pool"
volume = "runner-cache-rust"

[cache_profiles.rust.prune]
enabled = true
interval = "24h"
max_age = "336h"
temp_max_age = "6h"
paths = ["/cache/buildx"]

[[cache_profiles.rust.symlinks]]
source = "/cache/cargo-registry"
target = "/home/runner/.cargo/registry"

[[cache_profiles.rust.symlinks]]
source = "/cache/cargo-git"
target = "/home/runner/.cargo/git"
```

Runner classes that omit `cache_profile` inherit the top-level `[cache]` settings.

When explicit runner classes are configured, per-runner state is stored below `state.filesystem.dir/runner-groups/<id>/`. Legacy single-class configs continue to use the configured state directory directly.

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

The same HTTP listener also exposes a read-only `GET /logs` endpoint for recent structured daemon logs. It requires `Authorization: Bearer <token>`, where the token is `GH_SCALER_LOG_TOKEN` if set, otherwise `GH_WEBHOOK_SECRET`.

Supported query parameters:

| Query | Description |
|-------|-------------|
| `runner` | Exact runner/container name |
| `repo` | Exact `owner/repo` |
| `action` | Exact action such as `queued`, `in_progress`, `completed`, `push`, `scheduled`, `failed` |
| `event_type` | Exact event family such as `workflow_job`, `push`, `scale_up`, `scale_down`, `cache_sync` |
| `workflow` | Case-insensitive workflow-name substring |
| `job` | Case-insensitive job-name substring |
| `commit` | Commit SHA prefix |
| `branch` | Exact branch name |
| `since`, `until` | RFC3339 timestamps |
| `limit` | Max entries returned (`200` default, `1000` max) |
| `q` | Case-insensitive free-text match across message/detail fields |

Example:

```bash
curl -H "Authorization: Bearer $GH_SCALER_LOG_TOKEN" \
  "http://runner-host:9876/logs?runner=gh-runner-auto-3&repo=ExampleOrg/example-repo&commit=abc1234"
```

#### Metrics: `[metrics]`

| Key | Default | Description |
|-----|---------|-------------|
| `enabled` | `true` | Push metrics to backend. `config.example.toml` sets this to `false` until Loki is configured. |
| `interval` | `60s` | Collection and push interval |
| `collect_workflows` | `true` | Include recent workflow run durations and outcomes |
| `workflow_repo_batch_size` | `25` | Max repos scanned per workflow-metrics interval (`0` = scan all repos) |
| `collect_host` | `true` | Include container counts and storage pool usage |

Loki-specific settings under `[metrics.loki]` are set via environment variables. They can point to either self-managed Loki or Grafana Cloud Loki. `LOKI_PUSH_URL` is required when metrics are enabled. `LOKI_USERNAME` plus `LOKI_API_KEY` or `LOKI_PASSWORD` enables HTTP basic auth; the legacy `GRAFANA_CLOUD_API_KEY` name is still accepted for Grafana Cloud deployments.

Loki pushes retry short-lived transport failures, including temporary DNS resolver errors, before surfacing the push failure in logs. Non-2xx Loki responses include the response body in the error so server-side validation failures are visible in the scaler journal.

When metrics are enabled, the daemon also derives two additional observability streams from the retained structured event history in the state dir:

- `lifecycle-metrics`: queue wait, runner reuse, and scale-down-to-next-scale-up analytics
- `issue-events`: warning/error events from the scaler itself for issue-count panels

Workflow metrics are collected in two phases to control GitHub API use. The provider first lists recent completed workflow runs without job/step detail, filters out runs already delivered to Loki, and only then enriches fresh failed runs with failed job, failed step, and failure reason. For org-scoped targets, the repository list is cached for 10 minutes and `workflow_repo_batch_size` rotates through repos across collection intervals. Repo-scoped targets query only their configured repository. With the default `25`, a large org is scanned in bounded chunks instead of every metrics tick. Loki entries use push-time timestamps so self-managed Loki deployments with old-sample rejection do not reject quiet repositories whose most recent workflow runs are older than the Loki sample window; the original GitHub completion time remains in the `completed_at` JSON field.

For lower GitHub API load, keep the webhook enabled so the poll loop can remain a safety net, keep `[metrics].interval` at or above the default `60s`, leave `workflow_repo_batch_size` bounded for large orgs, and disable `collect_workflows` if dashboard workflow history is not needed.

With `[[runner_classes]]`, runner and host metric streams include a `group_id` Loki label and JSON field so class-specific panels can filter or split the pool.

Shared-cache pruning only removes stale directories under configured `/cache/...` paths. It does not prune retained structured event history, workflow metric dedupe state, or issue-event dedupe state in the daemon state directory.

#### State: `[state]`

| Key | Default | Description |
|-----|---------|-------------|
| `provider` | `filesystem` | State tracking module (`filesystem`) |

Filesystem-specific: `[state.filesystem]`

| Key | Default | Description |
|-----|---------|-------------|
| `dir` | `.state` | Directory for per-container timestamp files, workflow/issue delivery caches, and persisted `/logs` history. `config.example.toml` uses `/var/lib/gh-runner-scaler/state` for systemd deployments. |

For production, use an absolute path like `/var/lib/gh-runner-scaler/state`.

The daemon state directory contains:

- `daemon_logs.jsonl`: append-first structured log history for `GET /logs`, compacted to the newest 20,000 entries when needed
- `workflow_metrics_seen.json`: delivered workflow-run keys so Loki is not spammed after restarts
- `issue_events_seen.json`: delivered warning/error event keys for the same reason

On startup, the log store tolerates a single trailing truncated JSON entry from an interrupted append and compacts the file. Other malformed history is treated as a real state-file error.

### Secrets (Environment Variables)

Secrets are **never** stored in the config file. Set them as environment variables or in an env file read by systemd.

| Variable | Required | Purpose |
|----------|----------|---------|
| `GH_SCALER_GITHUB_TOKEN` | Y | GitHub token for runner management |
| `GH_WEBHOOK_SECRET` | If webhook enabled | HMAC secret for signature verification |
| `GH_SCALER_LOG_TOKEN` | Optional | Dedicated bearer token for `GET /logs` (falls back to `GH_WEBHOOK_SECRET`) |
| `LOKI_PUSH_URL` | If metrics enabled | Grafana Loki push endpoint |
| `LOKI_USERNAME` | If Loki basic auth is enabled | Loki basic auth username or Grafana Cloud instance ID |
| `LOKI_API_KEY` or `LOKI_PASSWORD` | If Loki basic auth is enabled | Loki basic auth password or write API key |
| `GRAFANA_CLOUD_API_KEY` | Optional legacy fallback | Grafana Cloud Loki write API key |

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
GH_SCALER_GITHUB_TOKEN=...
GH_WEBHOOK_SECRET=your-webhook-secret
# GH_SCALER_LOG_TOKEN=separate-read-token
# LOKI_PUSH_URL=https://logs-prod-XXX.grafana.net/loki/api/v1/push
# LOKI_USERNAME=your-loki-instance-id
# LOKI_API_KEY=...
# GRAFANA_CLOUD_API_KEY=... # legacy fallback
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
level=INFO msg="daemon started" poll_interval=30s webhook=true metrics=false
level=INFO msg="webhook server listening" addr=:9876
```

Routine reconcile start and runner-state snapshots are debug-level logs. Normal `INFO`
output should stay focused on lifecycle events such as scale-up, scale-down, webhook
handling, and service startup.

### Performance follow-ups

The scaler-side cache drift, tool-cache env wiring, Buildx local cache root, and age-based Buildx cache pruning are handled in this repo, but two larger speed wins remain operational follow-up items:

- Heavy Docker builds should use `docker buildx build` with the shared local cache under `/cache/buildx`, or a registry/S3 cache when a repo needs cache sharing outside the runner host. Layer cache inside a one-job ephemeral clone is disposable by design unless it is explicitly exported and imported.
- Expensive common tooling should be prewarmed into the template or the shared tool cache. Common examples include cloud CLIs, deployment plugins, and pinned browser bundles.

### Quick update on the server

After a `git pull` on the server checkout, run:

```bash
./deploy/update-server.sh
```

The script rebuilds the binary from the current checkout, installs the binary and systemd unit, reloads systemd, and restarts `gh-runner-scaler.service`. It does not overwrite an existing `/etc/gh-runner-scaler/config.toml` or `/etc/gh-runner-scaler/env`.
It requires a working Go toolchain on the server because it builds from the checked-out source rather than downloading a release artifact.

Before you use it, make sure these runtime files already exist:

```bash
sudo test -f /etc/gh-runner-scaler/config.toml
sudo test -f /etc/gh-runner-scaler/env
```

If `/etc/gh-runner-scaler/config.toml` is missing but the repo checkout has a `config.toml`, the script will install that once. If either file is still missing, the script stops before touching systemd so it does not restart the service into a broken state.

On hosts without Go, build the Linux binary somewhere else, copy it to the host, and install it through your normal release process:

```bash
GOOS=linux GOARCH=amd64 go build -o /tmp/gh-runner-scaler ./cmd/scaler
scp /tmp/gh-runner-scaler deployer@runner-host:/tmp/gh-runner-scaler
ssh deployer@runner-host \
  'set -e
   sudo install -m 0755 /tmp/gh-runner-scaler /usr/local/bin/gh-runner-scaler
   sudo systemctl restart gh-runner-scaler.service
   sudo systemctl --no-pager --full status gh-runner-scaler.service --lines=20'
```

For production, verify the source commit before building and record the deployed commit or release artifact alongside your normal change log.

### One-shot reconcile

Verify LXD and GitHub connectivity before enabling the daemon. This is an active reconcile, not a dry-run; it may create or clean up managed runners and LXD containers according to the config.

```bash
sudo sh -c 'set -a; . /etc/gh-runner-scaler/env; set +a; exec /usr/local/bin/gh-runner-scaler reconcile --config /etc/gh-runner-scaler/config.toml'
```

### Manual / foreground run

For debugging, run the daemon in the foreground:

```bash
sudo sh -c 'set -a; . /etc/gh-runner-scaler/env; set +a; exec /usr/local/bin/gh-runner-scaler daemon --config /etc/gh-runner-scaler/config.toml'
```

### File layout after install

```
/usr/local/bin/gh-runner-scaler          -- binary
/etc/gh-runner-scaler/config.toml        -- configuration
/etc/gh-runner-scaler/env                -- secrets (mode 600)
/etc/systemd/system/gh-runner-scaler.service -- systemd unit
/var/lib/gh-runner-scaler/state/         -- container state files + workflow metric cache
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

Two dashboard artifacts live under `deploy/`:

- `deploy/grafana-dashboard.json`: repo-maintained baseline dashboard aligned to the current metrics contract.
- `deploy/grafana-dashboard-old.json`: exported/reference dashboard snapshot using Grafana's newer `elements` + `GridLayout` schema.

Treat `deploy/grafana-dashboard.json` as the source of truth for the metrics contract in this repo. Keep `deploy/grafana-dashboard-old.json` for export/schema comparisons or for operators who specifically want the fuller Grafana-export shape.

Both dashboards require Grafana with a Loki datasource receiving metrics from the scaler. That datasource can be backed by self-managed Loki or by Grafana Cloud with Loki enabled. The dashboards show:

- Runner capacity health, including provisioning runners during scale-up
- Lifecycle analytics such as queue wait, jobs per runner lifecycle, reuse rate, and scale-down-to-next-scale-up gap
- Workflow failure hotspots with repo/branch/workflow/job/step context
- Actionable daemon warning/error counts and recent issue details
- GitHub runner-inventory API errors split into their own table so transient
  upstream `listing runners` failures do not hide scaler-owned problems
- Recent workflow outcomes plus managed runner container counts and cache pool usage

The dashboards default to `1m` auto-refresh to match the default metrics collection interval. If you change `[metrics].interval`, keep the Grafana refresh interval aligned so panels are not repeatedly redrawn without new samples.

The maintained dashboard baseline expects a Loki datasource UID of `loki`. If your Grafana stack uses a different Loki datasource UID, remap the datasource during import or update the panel datasource settings after import.

## Load Testing

The repo includes a reusable load-test harness under `loadtest/`:

- `loadtest/repo-template/` seeds a standalone GitHub Actions repo with
  queue-burst, Node, Python, Go, and Playwright workflows.
- `loadtest/create-test-repo.sh` creates that repo locally and can push it to
  GitHub once `gh` auth is valid.
- `loadtest/dispatch-load.sh` dispatches repeatable workload profiles with
  varying concurrency and workload mixes.
- `loadtest/collect-server-evidence.sh` samples LXC and host resource
  snapshots during the run and writes the scaler journal when the capture
  ends.

The helper scripts assume:

- local Git identity is configured before seeding a repo (`git config --global user.name` / `user.email`)
- the target repo's default branch already contains the workflow files you want to dispatch
- browser profiles run against a template that already has the pinned Playwright browser bundle in `/home/runner/.cache/ms-playwright`
- server-side evidence collection runs on the scaler host with root or `sudo`

See `loadtest/README.md` for the recommended sequence, evidence collector
settings, and workload profiles.

---

## Design Notes

**ZFS cloning**: The template lives on a ZFS pool. Same-pool clones are metadata-only (~0.4s) vs cross-pool copies (~14s). Template and runners must share a pool. NVMe pools suit the persistent cache volume where sequential write throughput matters more.

**Idle timeout**: `idle_timeout = "300s"` balances warm-runner availability for bursty workloads against resource consumption.

**Concurrency**: All three subsystems run as goroutines in one process. A channel-based trigger with `time.AfterFunc` debounce replaces the bash flock + systemd timer approach. The daemon still allows only one reconcile at a time, but webhook-triggered demand is tracked while that reconcile runs so another pass starts immediately afterward instead of waiting for the next poll tick.

**Orphan detection**: Containers matching the auto-scale prefix but with no registered GitHub runner are cleaned up immediately. This catches containers left behind by crashed scalers, failed `config.sh`, or manual intervention.

**MAC handling**: When cloning the template, the scaler clears inherited `volatile.*.hwaddr` entries so LXD assigns a fresh MAC address. Without this, clones fail to start due to MAC collisions.
