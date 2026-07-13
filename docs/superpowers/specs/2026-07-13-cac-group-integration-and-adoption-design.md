# CAC Group integration and newcomer-adoption design

**Status:** Accepted design; implementation pending written-spec review
**Date:** 2026-07-13
**Scope:** Use cac-group as the real organization-scale acceptance exercise for the LXD backend and turn every observed setup hurdle into a reusable operator experience. Docker remains out of scope.

## Purpose

The platform should make a new operator successful without knowing its creator's host, token conventions, LXD layout, runner labels, or GitHub Actions internals. The cac-group rollout is the proving ground: it must set up a least-privilege organization integration, route suitable real workloads, provision several LXD runners concurrently, and leave a clear audit trail and rollback path.

The desired outcome is not merely that cac-group works. It is that a future operator can repeat the same guided process on their own Linux hardware using their own GitHub organization or repository, with sensible defaults and an obvious route to custom templates.

## Facts discovered from the CAC Group exercise

The design and tests use observed facts rather than a hypothetical deployment:

- cac-group has 15 active repositories, but only three currently contain active Actions workflows: cacmin-bot, stargaze-roi, and demo-repository.
- None of those workflows presently selects runner-class-cac, so enabling an organization runner alone would not move a job to the scaler.
- cacmin-bot has a native ARM release job that cannot run on the current amd64 LXD host. Its four parallel Bun CI jobs are suitable for an amd64 concurrency test.
- stargaze-roi exercises Go and docker build; demo-repository supplies a small static-site workload. Together they provide a realistic cross-project test mix.
- The current gh-runner-template clone has Git, Node/npm, Bun, Python, and a working Docker server, but no preinstalled Go or Rust. Setup actions can provision Go; named profiles must make richer toolchain guarantees explicit.
- The live scaler is reachable through https://gh-webhook.basementnodes.ca, and its current status shows accepted Axionic webhook events even though the checked CAC configuration describes a CAC target. This proves that the single global webhook secret/endpoint is unsuitable for independently managed targets.
- The active gh account is a cac-group administrator, but its classic credential lacks admin:org, so it cannot read or manage organization runners. The production service credential must not be silently substituted with that user token.
- The current reconciler creates at most one runner in a reconciliation pass. It cannot satisfy a six-job burst as an autoscaler, even with max_auto_runners = 6.

## Security and rollout boundaries

The CAC integration starts with a dedicated organization runner group named cac-scaler, visible only to cacmin-bot, stargaze-roi, and demo-repository. It does not grant every cac-group repository access on day one. The group expands only after the acceptance run passes and the operator explicitly selects additional repositories.

Public or fork-triggered pull-request workflows remain GitHub-hosted by default. A self-hosted runner can execute arbitrary workflow code with access to its network and anything mounted or available to the runner; an ephemeral container reduces residue but does not make untrusted code safe. The workflow advisor must flag this condition and refuse a one-click retarget unless the operator records an explicit override.

The existing nodev2 deployment is not changed during design. When the approved implementation reaches live integration, each host mutation is previewed, backed up, validated, and performed only as part of the CAC acceptance rollout. Existing non-CAC webhook traffic must continue to work without sharing CAC's secret or route.

## Target-based integration model

The current global [ci] token and global webhook secret are retained only as a legacy single-target configuration. New deployments use named targets.

    [[targets]]
    id = "cac-group"
    provider = "github"
    kind = "organization"
    org = "cac-group"
    credential_env = "GH_SCALER_TARGET_CAC_GROUP_TOKEN"
    webhook_secret_env = "GH_SCALER_TARGET_CAC_GROUP_WEBHOOK_SECRET"
    webhook_path = "/webhooks/cac-group"
    runner_group = "cac-scaler"
    allowed_repositories = [
      "cac-group/cacmin-bot",
      "cac-group/stargaze-roi",
      "cac-group/demo-repository",
    ]

    [[runner_classes]]
    id = "cac-web"
    target = "cac-group"
    profile = "cac-web-docker"
    labels = "self-hosted,linux,x64,runner-class-cac-web"
    match_labels = ["self-hosted", "linux", "x64", "runner-class-cac-web"]
    max_auto_runners = 6
    max_provisioning = 3

The exact serialized schema may evolve during implementation, but these invariants do not:

