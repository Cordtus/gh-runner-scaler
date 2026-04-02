#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

echo "=== GitHub Actions Runner Auto-Scaler Setup ==="

# Check config
if [[ ! -f "$SCRIPT_DIR/config" ]]; then
	echo "ERROR: Create $SCRIPT_DIR/config first (copy config.example)"
	echo "  cp $SCRIPT_DIR/config.example $SCRIPT_DIR/config"
	echo "  # Then set GITHUB_TOKEN (classic PAT with admin:org scope)"
	exit 1
fi

source "$SCRIPT_DIR/config"
if [[ -z "${GITHUB_TOKEN:-}" ]]; then
	echo "ERROR: GITHUB_TOKEN not set in config"
	exit 1
fi

# Check template container exists
if ! lxc info "$TEMPLATE" &>/dev/null; then
	echo "ERROR: Template container '$TEMPLATE' not found"
	echo "  Create it: lxc copy gh-runner-3 $TEMPLATE && lxc stop $TEMPLATE"
	echo "  Then clean runner config: lxc start $TEMPLATE && lxc exec $TEMPLATE -- rm -f /home/runner/.runner /home/runner/.credentials /home/runner/.credentials_rsaparams /home/runner/.runner_migrated && lxc stop $TEMPLATE"
	exit 1
fi

# Check template is stopped
local_status=$(lxc list --format json | jq -r ".[] | select(.name == \"$TEMPLATE\") | .status")
if [[ "$local_status" != "Stopped" ]]; then
	echo "ERROR: Template container '$TEMPLATE' must be stopped (currently: $local_status)"
	exit 1
fi

# Make scripts executable
chmod +x "$SCRIPT_DIR/gh-runner-scaler.sh"
chmod +x "$SCRIPT_DIR/sync-axionic-ui.sh"

# Create state directory
mkdir -p /var/lib/gh-runner-scaler

# Install systemd units
cp "$SCRIPT_DIR/gh-runner-scaler.service" /etc/systemd/system/
cp "$SCRIPT_DIR/gh-runner-scaler.timer" /etc/systemd/system/
cp "$SCRIPT_DIR/gh-runner-ui-sync.service" /etc/systemd/system/
cp "$SCRIPT_DIR/gh-runner-ui-sync.timer" /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now gh-runner-scaler.timer
systemctl enable --now gh-runner-ui-sync.timer

echo ""
echo "Installed and started. Check status:"
echo "  systemctl status gh-runner-scaler.timer"
echo "  journalctl -u gh-runner-scaler.service -f"
echo ""
echo "Manual test run:"
echo "  sudo $SCRIPT_DIR/gh-runner-scaler.sh"
