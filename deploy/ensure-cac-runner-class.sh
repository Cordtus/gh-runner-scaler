#!/usr/bin/env bash
# Ensure the gh-runner-scaler provisions a cac-group (runner-class-cac) runner class.
# Run as root on the LXD host:  sudo bash deploy/ensure-cac-runner-class.sh
set -euo pipefail
CONFIG="/etc/gh-runner-scaler/config.toml"
if [[ "${EUID}" -ne 0 ]]; then echo "error: run as root" >&2; exit 1; fi
[[ -f "$CONFIG" ]] || { echo "error: $CONFIG missing" >&2; exit 1; }

echo "===== current [ci] + runner classes ====="
grep -nE '^\[|^  (id|org|repo|prefix|labels|match_labels|max_auto_runners|template|enabled) *=' "$CONFIG"

echo "===== does config reference runner-class-cac? ====="
if grep -q 'runner-class-cac' "$CONFIG"; then
  echo "yes - runner-class-cac is configured"
else
  echo "NO - adding cac-group runner class block"
  cp "$CONFIG" "$CONFIG.bak.$(date +%s)"
  {
    echo
    echo '[[runner_classes]]'
    echo 'id = "cac-group"'
    echo 'org = "cac-group"'
    echo 'prefix = "gh-runner-cac"'
    echo 'labels = "self-hosted,linux,x64,runner-class-cac"'
    echo 'match_labels = ["self-hosted", "linux", "x64", "runner-class-cac"]'
    echo 'max_auto_runners = 6'
    echo 'runner_work_dir = "_work"'
    echo 'template = "gh-runner-template"'
  } >> "$CONFIG"
  echo "added runner-class-cac"
fi

echo "===== restart scaler ====="
systemctl restart gh-runner-scaler
sleep 3
systemctl --no-pager --full status gh-runner-scaler --lines=8

echo "===== recent provisioning logs ====="
journalctl -u gh-runner-scaler --since "30 minutes ago" --no-pager 2>/dev/null \
  | grep -iE "runner-class-cac|cac-group|provision|queued job|reconcil|creating runner|start.*runner" | tail -25
