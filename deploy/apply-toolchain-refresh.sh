#!/usr/bin/env bash
# Installs the weekly runner-template toolchain refresh and runs it once.
# Run as root on the LXD host that owns gh-runner-template (e.g. nodev2).
#
#   sudo bash deploy/apply-toolchain-refresh.sh [/path/to/gh-runner-scaler]
set -euo pipefail

REPO="${1:-/root/gh-runner-scaler}"
LIBEXEC="/usr/local/libexec/gh-runner-scaler"
UNIT_DIR="/etc/systemd/system"
SERVICE="gh-runner-toolchain-refresh.service"
TIMER="gh-runner-toolchain-refresh.timer"

if [[ "${EUID}" -ne 0 ]]; then
  echo "error: run as root (sudo)" >&2
  exit 1
fi
for command in git lxc systemctl install; do
  command -v "${command}" >/dev/null || { echo "error: missing command: ${command}" >&2; exit 1; }
done

# 1. Ensure the gh-runner-scaler repo is present and on latest main.
if [[ ! -d "${REPO}/.git" ]]; then
  install -d "$(dirname "${REPO}")"
  git clone https://github.com/cordtus/gh-runner-scaler.git "${REPO}"
fi
git -C "${REPO}" pull --ff-only origin main

# 2. Install the toolchain refresh script + systemd unit + weekly timer.
install -d "${LIBEXEC}"
install -m 0755 "${REPO}/deploy/refresh-runner-toolchains.sh" "${LIBEXEC}/refresh-runner-toolchains.sh"
install -m 0644 "${REPO}/deploy/systemd/${SERVICE}" "${UNIT_DIR}/${SERVICE}"
install -m 0644 "${REPO}/deploy/systemd/${TIMER}" "${UNIT_DIR}/${TIMER}"

systemctl daemon-reload
systemctl enable --now "${TIMER}"
echo "weekly timer enabled:"
systemctl list-timers "${TIMER}" --no-pager

# 3. Run the refresh once now (starts gh-runner-template, refreshes
#    Rust/cargo index, Node/npm, Yarn, pip, then stops it).
echo "running toolchain refresh now (unblocks stale-cargo --locked CI)..."
"${LIBEXEC}/refresh-runner-toolchains.sh"

echo "done. Template toolchains refreshed and weekly refresh scheduled."
