# Designing Runner Classes

This guide explains how to design the right `[[runner_classes]]` for your own
workloads. It is the decision framework behind the working example in
`deploy/nodev2.config.toml`.

## The mental model

A runner class defines one logical runner type that the scaler creates on
demand. It is the combination of five choices:

| Choice | Config key | What it controls |
|--------|-----------|------------------|
| GitHub target | `org` or `repo` | Which org's or repo's jobs can route onto this class |
| Routing labels | `labels` + `match_labels` | How GitHub schedules jobs onto the class |
| Base image | `template` | The stopped LXD container every runner is cloned from |
| Cache profile | `cache_profile` | The persistent ZFS volumes shared across all clones of the class |
| Capacity | `max_auto_runners`, `baseline`, `idle_timeout` | How many runners, whether one stays warm, how fast idle ones are torn down |

GitHub routes a job onto a self-hosted runner when every label in the job's
`runs-on` is present on the runner. This is a **superset match**, not an exact
match. So a distinct class label routes precisely:

```yaml
runs-on: [self-hosted, linux, x64, runner-class-node]
```

A job carrying `runner-class-node` can never land on a runner whose labels
omit it, and a job without it can never land on a node-class runner.

**The default class shortcut:** with no `[[runner_classes]]` blocks at all, the
scaler synthesizes one `default` class from `[scaler]`, `[container]`, `[cache]`,
and `[ci]`. That is the right shape for a single workload. Add explicit classes
once you need multiple logical runner types from one daemon.

## The design process

### 1. Enumerate your workloads

List every CI job you run, then group them by what materially differs:

- **Toolchain** — Node, Rust, Go, Python, browser tests. Each pulls a different
  set of caches and possibly needs a different template.
- **Cache identity** — jobs that share dependency caches (same package
  manager, same repo) belong in one class; jobs whose caches would conflict do
  not. A Foundry build writing into the same cache namespace as a plain npm
  build is a conflict.
- **Resource profile** — heavy Docker/Buildx builds and browser runs have
  different memory/CPU and isolation needs than a 30-second lint job.
- **Isolation** — do not run two jobs against one Docker daemon or one Buildx
  cache namespace concurrently unless they tolerate each other.
- **Frequency** — a steady high-volume stream (every push) behaves differently
  from occasional bursty queues.

Two workloads belong in the **same class** only when they can share the same
template, the same cache profile, and the same capacity policy without
interfering. Otherwise give each its own class.

### 2. Decide the template

The template is a stopped LXD container that is cloned per runner. Clones are
ephemeral: each one self-registers, runs a single job, and is deleted.

- **Start from the common base** in `gh-runner-template` and preinstall what is
  expensive or version-critical: system dependencies, a Docker server, pinned
  browser bundles. Anything cheap and frequently updated belongs in a shared
  cache instead (see below).
- **Split a template only when a workload needs materially different
  toolchain or isolation.** The nodev2 platform runs Node, Foundry, and
  browser workloads off the same base template and separates them with cache
  profiles. A separate browser template only earns its keep when browser
  bundles or UI dependencies slow down general CI. Start with one template and
  split when load tests say to.
- A template used by several classes is shared; the distribution refresh
  (`deploy/refresh-runner-template.sh`) keeps `actions/runner` updated inside
  it. Never run `config.sh` on the template — clones configure themselves.

### 3. Design the cache profile

Ephemeral clones start cold. The shared cache is what makes them fast: a
persistent ZFS volume mounted on every clone, with symlinks redirecting each
tool's cache directory onto it. A class with no cache profile inherits the
top-level `[cache]` block; a class with a profile gets that profile's own
volumes.

Symlink each tool's cache onto a volume:

```toml
[[cache_profiles.node.symlinks]]
source = "/cache/npm"
target = "/home/runner/.npm"

[[cache_profiles.node.symlinks]]
source = "/cache/pnpm"
target = "/home/runner/.local/share/pnpm/store"

[[cache_profiles.node.symlinks]]
source = "/cache/tool-cache"
target = "/opt/hostedtoolcache"
```

| Toolchain | Cache path | Notes |
|-----------|-----------|-------|
| npm | `/home/runner/.npm` | Registry + tarball cache |
| pnpm | `/home/runner/.local/share/pnpm/store` | Global content-addressable store |
| pip | `/home/runner/.cache/pip` | Wheels |
| Poetry | `/home/runner/.cache/pypoetry` | |
| Go build | `/home/runner/.cache/go-build` | Compiler cache |
| Go modules | `/home/runner/go/pkg/mod` | |
| Cargo registry | `/home/runner/.cargo/registry` | |
| Cargo git | `/home/runner/.cargo/git` | |
| Foundry | `/home/runner/.foundry` | `forge`/`cast` installs |
| Playwright | `/home/runner/.cache/ms-playwright` | Browsers; also preinstall in template |
| hostedtoolcache | `/opt/hostedtoolcache` | `actions/setup-*` downloads; the scaler writes `AGENT_TOOLSDIRECTORY` and `RUNNER_TOOL_CACHE` into `/home/runner/.env` when present |
| Docker Buildx | `/cache/buildx` | See the Buildx section below |

