#!/usr/bin/env bash

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SERVICE_NAME="gh-runner-scaler.service"
BIN_PATH="/usr/local/bin/gh-runner-scaler"
UNIT_PATH="/etc/systemd/system/gh-runner-scaler.service"
CONFIG_DIR="/etc/gh-runner-scaler"
CONFIG_PATH="${CONFIG_DIR}/config.toml"
ENV_PATH="${CONFIG_DIR}/env"
STATE_DIR="/var/lib/gh-runner-scaler/state"

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

if ! command -v go >/dev/null 2>&1; then
  echo "error: go is required on the server to run this update script" >&2
  exit 1
fi

TMP_BIN="$(mktemp)"
trap 'rm -f "${TMP_BIN}"' EXIT

echo "Building gh-runner-scaler from ${REPO_ROOT}"
(
  cd "${REPO_ROOT}"
  go build -o "${TMP_BIN}" ./cmd/scaler
)

echo "Installing binary and systemd unit"
run_as_root install -d "${CONFIG_DIR}" "${STATE_DIR}"
run_as_root install -m 0755 "${TMP_BIN}" "${BIN_PATH}"
run_as_root install -m 0644 "${REPO_ROOT}/deploy/systemd/gh-runner-scaler.service" "${UNIT_PATH}"

if [[ -f "${REPO_ROOT}/config.toml" && ! -f "${CONFIG_PATH}" ]]; then
  echo "Installing missing config from repo checkout"
  run_as_root install -m 0644 "${REPO_ROOT}/config.toml" "${CONFIG_PATH}"
fi

if [[ ! -f "${CONFIG_PATH}" ]]; then
  echo "error: ${CONFIG_PATH} does not exist; create it before running this update script" >&2
  exit 1
fi

if [[ ! -f "${ENV_PATH}" ]]; then
  echo "error: ${ENV_PATH} does not exist; create it before running this update script" >&2
  exit 1
fi

echo "Reloading and restarting ${SERVICE_NAME}"
run_as_root systemctl daemon-reload
if run_as_root systemctl is-enabled --quiet "${SERVICE_NAME}" 2>/dev/null; then
  run_as_root systemctl restart "${SERVICE_NAME}"
else
  run_as_root systemctl enable --now "${SERVICE_NAME}"
fi

run_as_root systemctl --no-pager --full status "${SERVICE_NAME}" --lines=20
