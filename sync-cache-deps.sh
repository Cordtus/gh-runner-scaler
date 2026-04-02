#!/usr/bin/env bash
set -euo pipefail

# Syncs a dependency repo on the shared cache volume.
# Called by the webhook on push events. Uses lxc exec on an already-running
# container rather than spinning up a new one.
#
# Usage: sync-cache-deps.sh <repo_full_name> <cache_path>
# Example: sync-cache-deps.sh Axionic-Labs/axionic-ui /cache/axionic-ui

export PATH="/snap/bin:$PATH"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONFIG_FILE="${GH_SCALER_CONFIG:-$SCRIPT_DIR/config}"

LXC_REMOTE=""
PREFIX="gh-runner-auto"

if [[ -f "$CONFIG_FILE" ]]; then
	source "$CONFIG_FILE"
fi

LXC_PREFIX="${LXC_REMOTE:+${LXC_REMOTE}:}"

REPO="${1:?Usage: $0 <repo_full_name> <cache_path>}"
CACHE_PATH="${2:?Usage: $0 <repo_full_name> <cache_path>}"

log() { echo "$(date -Iseconds) [sync] $*"; }

# Find any running container that has the cache volume mounted.
# Prefer the permanent runner, fall back to any auto-scaled one.
find_running_container() {
	local containers
	containers=$(lxc list "${LXC_PREFIX}" --format json 2>/dev/null \
		| jq -r '.[] | select(.status == "Running") | .name' | sort)

	# Prefer gh-runner (permanent), then any auto container
	for c in $containers; do
		if [[ "$c" == "gh-runner" ]]; then
			echo "$c"
			return
		fi
	done

	# Fall back to first running container
	echo "$containers" | head -1
}

CONTAINER=$(find_running_container)
if [[ -z "$CONTAINER" ]]; then
	log "ERROR: no running containers to exec into"
	exit 1
fi

BEFORE=$(lxc exec "${LXC_PREFIX}${CONTAINER}" -- \
	git -C "$CACHE_PATH" rev-parse HEAD 2>/dev/null || echo "unknown")

lxc exec "${LXC_PREFIX}${CONTAINER}" -- bash -c "
	git config --global --add safe.directory '$CACHE_PATH' 2>/dev/null
	git -C '$CACHE_PATH' fetch origin main 2>&1
	git -C '$CACHE_PATH' reset --hard origin/main 2>&1
" 2>&1

AFTER=$(lxc exec "${LXC_PREFIX}${CONTAINER}" -- \
	git -C "$CACHE_PATH" rev-parse HEAD 2>/dev/null || echo "unknown")

if [[ "$BEFORE" == "$AFTER" ]]; then
	log "${REPO}: already at ${AFTER:0:7} (via $CONTAINER)"
else
	log "${REPO}: updated ${BEFORE:0:7} -> ${AFTER:0:7} (via $CONTAINER)"
fi
