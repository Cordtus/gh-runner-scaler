# Runner Infrastructure Optimization Plan

## Context

Self-hosted runners for the Axionic-Labs GitHub org run as ephemeral LXC containers
on 192.168.0.170 (nodev2). Each container is cloned from a stopped ZFS template on
`cstor` (4x SATA SSD RAIDZ1), runs one job, then is destroyed. This means every job
starts cold: no dependency caches, no tool caches, no incremental build state.

Benchmark results (2026-04-02) confirmed the template should stay on cstor -- same-pool
ZFS clone is 0.38s vs 13-15s for cross-pool copy. NVMe pools (pool7, pool9) are better
suited for persistent cache volumes.

## Already Implemented (across Axionic-Labs repos)

- `actions/cache@v4` for axionic-ui dependency in Spectra-App workflows
- `setup-node` removed; pre-installed `/opt/axionic-ui` symlinked instead
- `paths-ignore` filters to skip CI on doc-only changes
- `concurrency` groups to cancel superseded runs
- Branch: `chore/ci-workflow-optimization` across multiple repos

## Phase 1: Persistent Build Cache (runner-scaler changes)

### 1a. Create a shared cache volume on an NVMe pool

Use pool9 (7% used, plenty of headroom) as a persistent cache volume. Create a
dedicated ZFS dataset via LXD that gets mounted into every ephemeral runner.

Cache paths to mount:
- `/cache/npm` -> container's `~/.npm`
- `/cache/yarn` -> container's `~/.cache/yarn`
- `/cache/pip` -> container's `~/.cache/pip`
- `/cache/tool-cache` -> container's `/opt/hostedtoolcache` (RUNNER_TOOL_CACHE)

Changes to `gh-runner-scaler.sh`:
- In `scale_up()`, after `lxc start`, attach the shared cache volume via
  `lxc config device add <name> cache disk source=/path pool=pool9 path=/cache`
- Add cache directory initialization (correct ownership for `runner` user)

### 1b. Set RUNNER_TOOL_CACHE in runner environment

Configure the runner service to use `/cache/tool-cache` so `actions/setup-node`,
`actions/setup-python`, etc. persist across ephemeral containers.

### 1c. Per-repo cache directories

Structure: `/cache/repos/<owner>/<repo>/` for repo-specific build caches (e.g.,
Next.js `.next/cache`, TypeScript `tsconfig.tsbuildinfo`). Workflows can symlink or
`--cache-dir` to these paths.

## Phase 2: axionic-ui Dependency Fix

### Problem

`/opt/axionic-ui` on the runner is stale -- never updated by CI, and the runner
process can't write to it (permission denied on `git fetch`). PR #110 on Spectra-App
is currently blocked by this.

### Immediate Fix (unblock PR #110)

Update `/opt/axionic-ui` on the runner host manually:
```bash
ssh bv@192.168.0.170
sudo git -C /opt/axionic-ui fetch origin main
sudo git -C /opt/axionic-ui reset --hard origin/main
sudo chown -R root:root /opt/axionic-ui
```

### Short-Term: Runner Cron Job

Add a systemd timer on the runner host to keep `/opt/axionic-ui` current:
```
[Timer]
OnBootSec=60
OnUnitActiveSec=300
```
Pulls `origin main` every 5 minutes. Minimal workflow changes.

This can be managed from this repo as another systemd unit
(`gh-runner-ui-sync.service` + `.timer`).

### Long-Term: Git Dependency (recommended)

Change Spectra-App `package.json`:
```json
"@axionic/ui": "github:Axionic-Labs/axionic-ui#main"
```

Benefits:
- Eliminates `/opt` dependency entirely
- No symlink required in CI
- Pin to commit SHA or tag for reproducibility
- Explicitly bump when needed
- Local dev can override with `npm link`
- Aligns with marketing frontend approach

Requires: updating all 5 Spectra-App workflows to remove the symlink step.

## Phase 3: Warm Template Refresh

Add `update-template.sh` to this repo:
1. Start the template container
2. `apt-get update && apt-get upgrade`
3. Update runner software to latest version
4. Pre-install common tools (node LTS, python3, build-essential)
5. Clear caches (they'll live on the persistent volume now)
6. Stop and snapshot

Run weekly via systemd timer (`gh-runner-template-refresh.service` + `.timer`).

## Phase 4: Incremental Build Tooling (deferred)

Turborepo (MIT) or Nx (MIT) could cache build outputs by content hash. This requires
adoption in the actual project repos, not just runner-side changes. Evaluate after
phases 1-3 are measured.

Candidate repos:
- Spectra-App (Next.js -- would benefit most from Turborepo)
- Axionic-Labs-Mechanex-Frontend
- axionic-ui (build system)

## Priority Order

| Phase | Effort | Impact | Dependency |
|-------|--------|--------|------------|
| 2 (immediate) | 10 min | Unblocks PR #110 | None |
| 2 (cron) | 30 min | Keeps axionic-ui current | None |
| 1a-b | 2-3 hrs | Eliminates cold cache on every job | None |
| 3 | 1-2 hrs | Faster boot, current packages | None |
| 1c | 1 hr | Per-repo build cache | 1a |
| 2 (git dep) | 1 hr | Eliminates /opt dependency | Spectra-App PR |
| 4 | Days | Incremental builds across repos | 1-3 measured |
