#!/usr/bin/env bash
# Diagnose why gh-runner-scaler workflow-metrics are empty.
# Run as root on the LXD host running the scaler (e.g. nodev2):
#   sudo bash deploy/diagnose-workflow-metrics.sh
set -euo pipefail

CONFIG="/etc/gh-runner-scaler/config.toml"
ENV_FILE="/etc/gh-runner-scaler/env"

echo "===== $CONFIG : [ci] and [metrics] sections ====="
awk '/^\[ci\]/{f=1} f&&/^\[/&&!/^\[ci\]/{exit} f{print}' "${CONFIG}" 2>/dev/null
awk '/^\[metrics\]/{f=1} f&&/^\[/&&!/^\[metrics\]/{exit} f{print}' "${CONFIG}" 2>/dev/null

echo
echo "===== per-runner-class ci overrides (org/repo/labels) ====="
awk '/^\[\[runner_classes\]\]/{print "\n["$0"]"} /^  (id|org|repo|prefix|labels|template) *=/{print}' "${CONFIG}" 2>/dev/null

echo
echo "===== scaler logs: workflow-metrics collection (last 24h) ====="
journalctl -u gh-runner-scaler --since "24 hours ago" --no-pager 2>/dev/null \
  | grep -iE "workflow|metric|listworkflow|listrepo|listorg|ci\.org|ci\.repo|collect" \
  | tail -40 || echo "(journalctl returned nothing)"

echo
echo "===== is workflow_metrics_seen.json present? (dedup cache) ====="
ls -la /var/lib/gh-runner-scaler/state/workflow_metrics_seen.json 2>/dev/null || echo "not present (fresh dedup cache)"

echo
echo "===== scaler version ====="
/usr/local/bin/gh-runner-scaler --version 2>/dev/null || echo "(no --version flag)"
