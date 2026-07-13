# LXC operator experience and runner-profile design

**Status:** Accepted for implementation planning
**Date:** 2026-07-13
**Scope:** LXC/LXD only; Docker is intentionally deferred.

## Goal

Make the scaler practical for an operator who has never seen this repository:
they should be able to install its prerequisites, choose a sensible runner
profile, create or import a template, configure GitHub routing, validate the
result, and operate it through clear prompted commands.  The same commands
must remain usable non-interactively in scripts and automation.

The product is not a collection of opaque prebuilt images.  It ships useful
profiles as immediately usable defaults and as documented, reproducible
examples that an operator can copy and tailor to their own workload.

## Scope and safety boundary

This work improves the existing LXD-backed service. It does not add, enable,
or select Docker support.  Docker architecture is a later, separate design.

`nodev2` (`192.168.0.170`) remains on its current LXD deployment throughout
this work.  No implementation or verification step may deploy, restart,
rewrite configuration, or otherwise mutate that host.  Integration testing
uses a disposable local LXD project or an explicitly supplied test host.

The existing explicit `[container] provider = "lxd"` and
`[container] template = "..."` configuration remains valid and retains its
current meaning.  A migration must never reinterpret an existing deployment
as an automatically managed profile.

## Operator experience

### Guided first run, scriptable thereafter

`gh-runner-scaler setup` is the first-run entry point. When attached to a
terminal, it presents concise, keyboard-accessible selections and text inputs:

1. Inspect host prerequisites and explain failed checks.
2. Choose local or remote LXD, storage/project, and a default runner profile.
3. Choose an image source: import a published pinned image, build the bundled
   recipe locally, or start a custom recipe from a bundled profile.
4. Enter GitHub target, runner-group scope, class labels, capacity settings,
   cache isolation choice, and optional GPU selection.
5. Show a redacted configuration diff and the exact non-interactive command
   that reproduces the choices.
6. Require an explicit confirmation before it writes configuration, images,
   profiles, or a service unit.
7. Run `config validate` and `doctor`, then present the next safe command.

Every prompt has a documented flag or config equivalent. `--non-interactive`
requires all values that would otherwise be prompted. Secrets are accepted
through environment variables, a file descriptor, or the system secret store
integration when supported; they are never written to TOML, command history,
logs, a rendered diff, or diagnostic bundles.

The command surface is deliberately small:

| Command | Purpose |
| --- | --- |
| `setup` | Guided creation or update of an operator-owned configuration. |
| `config validate` | Strictly parse and cross-check configuration without mutating anything. |
| `doctor` | Run the service identity's read-only readiness checks and explain fixes. |
| `template catalog` | List bundled, locally built, and configured published profiles. |
| `template import` | Import a selected version of a published profile image. |
| `template init` | Copy a profile recipe into an operator-owned custom template. |
| `template build` | Build, sanitize, test, and publish a local LXD image from a recipe. |
| `template publish` | Export an already verified image and catalog metadata for a SimpleStreams publisher. |
| `configure class` | Add or edit a GitHub runner class through the same validated model. |
| `status` | Show class capacity, templates, runner state, cache state, and actionable warnings. |

The existing `reconcile` command remains an explicit operational action, but
the normal service path is not hidden behind the wizard. All mutating commands
display a plan first, write a `.new` file, validate it, preserve a timestamped
backup, and atomically install only after confirmation.

### Preflight and diagnostics

`doctor` is a no-write command and runs checks as the account used by the
service, not merely as the interactive administrator. It verifies:

- Supported Linux architecture, LXD client/server reachability, selected
  project, storage pool, image/template presence, and adequate disk, memory,
  and inodes.
- The exact LXD permissions and remote authentication available to the service
  identity, including remote-image access where configured.
- GitHub connectivity and token/app permissions without echoing credentials;
  runner group and label visibility where the API permits it.
- Template fingerprint/version, runner bootstrap readiness, expected profile
  capability labels, cache volumes, and writable cache paths.
- Webhook listener configuration, metrics configuration, and outbound network
  requirements as independently optional capabilities.

Failures name the failed check, why it matters, a safe fix, and the command to
rerun. A first-run setup must not claim success while metrics, cache, or an
optional integration is silently unusable. Optional integrations instead show
as intentionally disabled or explicitly degraded.

## Configuration and profile model