- A runner class belongs to one named target and obtains its GitHub client, registration URL, runner group, token, and webhook validation only from that target.
- Every target has a distinct route and HMAC secret. Signature validation chooses the target by route before parsing or dispatching the event.
- A class cannot be configured for an organization repository outside its target's allowed-repository list.
- The service never logs a token, HMAC secret, or a command line containing one. Environment variable names may be shown; values may not.
- Legacy direct-template classes retain their current configuration and become a synthesized default target only when no named targets are configured.

The runner registration command includes the configured GitHub runner group. GitHub documents selected-repository runner groups and configuration with config.sh --runnergroup; the service must use that capability rather than registering into the broad default group.

## Guided adoption flow

gh-runner-scaler setup becomes a terminal-first guided workflow with exact non-interactive equivalents. The CAC exercise defines its required screens and their safe defaults.

### 1. Discover and diagnose

The wizard starts with a no-write doctor pass that reports Linux architecture, LXD access as the service account, clone storage pool, template state, Docker capability where requested, available disk/RAM/inodes, outbound GitHub connectivity, and publicly reachable HTTPS webhook URL. It inspects the selected GitHub organization read-only and summarizes active workflows, runs-on labels, architectures, Docker/service-container use, trigger types, and repository visibility.

For CAC, this produces three actionable classifications:

| Workflow | Classification | Default recommendation |
| --- | --- | --- |
| cacmin-bot Build and Release | ARM64 release path | Keep GitHub-hosted ARM. |
| cacmin-bot CI | Trusted manual/push candidate; four parallel Bun jobs | Route only explicit trusted dispatches or protected-main pushes to cac-web. |
| stargaze-roi CI | Go plus Docker candidate; public PR trigger | Use a trusted dispatch/branch acceptance run first; do not route public PRs by default. |
| demo-repository Proof HTML | Low-resource static-site candidate | Use in the cross-project acceptance run. |

The summary distinguishes unsupported, unsafe-by-default, and ready workloads. It never presents an incompatible ARM job as a generic failure.

### 2. Select a profile or customize one

The wizard lists first-party general, web, rust, python-data, ai-gpu, and docs profiles with package guarantees, architecture, resource expectations, last verified release, source recipe, and smoke test. It offers:

- **Use published profile** — import a pinned SimpleStreams release.
- **Build bundled recipe locally** — reproduce the profile in an isolated LXD build project.
- **Create a custom template** — copy a selected profile recipe and smoke test to an operator-owned directory, then amend packages/tooling deliberately.
- **Use existing template** — retain gh-runner-template, mark it as operator-managed, and run only capability checks.

For the first CAC acceptance run, a local cac-web-docker profile derived from web is the recommended choice. Its manifest adds the nested-LXD Docker contract and smoke test required by stargaze-roi; the standard web profile continues to promise only its Node/browser tooling. The wizard explains that Go uses actions/setup-go until a derived Go profile is selected. It does not claim that either profile is a Rust or GPU image.

### 3. Create narrowly scoped GitHub credentials

The setup flow generates no GitHub credential itself. GitHub requires the operator's authenticated browser to approve a fine-grained personal access token, and the tool must not ask users to paste credentials into a config file or shell history.

Instead, it opens or prints the exact GitHub token-creation URL and a concise permission checklist:

| Scope | Permission | Reason |
| --- | --- | --- |
| Organization | Self-hosted runners: write | Registration/removal tokens, runner inventory, and selected runner group membership. |
| Organization | Webhooks: write | Create, ping, inspect, and remove the target-specific hook. |
| Organization | Administration: write | Restrict organization self-hosted-runner policy to selected repositories. |
| Repository (selected repositories only) | Actions: read | Read queued workflow jobs and verify acceptance runs. |
| Repository (selected repositories only) | Metadata: read | Resolve selected repository access and workflow metadata. |

The TUI reads the token with terminal echo disabled, writes it only to the service environment file with mode 0600, confirms it with non-secret API calls, and advises a user-selected expiry plus rotation date. A GitHub App is a future alternative once the scaler can mint and rotate installation tokens; the first implementation must not pretend a static PAT has automatic rotation.

### 4. Provision target infrastructure

