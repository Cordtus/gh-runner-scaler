#!/usr/bin/env bash
set -euo pipefail

# Syncs axionic-ui on the persistent cache volume to latest main.
# Runs inside a temporary container that has the cache volume attached.
# Designed to be called by a systemd timer.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONFIG_FILE="${GH_SCALER_CONFIG:-$SCRIPT_DIR/config}"

CACHE_POOL=""
CACHE_VOLUME=""
LXC_REMOTE=""

if [[ -f "$CONFIG_FILE" ]]; then
	source "$CONFIG_FILE"
fi

LXC_PREFIX="${LXC_REMOTE:+${LXC_REMOTE}:}"
SYNC_CONTAINER="axionic-ui-sync"

log() { echo "$(date -Iseconds) [ui-sync] $*"; }

if [[ -z "$CACHE_POOL" || -z "$CACHE_VOLUME" ]]; then
	log "ERROR: CACHE_POOL and CACHE_VOLUME must be set in config"
	exit 1
fi

cleanup() {
	lxc storage volume detach "${LXC_PREFIX}${CACHE_POOL}" "$CACHE_VOLUME" "$SYNC_CONTAINER" 2>/dev/null || true
	lxc delete "${LXC_PREFIX}${SYNC_CONTAINER}" --force 2>/dev/null || true
}
trap cleanup EXIT

# Use the runner template (has git) instead of pulling a fresh image
lxc copy "${LXC_PREFIX}${TEMPLATE:-gh-runner-template}" "${LXC_PREFIX}${SYNC_CONTAINER}"
lxc storage volume attach "${LXC_PREFIX}${CACHE_POOL}" "$CACHE_VOLUME" "${LXC_PREFIX}${SYNC_CONTAINER}" /cache
lxc start "${LXC_PREFIX}${SYNC_CONTAINER}"

# Wait for boot
for _ in $(seq 1 30); do
	if lxc exec "${LXC_PREFIX}${SYNC_CONTAINER}" -- test -d /cache/axionic-ui 2>/dev/null; then
		break
	fi
	sleep 1
done

BEFORE=$(lxc exec "${LXC_PREFIX}${SYNC_CONTAINER}" -- git -C /cache/axionic-ui rev-parse HEAD 2>/dev/null || echo "unknown")

lxc exec "${LXC_PREFIX}${SYNC_CONTAINER}" -- bash -c '
	git config --global --add safe.directory /cache/axionic-ui
	cd /cache/axionic-ui
	git fetch origin main 2>&1
	git reset --hard origin/main 2>&1
	chown -R 1001:1001 /cache/axionic-ui
'

AFTER=$(lxc exec "${LXC_PREFIX}${SYNC_CONTAINER}" -- git -C /cache/axionic-ui rev-parse HEAD 2>/dev/null || echo "unknown")

if [[ "$BEFORE" == "$AFTER" ]]; then
	log "axionic-ui already at ${AFTER:0:7}"
else
	log "axionic-ui updated: ${BEFORE:0:7} -> ${AFTER:0:7}"
fi
