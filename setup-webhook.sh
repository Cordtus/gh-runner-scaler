#!/usr/bin/env bash
set -euo pipefail

# Create the GitHub org webhook for workflow_job events.
# Requires: GITHUB_TOKEN with admin:org_hook scope, and the webhook
# listener already running and accessible at WEBHOOK_URL.
#
# Usage: ./setup-webhook.sh <webhook_url> <webhook_secret>
# Example: ./setup-webhook.sh https://your-host:9876 your-secret-here

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/config"

WEBHOOK_URL="${1:?Usage: $0 <webhook_url> <webhook_secret>}"
WEBHOOK_SECRET="${2:?Usage: $0 <webhook_url> <webhook_secret>}"

echo "Creating org webhook for $ORG -> $WEBHOOK_URL"

curl -sf -X POST \
  -H "Authorization: token $GITHUB_TOKEN" \
  -H "Accept: application/vnd.github+json" \
  "https://api.github.com/orgs/$ORG/hooks" \
  -d "$(cat <<EOF
{
  "name": "web",
  "active": true,
  "events": ["workflow_job", "push"],
  "config": {
    "url": "$WEBHOOK_URL",
    "content_type": "json",
    "secret": "$WEBHOOK_SECRET",
    "insecure_ssl": "0"
  }
}
EOF
)" | jq '{id: .id, url: .config.url, events: .events, active: .active}'

echo "Webhook created. Update gh-runner-webhook.service with:"
echo "  Environment=GH_WEBHOOK_SECRET=$WEBHOOK_SECRET"
