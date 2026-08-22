#!/usr/bin/env bash
set -euo pipefail

TEMPLATE="${RUNNER_TEMPLATE:-gh-runner-template}"
APT_FLAG="${RUNNER_TOOLCHAIN_APT:-0}"

for command in lxc; do
  command -v "${command}" >/dev/null || {
    echo "error: required command is missing: ${command}" >&2
    exit 1
  }
done

template_started=0
cleanup() {
  if [[ "${template_started}" -eq 1 ]]; then
    lxc stop "${TEMPLATE}" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

status="$(lxc info "${TEMPLATE}" | awk '/^Status:/ {print $2}')"
if [[ "${status}" != "stopped" ]]; then
  echo "error: template ${TEMPLATE} must be stopped, found ${status:-unknown}" >&2
  exit 1
fi

echo "starting template ${TEMPLATE} for toolchain refresh..."
lxc start "${TEMPLATE}"
template_started=1

for _ in $(seq 1 30); do
  if lxc exec "${TEMPLATE}" -- true >/dev/null 2>&1; then
    break
  fi
  sleep 2
done

exec_in() {
  lxc exec "${TEMPLATE}" -- bash -lc "$1" || true
}

echo "refreshing toolchains and caches in ${TEMPLATE}..."

# Rust: bump stable and force a fresh crates.io index (prevents stale --locked resolution).
exec_in 'command -v rustup >/dev/null && rustup update stable --no-self-update'
exec_in 'command -v cargo >/dev/null && cargo search --limit 1 serde >/dev/null 2>&1'

# Node / npm global packages.
exec_in 'command -v npm >/dev/null && npm update -g --no-audit --no-fund'

# Yarn cache.
exec_in 'command -v yarn >/dev/null && yarn cache clean'

# Python / pip.
exec_in 'command -v pip3 >/dev/null && pip3 install --upgrade pip >/dev/null 2>&1'

# Optional full system package update (opt-in; the template is a curated image).
if [[ "${APT_FLAG}" == "1" ]]; then
  exec_in 'export DEBIAN_FRONTEND=noninteractive && apt-get update -y && apt-get upgrade -y'
fi

echo "toolchain refresh complete for ${TEMPLATE}"
lxc stop "${TEMPLATE}"
template_started=0