Configuration gains a versioned, strictly decoded schema. Unknown TOML keys,
misspelled table names, duplicate class IDs, invalid label syntax, impossible
capacity values, unknown profile IDs, and conflicting `template`/`profile`
selection are errors. The command reports locations and suggestions.

Runner classes resolve to exactly one template source:

- `template = "operator-owned-image"` preserves the current direct, manually
  managed LXD template path.
- `profile = "rust"` resolves an operator-selected profile record to a local,
  pinned LXD image fingerprint.

The published profile alias is never the runtime authority: importing or
building writes the chosen release and image fingerprint into local managed
state. Alias refreshes occur only through an explicit update/import command.
This yields repeatable runners and prevents a catalog change from altering a
live workload unexpectedly.

Each profile manifest declares its stable ID, display name, description,
architecture, base OS, release/version, image fingerprint, source recipe,
capabilities, LXD profile/device requirements, bootstrap checks, cache
recommendations, smoke tests, and SBOM/provenance references. Manifests are
human-readable and are the source for the catalog and documentation.

Existing classes continue to use their explicitly configured template. Setup
offers, but never forces, a migration to a profile-backed class. It produces a
preview that states the prior image, selected release, resolved fingerprint,
new labels, and rollback command.

## Bundled runner profiles

All first-party profiles target `linux/amd64` initially. Their recipes use a
supported Ubuntu LTS base and pin the major tooling versions and image release
metadata. Workflow-level setup actions remain supported; the profiles reduce
common cold-start work rather than guessing every private dependency.

| Profile | Purpose and included baseline |
| --- | --- |
| `general` | GitHub Actions runner plus Git, curl, CA certificates, jq, archive tools, Bash, compiler/build essentials, and dependable package bootstrap tools. Suitable when workflows install their language/toolchain with setup actions. |
| `web` | `general` plus supported Node LTS, Corepack, pnpm/Yarn support, common browser/build libraries, and an explicit Playwright browser-install option with a smoke test. |
| `rust` | `general` plus Rustup, stable Rust/Cargo, Clippy, Rustfmt, linker/build dependencies, and a compiled smoke project. |
| `python-data` | `general` plus a supported Python, virtual-environment tooling, pip/uv or pipx policy, compiler and common numerical build prerequisites. Heavy libraries remain workload or derived-template choices. |
| `ai-gpu` | A separately versioned Python-heavy CUDA/PyTorch-ready recipe, only usable once GPU preflight passes. It is resource-expensive and intentionally opt-in. |
| `docs` | `general` plus Node LTS, documentation/static-site build tooling, common image/font libraries, and a reproducible static-site smoke build. |

The AI profile has an additional host compatibility contract. Setup discovers
and displays GPUs but defaults to no attachment. An operator must choose an
identified physical GPU or an already created MIG instance. The generated LXD
device configuration is visible before apply. LXD supports physical GPU
devices for containers and supports MIG devices for containers; MIG instances
must already exist on the host. The profile's tests validate device visibility
and a pinned framework probe inside a disposable runner, not merely that a
package installed. The normal result for a machine without a compatible GPU is
a clear blocked status and a usable non-GPU profile, never a half-configured
AI runner.

Profiles are starting points, not constraints. `template init my-ci --from
rust` copies the recipe, manifest skeleton, package policy, and smoke test into
an operator-owned directory. The catalog marks first-party and local-derived
profiles distinctly and links every first-party profile to its recipe and
customization guide.

## Image building and distribution

The source of truth for every first-party image is a versioned recipe in this
repository. Building an image follows a repeatable pipeline:

1. Resolve the declared base-image digest and recipe/tooling versions.
2. Build in an isolated LXD build project, with no production runner cache.
3. Run profile-specific smoke tests and record their results.
4. Stop the build instance; remove runner registration, tokens, logs, machine
   identity, workspace contents, and other instance-specific metadata.
5. Publish a versioned LXD image alias and record the immutable fingerprint,
   SBOM, recipe revision, base-image source, and build timestamp.

Published artifacts use an HTTPS SimpleStreams catalog plus the source recipes
and manifests. The catalog provides friendly release aliases; runtime imports
pin fingerprints. Documentation includes both `template import` for an
operator who wants a ready default and `template init`/`build` for an operator
who wants full control. Local/offline installations can import the exported
image and manifest from a file without relying on the public catalog.

This aligns with LXD's documented SimpleStreams remotes and image publishing
model: <https://documentation.ubuntu.com/lxd/latest/howto/images_remote/> and
<https://documentation.ubuntu.com/lxd/default/howto/images_create/>. GPU
device support follows the LXD GPU device reference:
<https://documentation.ubuntu.com/lxd/latest/reference/devices_gpu/>.

