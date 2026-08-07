# AGENTS.md

## Big trap: do not resurrect the old bash/python system

The repo previously shipped a `CLAUDE.md` describing an obsolete bash/python architecture (`gh-runner-scaler.sh`, `webhook.py`, `metrics.py`, `install.sh`, `setup-webhook.sh`, `gh-runner-scaler.timer`); it was deleted as part of cleanup because that code never existed in git. The real system is **one statically-linked Go binary** (`cmd/scaler`). If that `CLAUDE.md` reappears, trust `README.md` and delete it again; never resurrect the old files or systemd unit names.

## What this is

Demand-driven auto-scaler for GitHub Actions self-hosted runners on LXC (LXD) containers. A single daemon process runs the webhook listener, poll loop, reconcile engine, and metrics pusher as goroutines.

## Verify (matches `.github/workflows/ci.yml`)

```bash
gofmt -l .          # must output nothing
go vet ./...
go test ./...       # pure unit tests; no LXD/GitHub required
go build -o gh-runner-scaler ./cmd/scaler
```

Go 1.26+ from `go.mod`. No lint/test runner beyond these. `deploy/tests/*.sh` are shell unit tests for the deploy scripts and are run manually, not in CI.

## Architecture

- `cmd/scaler/main.go` is the composition root and the **only** file importing provider packages. `internal/` depends only on interfaces in `internal/iface/` (`ContainerRuntime`, `CIProvider`, `CacheManager`, `MetricsBackend`, `StateStore`).
- Adding a provider = new `provider/<name>/` package implementing an interface + one `case` in `main.go` (`wireRuntimeAndCache`, `wireCIProvider`, etc.). Zero `internal/` changes.
- `internal/engine` = scaling logic; `internal/daemon` = goroutine orchestration, webhook HTTP server, logs, metrics loop; `internal/demand` = queued-demand tracking; `internal/runnerobs` = runner log delivery bootstrap.
- With `[[runner_classes]]` in config, per-group state lives under `<state.filesystem.dir>/runner-groups/<id>/`; a config with no classes uses the state dir directly. Multiple classes sharing a GitHub target share one `ghprovider.Provider` via `ShareRunnerCacheWith` (runner-inventory + registration-token cache).
- CLI: `gh-runner-scaler daemon` (default), `reconcile`, `version`. `--config` defaults to `config.toml`. **`reconcile` is an active pass, not a dry-run** — it creates and tears down runners/containers.

## Config and secrets

- Copy `config.example.toml` to `config.toml`; both are git-ignored. Only `deploy/nodev2.config.toml` is the tracked live config.
- Secrets never live in the TOML: `GH_SCALER_GITHUB_TOKEN`, `GH_WEBHOOK_SECRET`, `GH_SCALER_LOG_TOKEN` (logs endpoint), `LOKI_PUSH_URL`/`LOKI_USERNAME`/`LOKI_API_KEY` (or legacy `GRAFANA_CLOUD_API_KEY`).
- Production reads `/etc/gh-runner-scaler/config.toml` and `/etc/gh-runner-scaler/env` (mode 600) via the systemd unit's `EnvironmentFile=`. State lives at `/var/lib/gh-runner-scaler/state/` (repo `.state/` is local-dev scratch, git-ignored).
- Webhook listener on `:9876`: POST `/` (webhook), GET `/healthz`, `/statusz`, and `/logs` (bearer auth, defaults to `GH_WEBHOOK_SECRET`).

## Deploy (the live service is remote, not this machine)

- Target host: **nodev2** (`bv@192.168.0.170`), repo checked out there. Deploy from that checkout, never from this dev box.
- `./deploy/update-server.sh` rebuilds from source, installs binary + systemd units + `deploy/` helper scripts, and restarts `gh-runner-scaler.service`. It refuses to run without `/etc/gh-runner-scaler/{config.toml,env}` (unless the repo `config.toml` exists and the live config is missing). Requires a Go toolchain on the host.
- To deploy the tracked live config: `GH_RUNNER_SCALER_CONFIG_SOURCE=deploy/nodev2.config.toml ./deploy/update-server.sh`. Full deploy with observability setup: `./deploy/deploy-runner-observability.sh` (run on nodev2).
- `deploy/refresh-runner-template.sh` (driven by `gh-runner-distribution-refresh.timer`) downloads verified `actions/runner` archives into the stopped template. Manual version pinning is in `docs/runner-orchestration-runbook.md`.
- User-facing setup helpers (both unit-tested in `deploy/tests/`, run manually): `deploy/setup-github.sh` validates a GitHub token and creates/updates the webhook via the GitHub API; `deploy/deploy-grafana-dashboard.sh` imports `deploy/grafana-dashboard.json` into an existing Grafana, rewriting the Loki datasource UID.
- `docs/runner-classes-guide.md` is the user-facing decision guide for designing `[[runner_classes]]`; the README Quickstart is the onboarding path.

## Operational gotchas

- Template container must stay **stopped**; never run `config.sh` on it. Ephemeral clones self-register.
- Template and clones must share a ZFS pool — same-pool clones are ~0.4s, cross-pool copies ~14s.
- The scaler clears inherited `volatile.*.hwaddr` on clones; without this, clones fail to boot on MAC collision.
- Load-testing harness lives in `loadtest/` (see `loadtest/README.md`); it dispatches real jobs against a seeded test repo and needs `gh` auth plus a target repo covered by the scaler config.
