# Runner Observability Delivery Design

## Goal

Retain complete ephemeral-runner telemetry: scaler lifecycle and capacity metrics, plus searchable GitHub Actions runner diagnostic and job logs, without replaying template history or creating an unbounded retry load.

## Confirmed failure

Every runner clone inherits a manually configured `promtail.service`. It scans retained `Worker_*.log` files in the template and sends them to Grafana Cloud. Fresh clones do not have meaningful positions for those historical files, while the configured cloud credential returns HTTP 401. Each running clone therefore replays old diagnostics and continuously retries rejected batches.

The scaler's own structured metrics are independent of Promtail and remain the authoritative source for capacity, lifecycle, workflow, host, and issue-event panels.

## Chosen architecture

Keep real-time per-runner log shipping, but make it a managed runner-observability feature rather than unmanaged template state.

1. The scaler continues to push its structured metrics through its existing Loki backend.
2. The stopped runner template contains no retained `_diag` or `_work` log files and does not autostart a manually configured Promtail service.
3. During runner provisioning, the scaler installs a generated, per-runner Promtail configuration and starts the shipper only after the runner has registered.
4. The generated configuration uses the internal Loki endpoint. Runner containers never contain a Grafana Cloud credential.
5. Log streams carry stable labels for the runner group, container, repository target, and log kind. The source set is limited to current-runner diagnostics and job logs.
6. Promtail stores offsets only for that runner's lifetime. Containers are deleted after cleanup, so stale offsets cannot hide future runner logs and stale template logs cannot be replayed.
7. Before Promtail starts, the provisioner validates that the configured endpoint is reachable and accepts a small health request. A failed preflight leaves runner execution available, records one scaler issue event, and suppresses log-shipping startup for that runner.
8. The generated client configuration uses bounded exponential backoff, a maximum retry count, bounded batches, and a maximum source-file size. Permanent authentication and validation failures are not retried.
9. The scaler emits observability-health fields and issue events for preflight failure, delivery failure, dropped batches, and retry exhaustion. Lifecycle metrics continue even if log delivery is unavailable.

## Data flow

```text
GitHub job -> ephemeral LXC runner -> generated Promtail -> internal Loki -> Grafana
                     |                         |
                     +-> scaler lifecycle metrics+
                              -> internal Loki -> Grafana
```

The runner process must never wait for log delivery. Log shipping is best-effort observability, with bounded resource use and explicit failure telemetry.

## Guardrails

- No service credentials in the runner template.
- No recursive scan of template-inherited historical logs.
- No start of the shipper until runner registration succeeds.
- No retry for HTTP 400, 401, 403, or 4xx errors other than 429.
- Retry only temporary transport errors, 429, and 5xx responses; use exponential backoff with jitter and a hard attempt limit.
- On retry exhaustion, drop the affected batch, increment a counter, and emit one rate-limited issue event rather than retrying indefinitely.
- Cap source-file count, individual size, batch size, and total bytes per runner lifecycle. Emit a truncation event when a cap applies.
- Preserve source timestamps and runner/job metadata so Grafana queries remain useful after container deletion.

## Testing and acceptance

Unit and component tests must prove that:

- provisioning generates a runner-specific configuration with no credential material;
- a template with retained logs fails the template-readiness check;
- 401/403 failures are classified as permanent and do not retry;
- transient failures back off, recover when possible, and stop at the retry limit;
- endpoint preflight failure does not block runner registration or lifecycle metrics;
- source and batch caps produce observable truncation/drop events;
- log entries carry the expected stable labels and timestamps.

Live acceptance requires a disposable runner lifecycle: one current job's diagnostics and job logs are visible in Grafana, no historical logs appear from the template, a forced rejected endpoint produces one bounded issue event, and nodev2's load remains stable while idle runners are present.

## Out of scope

- Changing Grafana/Loki retention policy.
- Rewriting existing runner lifecycle metrics or dashboard queries.
- Retaining an external Grafana Cloud credential inside ephemeral runner containers.