## Reliable scaler behavior

The user experience must accurately represent what the service will do.
Implementation is staged to correct the current operational hazards:

- Provision multiple eligible queued jobs up to a configured bounded
  concurrency, rather than one runner per reconciliation interval. Capacity
  calculations account for starting instances and a configurable warm buffer.
- Track runner registration and instance lifecycle explicitly. A busy runner is
  never simply forgotten: recovery reconciles GitHub state, LXD state, and a
  bounded orphan policy with clear operator reporting.
- Make cache identity class-scoped by default. Sharing a cache across classes
  requires an explicit, documented trust acknowledgement. Attachment failure
  is a hard failed provision, not an invisible fallback to an instance-local
  directory.
- Scope cache synchronization to the requested class and configured cache
  volume; never select an arbitrary active runner.
- Treat template readiness, registration failure, webhook errors, and cleanup
  failures as structured states visible in `status`, metrics, and logs.

Runner labels describe concrete capabilities from the selected profile and
operator configuration. A `runs-on` mismatch therefore becomes a diagnostic
with suggested labels/profile rather than an unexplained queued job.

## Documentation

Documentation is organized around operator outcomes rather than internal
packages:

- README: a short local-LXD quick start, an explicit security boundary for
  untrusted workflows, and links to the full guide.
- Installation guide: supported Linux prerequisites, LXD initialization,
  service account setup, secret handling, local/remote LXD, and rollback.
- First-run guide: annotated `setup` flow and exact non-interactive equivalent.
- Profile catalog: capabilities, footprint expectations, known limitations,
  release provenance, import/build/customize instructions, and the choice
  between default profiles and a derived template.
- Operations guide: `doctor`, status interpretation, capacity/cost tuning,
  cache isolation, backup/update/rollback, webhook/metrics, and incident
  recovery.
- Security guide: do not expose privileged self-hosted runners to untrusted
  fork/pull-request code; use runner groups and repository scope deliberately.

The stale ignored `CLAUDE.md` is not a reliable public guide. Because this
repository currently has no `AGENTS.md`, the implementation creates a tracked,
concise current contributor/operator guide there and keeps it synchronized with
the command surface, tests, profile recipes, and deployment boundaries.

## Validation and acceptance criteria

Work is delivered in independently reviewable phases:

1. **Safety and configuration:** repair the stale nodev2 configuration test,
   add strict configuration validation and migration behavior, add `doctor`,
   preserve legacy direct-template deployments, and document current LXD
   operation.
2. **Profiles and local recipes:** add all six profile manifests/recipes,
   safe build/sanitize/smoke-test commands, catalog/status discovery, and
   customization documentation.
3. **Published catalog:** generate/export signed-or-checksummed catalog
   artifacts, support pinned import and offline import, and verify provenance
   reporting.
4. **Runtime hardening:** bounded concurrent provisioning, lifecycle/orphan
   reconciliation, cache isolation/attachment correctness, class-safe cache
   synchronization, and clear status/metrics.

Each phase requires deterministic unit tests for its configuration and state
transitions, contract tests for the LXD client boundary, and disposable-LXD
integration tests that never use a production project. Profile builds require
their advertised smoke tests. The AI profile test has a deterministic
no-compatible-GPU path plus an opt-in compatible-host test. Load tests wait
for real GitHub job completion, assert requested capacity and cleanup, and
name their required labels/profile rather than assuming a preconfigured host.

Before any release, the complete Go test suite, vet, build, configuration
fixtures, documentation command checks, and relevant disposable-LXD tests must
pass. `nodev2` is verified only by read-only status comparison; it is not a
test target or deployment target for this project.

## Decisions recorded

- Keep LXC/LXD as the only active execution backend; Docker is deferred.
- Use a guided first-run TUI with complete non-interactive CLI parity.
- Ship `general`, `web`, `rust`, `python-data`, `ai-gpu`, and `docs` profiles
  for amd64, while keeping GPU optional and opt-in.
- Treat recipes as canonical and publish image/catalog artifacts through
  SimpleStreams for plug-and-play imports.
- Route runner classes through profile aliases that resolve to pinned local
  image fingerprints, while preserving direct template selection.
- Make custom/derived templates a primary documented workflow, not an escape
  hatch.
- Make no live changes to nodev2 during design, implementation, or testing.
