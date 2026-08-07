#!/usr/bin/env bash

# deploy-grafana-dashboard.sh -- import deploy/grafana-dashboard.json into an
# existing Grafana stack (self-managed or Grafana Cloud).
#
# usage:
#   deploy/deploy-grafana-dashboard.sh \
#     --url https://your-grafana-host \
#     --token <service-account-token-or-api-key>
#
# options:
#   --url URL             Grafana base URL; defaults to $GRAFANA_API_URL
#   --token TOKEN         Grafana service account token or API key;
#                         defaults to $GRAFANA_SERVICE_ACCOUNT_TOKEN,
#                         then $GRAFANA_CLOUD_API_KEY
#   --datasource-uid UID  Loki datasource UID to wire the panels to;
#                         defaults to "loki" (the value the dashboard uses)
#   --dashboard PATH      dashboard JSON to import; defaults to
#                         deploy/grafana-dashboard.json in the repo checkout
#
# example:
#   deploy/deploy-grafana-dashboard.sh --url https://grafana.example.com \
#     --datasource-uid cd7f8b2e

set -euo pipefail

GRAFANA_URL="${GRAFANA_API_URL:-}"
GRAFANA_TOKEN="${GRAFANA_SERVICE_ACCOUNT_TOKEN:-${GRAFANA_CLOUD_API_KEY:-}}"
DATASOURCE_UID="loki"
DASHBOARD=""
CURL_BIN="${CURL_BIN:-curl}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

usage() {
  awk 'NR == 1 { next } /^[[:space:]]*$/ { next } /^#/ { line = $0; sub(/^# ?/, "", line); print line; next } { exit }' "${BASH_SOURCE[0]}"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --url)
      GRAFANA_URL="$2"
      shift 2
      ;;
    --token)
      GRAFANA_TOKEN="$2"
      shift 2
      ;;
    --datasource-uid)
      DATASOURCE_UID="$2"
      shift 2
      ;;
    --dashboard)
      DASHBOARD="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "error: unknown option: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if [[ -z "${DASHBOARD}" ]]; then
  DASHBOARD="${REPO_ROOT}/deploy/grafana-dashboard.json"
fi
if [[ ! -f "${DASHBOARD}" ]]; then
  echo "error: dashboard file not found: ${DASHBOARD}" >&2
  exit 1
fi
if [[ -z "${GRAFANA_URL}" ]]; then
  echo "error: no Grafana URL; pass --url or set GRAFANA_API_URL" >&2
  exit 2
fi
if [[ -z "${GRAFANA_TOKEN}" ]]; then
  echo "error: no Grafana token; pass --token, set GRAFANA_SERVICE_ACCOUNT_TOKEN, or set GRAFANA_CLOUD_API_KEY" >&2
  exit 2
fi

if ! command -v jq >/dev/null 2>&1; then
  echo "error: jq is required" >&2
  exit 1
fi

dashboard_json="$(jq --arg uid "${DATASOURCE_UID}" '
  walk(if type == "object" and has("uid") and .uid == "loki" then .uid = $uid else . end)
' "${DASHBOARD}")"
body="$(jq -cn --argjson dashboard "${dashboard_json}" '{dashboard: $dashboard, overwrite: true}')"

echo "Importing $(jq -r '.title // "dashboard"' <<<"${dashboard_json}") into ${GRAFANA_URL}"
response="$(
  printf '%s' "${body}" | "${CURL_BIN}" --fail --silent --show-error \
    --max-time 30 \
    -H "Authorization: Bearer ${GRAFANA_TOKEN}" \
    -H "Content-Type: application/json" \
    --data-binary @- \
    "${GRAFANA_URL%/}/api/dashboards/db"
)"
status="$(jq -er '.status' <<<"${response}")"
uid="$(jq -r '.uid // "unknown"' <<<"${response}")"
if [[ "${status}" != "success" ]]; then
  echo "error: Grafana did not accept the dashboard: ${response}" >&2
  exit 1
fi
echo "Dashboard imported (uid ${uid})."
echo "Panels use the Loki datasource UID '${DATASOURCE_UID}'; remap it in"
echo "Grafana if that datasource is not available in your stack."
