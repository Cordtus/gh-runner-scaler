#!/usr/bin/env bash

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SERVICE_NAME="gh-runner-scaler.service"
BIN_PATH="/usr/local/bin/gh-runner-scaler"
UNIT_PATH="/etc/systemd/system/gh-runner-scaler.service"
LIBEXEC_DIR="/usr/local/libexec/gh-runner-scaler"
DISTRIBUTION_SCRIPT="${LIBEXEC_DIR}/refresh-runner-template.sh"
LXC_READY_SCRIPT="${LIBEXEC_DIR}/wait-for-lxc-ready.sh"
ARCHIVE_VERIFY_SCRIPT="${LIBEXEC_DIR}/verified-runner-archive.sh"
DISTRIBUTION_SERVICE="gh-runner-distribution-refresh.service"
DISTRIBUTION_TIMER="gh-runner-distribution-refresh.timer"
CONFIG_DIR="/etc/gh-runner-scaler"
CONFIG_PATH="${CONFIG_DIR}/config.toml"
CONFIG_SOURCE="${GH_RUNNER_SCALER_CONFIG_SOURCE:-}"
ENV_PATH="${CONFIG_DIR}/env"
STATE_DIR="/var/lib/gh-runner-scaler/state"

PATH="/usr/local/go/bin:${PATH}"
export PATH

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

echo "Installing binary, maintenance scripts, and systemd units"
run_as_root install -d "${CONFIG_DIR}" "${STATE_DIR}" "${LIBEXEC_DIR}"
run_as_root install -m 0755 "${TMP_BIN}" "${BIN_PATH}"
run_as_root install -m 0755 "${REPO_ROOT}/deploy/refresh-runner-template.sh" "${DISTRIBUTION_SCRIPT}"
run_as_root install -m 0755 "${REPO_ROOT}/deploy/wait-for-lxc-ready.sh" "${LXC_READY_SCRIPT}"
run_as_root install -m 0755 "${REPO_ROOT}/deploy/verified-runner-archive.sh" "${ARCHIVE_VERIFY_SCRIPT}"
run_as_root install -m 0644 "${REPO_ROOT}/deploy/systemd/gh-runner-scaler.service" "${UNIT_PATH}"
run_as_root install -m 0644 "${REPO_ROOT}/deploy/systemd/${DISTRIBUTION_SERVICE}" "/etc/systemd/system/${DISTRIBUTION_SERVICE}"
run_as_root install -m 0644 "${REPO_ROOT}/deploy/systemd/${DISTRIBUTION_TIMER}" "/etc/systemd/system/${DISTRIBUTION_TIMER}"

if [[ -n "${CONFIG_SOURCE}" ]]; then
  if [[ "${CONFIG_SOURCE}" != /* ]]; then
    CONFIG_SOURCE="${REPO_ROOT}/${CONFIG_SOURCE}"
  fi
  if [[ ! -f "${CONFIG_SOURCE}" ]]; then
    echo "error: requested config source does not exist: ${CONFIG_SOURCE}" >&2
    exit 1
  fi
  echo "Installing config from ${CONFIG_SOURCE}"
  run_as_root install -m 0644 "${CONFIG_SOURCE}" "${CONFIG_PATH}"
elif [[ -f "${REPO_ROOT}/config.toml" && ! -f "${CONFIG_PATH}" ]]; then
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

service_was_active=false
if run_as_root systemctl is-active --quiet "${SERVICE_NAME}" 2>/dev/null; then
  service_was_active=true
  run_as_root systemctl stop "${SERVICE_NAME}"
fi

distribution_enabled="$(
  awk '
    /^\[runner_distribution\]$/ { in_section=1; next }
    /^\[/ { in_section=0 }
    in_section && $1 == "enabled" {
      gsub(/[[:space:]]/, "", $3)
      print $3
      exit
    }
  ' "${CONFIG_PATH}"
)"
if [[ "${distribution_enabled}" == "true" ]]; then
  echo "Refreshing the verified runner distribution before restarting the scaler"
  if ! run_as_root systemctl start "${DISTRIBUTION_SERVICE}"; then
    [[ "${service_was_active}" == "false" ]] || run_as_root systemctl start "${SERVICE_NAME}"
    exit 1
  fi
  run_as_root systemctl enable --now "${DISTRIBUTION_TIMER}"
else
  run_as_root systemctl disable --now "${DISTRIBUTION_TIMER}" 2>/dev/null || true
fi

observability_enabled="$(
  awk '
    /^\[runner_observability\]$/ { in_section=1; next }
    /^\[/ { in_section=0 }
    in_section && $1 == "enabled" {
      gsub(/[[:space:]]/, "", $3)
      print $3
      exit
    }
  ' "${CONFIG_PATH}"
)"
if [[ "${observability_enabled}" == "true" ]]; then
  echo "Preparing the stopped runner template for bounded log delivery"
  if ! "${REPO_ROOT}/deploy/prepare-runner-template-observability.sh" gh-runner-template; then
    [[ "${service_was_active}" == "false" ]] || run_as_root systemctl start "${SERVICE_NAME}"
    exit 1
  fi
fi

if run_as_root systemctl is-enabled --quiet "${SERVICE_NAME}" 2>/dev/null; then
  run_as_root systemctl restart "${SERVICE_NAME}"
else
  run_as_root systemctl enable --now "${SERVICE_NAME}"
fi

run_as_root systemctl --no-pager --full status "${SERVICE_NAME}" --lines=20
