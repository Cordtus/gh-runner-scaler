#!/usr/bin/env bash
set -euo pipefail

# Ensure snap binaries (lxc) are in PATH
export PATH="/snap/bin:$PATH"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONFIG_FILE="${GH_SCALER_CONFIG:-$SCRIPT_DIR/config}"
STATE_DIR="$SCRIPT_DIR/.state"
LOCK_FILE="$SCRIPT_DIR/.scaler.lock"

# Defaults (overridden by config file)
TEMPLATE="gh-runner-template"
ORG="Axionic-Labs"
PREFIX="gh-runner-auto"
MAX_AUTO_RUNNERS=6
IDLE_TIMEOUT=300
LABELS="self-hosted,linux,x64"
GITHUB_TOKEN=""
LXC_REMOTE=""

if [[ -f "$CONFIG_FILE" ]]; then
	source "$CONFIG_FILE"
fi

if [[ -z "$GITHUB_TOKEN" ]]; then
	echo "ERROR: GITHUB_TOKEN not set in $CONFIG_FILE" >&2
	exit 1
fi

mkdir -p "$STATE_DIR"

API="https://api.github.com"
AUTH_HEADER="Authorization: Bearer $GITHUB_TOKEN"

# LXC command prefix (empty for local, "remote:" for remote)
LXC_PREFIX="${LXC_REMOTE:+${LXC_REMOTE}:}"

log() { echo "$(date -Iseconds) [scaler] $*"; }

acquire_lock() {
	exec 9>"$LOCK_FILE"
	if ! flock -n 9; then
		log "Another instance is running, exiting"
		exit 0
	fi
}

api_get() {
	curl -sf -H "$AUTH_HEADER" -H "Accept: application/vnd.github+json" "$API$1"
}

api_post() {
	curl -sf -X POST -H "$AUTH_HEADER" -H "Accept: application/vnd.github+json" "$API$1"
}

get_runners() {
	api_get "/orgs/$ORG/actions/runners?per_page=100"
}

get_queued_runners() {
	# Count runners in queued state (online but not busy and not picking up jobs).
	# Falls back to 0 if the API call fails.
	local runners_json=$1
	echo "$runners_json" | jq '[.runners[] | select(.busy == false and .status == "online")] | length' 2>/dev/null || echo "0"
}

get_reg_token() {
	api_post "/orgs/$ORG/actions/runners/registration-token" | jq -r '.token'
}

get_remove_token() {
	api_post "/orgs/$ORG/actions/runners/remove-token" | jq -r '.token'
}

list_auto_containers() {
	lxc list "${LXC_PREFIX}" --format json 2>/dev/null \
		| jq -r ".[] | select(.name | startswith(\"$PREFIX\")) | .name" \
		| sort || true
}

next_name() {
	local i=1
	while lxc info "${LXC_PREFIX}${PREFIX}-${i}" &>/dev/null; do
		((i++))
	done
	echo "${PREFIX}-${i}"
}

scale_up() {
	local name token
	name=$(next_name)
	token=$(get_reg_token)

	if [[ -z "$token" || "$token" == "null" ]]; then
		log "ERROR: failed to get registration token"
		return 1
	fi

	log "Scaling up: creating $name from $TEMPLATE"

	lxc copy "${LXC_PREFIX}${TEMPLATE}" "${LXC_PREFIX}${name}"
	lxc start "${LXC_PREFIX}${name}"

	# Wait for networking + runner binary (90s for ZFS clone + boot)
	local ready=false
	for _ in $(seq 1 90); do
		if lxc exec "${LXC_PREFIX}${name}" -- test -f /home/runner/config.sh 2>/dev/null; then
			ready=true
			break
		fi
		sleep 1
	done

	if [[ "$ready" != "true" ]]; then
		log "ERROR: $name did not become ready in 90s, tearing down"
		lxc stop "${LXC_PREFIX}${name}" --force 2>/dev/null || true
		lxc delete "${LXC_PREFIX}${name}" --force 2>/dev/null || true
		return 1
	fi

	# Configure runner as ephemeral so it self-removes after one job.
	# This gives us clean containers for every job and avoids state leaks.
	if ! lxc exec "${LXC_PREFIX}${name}" -- su - runner -c \
		"./config.sh --url https://github.com/$ORG --token '$token' --name '$name' --labels '$LABELS' --work _work --unattended --ephemeral --replace"; then
		log "ERROR: config.sh failed for $name, tearing down"
		lxc stop "${LXC_PREFIX}${name}" --force 2>/dev/null || true
		lxc delete "${LXC_PREFIX}${name}" --force 2>/dev/null || true
		return 1
	fi

	# Install and start service
	lxc exec "${LXC_PREFIX}${name}" -- bash -c "cd /home/runner && ./svc.sh install runner && ./svc.sh start"

	# Track state
	date +%s > "$STATE_DIR/${name}.last_active"

	log "Scaled up: $name is online (ephemeral)"
}

