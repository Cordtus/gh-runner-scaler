# Runner orchestration runbook

## Normal state

- `gh-runner-primary` is the sole persistent Poolbet Node/Docker baseline.
- Other class containers exist only while their class has queued work or a job is running.
- `/var/lib/gh-runner-scaler/state/demand.json` survives daemon restarts and expires abandoned entries after 30 minutes.
- `/var/lib/gh-runner-scaler/runner-distributions/current-version` records the verified template version.
- The distribution timer checks daily; an unchanged version performs no download or LXD mutation.

## Deploy

From the repository checkout on nodev2:

```bash
sudo ./deploy/deploy-runner-observability.sh
```

The deployment builds and installs the daemon, installs the nodev2 config and
systemd units, refreshes the verified runner distribution before restarting the
scaler, clears inherited template logs, and verifies the service endpoints.

## Verify

```bash
systemctl status gh-runner-scaler.service
systemctl status gh-runner-distribution-refresh.timer
systemctl status gh-runner-distribution-refresh.service
curl -fsS http://127.0.0.1:9876/statusz | jq .
lxc list 'gh-runner-*'
cat /var/lib/gh-runner-scaler/runner-distributions/current-version
journalctl -u gh-runner-scaler.service --since '-15 min'
```

With no queued jobs, only `gh-runner-primary` should remain. Dispatch one
class-labelled job and confirm the Grafana “Demand, Baseline, and Overflow”
panel moves from queued demand to overflow and back to zero.

## Failure handling and rollback

A metadata, download, checksum, or template-install failure leaves the prior
verified archive and template `current` link in place. Inspect:

```bash
journalctl -u gh-runner-distribution-refresh.service -n 100
systemctl reset-failed gh-runner-distribution-refresh.service
systemctl start gh-runner-distribution-refresh.service
```

To roll back, stop the template, repoint
`/home/runner/actions-runner/current` inside it to one of the two retained
version directories, stop the template again, and restart the scaler. Never
change a live busy runner.