Rules of thumb:

- **Give each workload its own volume when caches could conflict** (a Foundry
  build writing into the npm namespace) or when one workload's cache volume
  would bloat another's. `nodev2` uses separate volumes per profile:
  `runner-cache`, `runner-cache-foundry`, `runner-cache-browser`.
- **Same-pool, dedicated volumes.** Template and clones must share a ZFS pool;
  same-pool clones are ~0.4s, cross-pool copies ~14s. The cache volume should
  also live on a fast pool.
- **Keep cache paths per-repo, per-branch for Buildx.** The scaler prepares
  `/cache/buildx` and exports `RUNNER_BUILDX_CACHE_ROOT` plus `DOCKER_BUILDKIT=1`,
  but workflows must opt in with explicit `--cache-from/--cache-to type=local`
  arguments. Use a stable path per repo, branch, and image so parallel jobs do
  not overwrite each other.
- Pruning is per-profile: `[cache_profiles.<id>.prune]` controls stale directory
  and abandoned `*-next` export cleanup. Defaults match the top-level `[cache.prune]`.

### 4. Set capacity

| Key | Meaning |
|-----|---------|
| `max_auto_runners` | Ceiling on concurrent ephemeral runners for the class. Size to your worst burst depth, not your average. |
| `baseline = true` | Keep one persistent warm runner always registered (named by `baseline_name`). Saves clone+register latency for steady high-frequency traffic; waste otherwise. |
| `idle_timeout` | How long a runner stays idle before teardown. It is a safety valve for runners that never receive work, not a warm-pool replacement interval. |
| `queue_audit_interval` | How often the daemon audits GitHub for missed webhook demand. |

Use a baseline for the single busiest class (e.g. the Node/Docker class that
every push hits) and keep everything else strictly on-demand.

### 5. Pick labels

The class label `runner-class-<id>` is the primary routing key. Keep the base
labels (`self-hosted,linux,x64`, plus host-specific ones like `nodev2,docker`)
identical across classes that share a host so jobs that omit a class label can
still find a runner. `match_labels` mirrors the same set and controls webhook
routing — a queued job whose labels intersect a class's `match_labels` wakes
that class; a job with no labels wakes every class for the target.

## Worked examples

The live `deploy/nodev2.config.toml` shows the pattern: one org-scoped
CAC-Group class plus repo-scoped classes, all sharing one template, with three
cache profiles for Node, Node+Foundry, and Node+Playwright.

```toml
[[runner_classes]]
id = "node"                       # Node.js + Docker, the busiest pool
repo = "owner/poolbet"
prefix = "gh-runner-node"
labels = "self-hosted,linux,x64,nodev2,docker,runner-class-node"
match_labels = ["self-hosted", "linux", "x64", "nodev2", "docker", "runner-class-node"]
max_auto_runners = 2
template = "gh-runner-template"
cache_profile = "node"
baseline = true                   # one persistent warm runner
baseline_name = "gh-runner-primary"

[[runner_classes]]
id = "node-foundry"               # same toolchain, isolated cache namespace
repo = "owner/poolbet"
prefix = "gh-runner-node-foundry"
labels = "self-hosted,linux,x64,nodev2,docker,runner-class-node-foundry"
match_labels = ["self-hosted", "linux", "x64", "nodev2", "docker", "runner-class-node-foundry"]
max_auto_runners = 2
template = "gh-runner-template"
cache_profile = "node-foundry"

[[runner_classes]]
id = "browser"                    # Playwright-heavy jobs
repo = "owner/poolbet"
prefix = "gh-runner-node-browser"
labels = "self-hosted,linux,x64,nodev2,docker,runner-class-browser"
match_labels = ["self-hosted", "linux", "x64", "nodev2", "docker", "runner-class-browser"]
max_auto_runners = 2
template = "gh-runner-template"
cache_profile = "node-browser"
```

Workflows then route with:

```yaml
runs-on: [self-hosted, linux, x64, nodev2, docker, runner-class-node]
```

## Common mistakes

- **One giant class for everything.** If jobs can share a template and cache
  profile, one class is fine; if they cannot, splitting is cheaper than
  debugging cache thrash and daemon contention.
- **Missing base labels.** A job's `runs-on` must be a subset of the runner's
  labels. If workflows add `nodev2,docker` but the class omits them, jobs queue
  forever.
- **Forgetting the `runner-class-<id>` label** in `runs-on`. Without it, jobs
  match the whole pool by base labels and class demand routing never fires.
- **Per-runner cache tuning.** Layer caches inside an ephemeral clone are
  disposable; only shared volumes and the template persist. Prewarm expensive
  tools into the template or a shared tool cache.
- **Concurrent Buildx writes to one cache path.** Always key the Buildx cache
  path by repo, branch, and image, and write to a `-next` directory before
  atomically replacing the active one.