scale_down() {
	local name=$1
	log "Scaling down: $name"

	# Stop the runner service
	lxc exec "${LXC_PREFIX}${name}" -- bash -c "cd /home/runner && ./svc.sh stop" 2>/dev/null || true

	# Deregister via config.sh
	local token
	token=$(get_remove_token) || true
	if [[ -n "$token" && "$token" != "null" ]]; then
		lxc exec "${LXC_PREFIX}${name}" -- su - runner -c "./config.sh remove --token '$token'" 2>/dev/null || true
	fi

	# Delete runner via API (catches cases where config.sh remove fails)
	local runner_id
	runner_id=$(api_get "/orgs/$ORG/actions/runners?per_page=100" \
		| jq --arg n "$name" '.runners[] | select(.name == $n) | .id' 2>/dev/null) || true
	if [[ -n "$runner_id" && "$runner_id" != "null" ]]; then
		curl -sf -X DELETE -H "$AUTH_HEADER" -H "Accept: application/vnd.github+json" \
			"$API/orgs/$ORG/actions/runners/$runner_id" 2>/dev/null || true
		log "Deleted runner $name (id: $runner_id) from GitHub"
	fi

	lxc stop "${LXC_PREFIX}${name}" --force 2>/dev/null || true
	lxc delete "${LXC_PREFIX}${name}" --force 2>/dev/null || true
	rm -f "$STATE_DIR/${name}."*

	log "Scaled down: $name removed"
}

main() {
	acquire_lock

	local runners_json
	runners_json=$(get_runners)

	if [[ -z "$runners_json" ]]; then
		log "ERROR: failed to query runners API"
		exit 1
	fi

	local total busy idle
	total=$(echo "$runners_json" | jq '.total_count')
	busy=$(echo "$runners_json" | jq '[.runners[] | select(.busy == true)] | length')
	idle=$((total - busy))

	local auto_containers auto_count
	auto_containers=$(list_auto_containers)
	if [[ -n "$auto_containers" ]]; then
		auto_count=$(echo "$auto_containers" | wc -l | tr -d ' ')
	else
		auto_count=0
	fi

	log "Runners: ${total} total, ${busy} busy, ${idle} idle | Auto: ${auto_count}/${MAX_AUTO_RUNNERS}"

	# --- Scale up ---
	# If all runners are busy, add one. The webhook triggers this immediately
	# on job queued events, so the 30s poll is just a safety net.
	if [[ $idle -eq 0 && $auto_count -lt $MAX_AUTO_RUNNERS ]]; then
		log "All runners busy, scaling up"
		scale_up || true
	fi

	# --- Scale down ---
	# Clean up idle auto-runners past timeout.
	# Ephemeral runners self-remove after their job, but the container stays.
	# Also clean up stopped/orphaned auto containers.
	if [[ -n "$auto_containers" ]]; then
		local now
		now=$(date +%s)

		while IFS= read -r container; do
			[[ -z "$container" ]] && continue

			# Check if the container is still running
			local container_status
			container_status=$(lxc list "${LXC_PREFIX}" --format json 2>/dev/null \
				| jq -r --arg n "$container" '.[] | select(.name == $n) | .status') || true

			if [[ "$container_status" == "Stopped" ]]; then
				# Ephemeral runner finished its job and stopped -- clean up immediately
				log "$container is stopped (ephemeral job complete)"
				scale_down "$container"
				continue
			fi

			# Is this runner busy right now?
			local is_busy
			is_busy=$(echo "$runners_json" | jq --arg n "$container" \
				'[.runners[] | select(.name == $n and .busy == true)] | length')

			if [[ "$is_busy" -gt 0 ]]; then
				date +%s > "$STATE_DIR/${container}.last_active"
			else
				local last_active idle_secs
				if [[ -f "$STATE_DIR/${container}.last_active" ]]; then
					last_active=$(cat "$STATE_DIR/${container}.last_active")
				else
					last_active=$now
					echo "$now" > "$STATE_DIR/${container}.last_active"
				fi
				idle_secs=$((now - last_active))

				if [[ $idle_secs -ge $IDLE_TIMEOUT ]]; then
					log "$container idle for ${idle_secs}s (>= ${IDLE_TIMEOUT}s)"
					scale_down "$container"
				fi
			fi
		done <<< "$auto_containers"
	fi
}

main "$@"