The wizard generates a 32-byte target-specific webhook secret, creates or reuses cac-scaler with selected-repository visibility, applies the organization self-hosted-runner policy for those repositories, and creates a GitHub organization webhook for only workflow_job events. Push is not subscribed until the operator enables a class-scoped cache-sync policy.

It creates an HTTPS reverse-proxy route such as https://gh-webhook.basementnodes.ca/webhooks/cac-group and sends a GitHub ping before enabling the daemon. The route exposes only webhook POST, healthz, and authenticated diagnostics; it does not expose logs or a broad unprotected status endpoint. The corresponding target configuration and environment file are rendered as a redacted plan, validated, backed up, and applied atomically.

If the user selects an external reverse proxy, setup emits a compact ready-to-paste Caddy/Nginx configuration and leaves it unapplied. A follow-up doctor waits for the public route before creating the GitHub webhook. This makes the local-host, remote-LXD, and external-proxy paths all explicit rather than assuming nodev2 conventions.

### 5. Recommend workflow edits safely

The workflow advisor produces a per-job patch preview. It does not rewrite all ubuntu-latest labels blindly. It adds an explicit workflow_dispatch acceptance route to a temporary trusted branch and uses the target label only for the selected test jobs. It preserves ARM labels, public/fork PR workflows, and deployment jobs unless the operator approves each exception.

After a successful acceptance run, the advisor offers an intentional production policy per job: GitHub-hosted, self-hosted on protected main pushes, self-hosted for trusted manual dispatch only, or excluded. Each patch contains the rollback commit and a short reason recorded in the repository.

### 6. Verify and hand off

status --target cac-group provides an operator-level report: selected repositories, runner group, profile image/fingerprint, configured capacity, active and starting runners, queue depth, recent webhook delivery, last provisioning error, cache isolation, and cleanup state. doctor --bundle creates a redacted support bundle with command results and release metadata.

## Concurrent acceptance run

The test does not depend on incidental workflow duration. Setup creates temporary trusted branches in the three selected CAC repositories and commits a minimal, explicitly named runner-scaler-acceptance.yml in each branch. The workflow has workflow_dispatch only, requests the target's custom label, and contains the project-relevant check plus a bounded hold so parallel execution is observable.

The coordinator dispatches six jobs in a short window:

| Repository | Jobs | Workload |
| --- | ---: | --- |
| cacmin-bot | 4 | Existing Bun install/build/test/typecheck/lint pattern, each held long enough to overlap. |
| stargaze-roi | 1 | Go test/build plus Docker build. |
| demo-repository | 1 | Static HTML validation. |

The scaler must create up to six runners while respecting both max_auto_runners = 6 and max_provisioning = 3. It may provision in bounded parallel waves but must not wait for a 30-second polling cycle to create a single runner per pass. A target-scoped queue calculation counts queued jobs, usable idle runners, runners starting, and the configured warm buffer.

The run passes only when all of the following are observed:

1. GitHub delivers signed workflow_job events successfully to the CAC route.
2. Every job runs on a cac-scaler runner with the requested custom label.
3. Six jobs overlap, no job remains queued due to an unprovisioned capacity slot, and runner names can be traced to LXD instances and log entries.
4. The Go/Docker job proves a usable Docker daemon, not merely a docker executable on PATH.
5. On completion, the ephemeral runner registrations, LXD instances, state records, cache mounts, and temporary acceptance branches are cleaned up or explicitly reported for operator action.
6. The organization runner group remains visible only to the three selected repositories and other configured targets continue to receive their own webhook traffic.

The test is retried only after a failure has a classified cause: GitHub policy, credential scope, public webhook delivery, runner-group routing, template capability, LXD capacity, scaler provisioning, workflow failure, or cleanup. It must not retry by silently increasing caps or broadening repository access.

## Product changes revealed by the exercise

