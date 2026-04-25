#!/usr/bin/env bash

set -euo pipefail

SERVICE_NAME="${SERVICE_NAME:-gh-runner-scaler.service}"
OUT_DIR="${1:-/tmp/gh-runner-scaler-loadtest-$(date +%Y%m%d-%H%M%S)}"
INTERVAL_SECONDS="${INTERVAL_SECONDS:-15}"
DURATION_SECONDS="${DURATION_SECONDS:-900}"
START_TIME="$(date -Iseconds)"

run_as_root() {
  if [[ "${EUID}" -eq 0 ]]; then
    "$@"
    return
  fi
  if command -v sudo >/dev/null 2>&1; then
    sudo "$@"
    return
  fi
  echo "error: root privileges are required for: $*" >&2
  exit 1
}

mkdir -p "${OUT_DIR}"
mkdir -p "${OUT_DIR}/snapshots"

capture_snapshot() {
  local timestamp snapshot_dir
  timestamp="$(date +%Y%m%d-%H%M%S)"
  snapshot_dir="${OUT_DIR}/snapshots/${timestamp}"
  mkdir -p "${snapshot_dir}"

  run_as_root systemctl --no-pager --full status "${SERVICE_NAME}" > "${snapshot_dir}/systemctl-status.txt"

  if command -v lxc >/dev/null 2>&1; then
    run_as_root lxc list --format json > "${snapshot_dir}/lxc-list.json" || true
    run_as_root lxc storage list --format json > "${snapshot_dir}/lxc-storage-list.json" || true
  fi

  run_as_root df -h > "${snapshot_dir}/df.txt"
  run_as_root free -m > "${snapshot_dir}/free.txt"
}

finish() {
  run_as_root journalctl -u "${SERVICE_NAME}" --since "${START_TIME}" --no-pager > "${OUT_DIR}/journalctl.log" || true
  echo "Captured server evidence in ${OUT_DIR}"
}

trap finish EXIT

end_epoch=0
if (( DURATION_SECONDS > 0 )); then
  end_epoch="$(( $(date +%s) + DURATION_SECONDS ))"
fi

first_snapshot=1
while true; do
  if (( first_snapshot == 0 )) && (( end_epoch > 0 )) && (( $(date +%s) >= end_epoch )); then
    break
  fi

  capture_snapshot
  first_snapshot=0

  if (( end_epoch > 0 )) && (( $(date +%s) >= end_epoch )); then
    break
  fi

  sleep_seconds="${INTERVAL_SECONDS}"
  if (( end_epoch > 0 )); then
    remaining_seconds="$(( end_epoch - $(date +%s) ))"
    if (( remaining_seconds < sleep_seconds )); then
      sleep_seconds="${remaining_seconds}"
    fi
  fi
  sleep "${sleep_seconds}"
done