| Observed friction | Product response | Acceptance evidence |
| --- | --- | --- |
| Credential scopes are opaque and the existing token failed with 403. | Target credential checklist, secure token input, permission probe, expiry/rotation reminder. | doctor names the missing permission and passes the non-secret API checks. |
| One global secret accepted unrelated target traffic. | Named targets with distinct paths/secrets and route-first validation. | A signed webhook for target A cannot trigger or appear in target B state. |
| No runner group or repository allow-list model exists. | Target provisioning creates/reuses selected-repository group and validates policy. | GitHub API shows exactly the selected repositories. |
| Workflows are treated as labels rather than security-sensitive programs. | Advisor classifies architecture, triggers, Docker, public/fork exposure, and patches only selected trusted jobs. | ARM and public PR workflows remain unchanged unless explicitly approved. |
| Current scaler creates one runner per pass. | Target-scoped desired-capacity planner and bounded concurrent provisioner. | Six-job CAC run reaches capacity within declared bounds. |
| Existing templates have unknown capability guarantees. | Profiles/manifests plus local-template inspection and smoke tests. | Chosen profile capability report matches the actual job results. |
| Webhook/reverse-proxy setup is host-specific. | Proxy-aware setup with managed or generated configuration, HTTPS validation, and ping verification. | GitHub ping and external health route pass before hook activation. |
| Cache paths can cross workloads and silently fall back. | Class-scoped cache defaults, explicit sharing acknowledgement, attachment as a provisioning prerequisite. | Each CAC class has its expected cache and attachment failures stop the runner. |
| Diagnostics require knowing several host commands. | doctor, status, and a redacted support bundle. | A new operator can identify a failed prerequisite without SSHing into source code. |

## Delivery sequence

This is intentionally split into working increments; no phase needs a broad organization rollout to be useful.

1. **Target safety foundation** — strict target configuration, legacy compatibility, route-first per-target webhooks, target-scoped GitHub clients, runner-group configuration, and minimum-permission checks.
2. **Capacity and lifecycle correctness** — desired-capacity calculation, bounded concurrent provisioning, hard cache-attachment failure, target/class-safe cache synchronization, and orphan-safe cleanup.
3. **Operator commands** — config validate, doctor, status, target provisioning plan/apply/rollback, secure secret handling, and generated reverse-proxy guidance.
4. **Templates and workflow advice** — first-party profile recipes/builds, custom-template bootstrap, capability smoke tests, workflow analysis, and safe patch previews.
5. **CAC acceptance and documentation** — selected runner group, scoped credential/webhook, trusted temporary workflow branches, six-job exercise, cleanup, and a concise reusable organization-adoption guide.

Each increment has unit tests, configuration fixtures, disposable-LXD integration coverage where applicable, and a human-readable rollback path. CAC live writes occur only in the final increment after the earlier increments are tested and reviewed locally.

## Documentation deliverables

The repository's user-facing guidance is organized around real operator tasks:

- **Quick start:** one small local LXD target using the default general profile.
- **Organization adoption:** the CAC-derived setup flow, least-privilege permissions, runner groups, webhook/proxy setup, and workflow selection.
- **Profile catalog:** default image purpose, package guarantees, resource expectations, import/build/customize paths, and smoke tests.
- **Workflow routing and security:** label selection, architecture mismatches, Docker requirements, trusted versus fork/PR workloads, and rollback.
- **Operations:** capacity tuning, cache isolation, secret rotation, health, diagnostics, incident recovery, backup, and upgrade.
- **Acceptance guide:** how to run a bounded multi-repository concurrency test without turning every organization workflow into a self-hosted workload.

The tracked contributor/operator guide is updated alongside commands and configuration so a maintainer receives the same boundaries and verification steps as an end user.

## External contracts

The design relies on GitHub's documented organization runner groups, selected repository access, --runnergroup configuration support, minimum REST permissions, and self-hosted-runner security guidance:

- https://docs.github.com/en/actions/how-tos/manage-runners/self-hosted-runners/manage-access
- https://docs.github.com/en/rest/actions/self-hosted-runners
- https://docs.github.com/en/rest/orgs/webhooks
- https://docs.github.com/en/rest/actions/permissions
- https://docs.github.com/en/actions/reference/security/secure-use

## Decisions recorded

- Use cac-group as the real, staged organization acceptance target.
- Start with a selected-repository runner group, not organization-wide access.
- Use a fine-grained PAT with explicit minimal permissions for the initial static-token implementation; do not promote the current interactive token into a production service credential.
- Use target-specific webhook paths and secrets; the current global secret is legacy-only.
- Preserve ARM and untrusted PR workflows on GitHub-hosted runners by default.
- Prove six concurrent cross-project jobs before expanding access.
- Treat the documented operator flow as a product contract and automate every repeated, host-specific setup step that the CAC exercise exposes.
