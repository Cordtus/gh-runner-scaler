#!/usr/bin/env bash

# setup-github.sh -- configure the GitHub side of a gh-runner-scaler target.
#
# Validates that the token can manage self-hosted runners for the target,
# creates or updates the workflow webhook GitHub will POST to, and prints the
# exact runs-on labels to add to workflows. Safe to re-run: webhooks are
# matched by payload URL and updated in place.
#
# usage:
#   deploy/setup-github.sh --org ORG
#   deploy/setup-github.sh --repo owner/name
#
# options:
#   --webhook-url URL        public HTTPS URL GitHub can reach (enables webhook
#                            create/update; omit it to only validate the token)
#   --webhook-secret SECRET  HMAC secret; defaults to $GH_WEBHOOK_SECRET
#   --push                   also subscribe to push events (required for
#                            [webhook.sync_repos] default-branch cache syncs)
#   --token TOKEN            token with self-hosted-runner management access;
#                            defaults to $GH_SCALER_GITHUB_TOKEN, then $GH_TOKEN
#   --labels "a,b,c"         comma-separated labels to print as the workflow
#                            hint; defaults to "self-hosted,linux,x64"
#
# example:
#   GH_WEBHOOK_SECRET="$(openssl rand -hex 32)" \
#     deploy/setup-github.sh --org ExampleOrg \
#       --webhook-url https://gh-webhook.example.com/

set -euo pipefail

ORG=""
REPO=""
WEBHOOK_URL=""
WEBHOOK_SECRET="${GH_WEBHOOK_SECRET:-}"
PUSH_EVENTS=0
TOKEN="${GH_SCALER_GITHUB_TOKEN:-${GH_TOKEN:-}}"
LABELS="self-hosted,linux,x64"
API_BASE="${GH_API_BASE:-https://api.github.com}"
CURL_BIN="${CURL_BIN:-curl}"

usage() {
  awk 'NR == 1 { next } /^[[:space:]]*$/ { next } /^#/ { line = $0; sub(/^# ?/, "", line); print line; next } { exit }' "${BASH_SOURCE[0]}"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --org)
      ORG="$2"
      shift 2
      ;;
    --repo)
      REPO="$2"
      shift 2
      ;;
    --webhook-url)
      WEBHOOK_URL="$2"
      shift 2
      ;;
    --webhook-secret)
      WEBHOOK_SECRET="$2"
      shift 2
      ;;
    --push)
      PUSH_EVENTS=1
      shift
      ;;
    --token)
      TOKEN="$2"
      shift 2
      ;;
    --labels)
      LABELS="$2"
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

if [[ -n "${ORG}" && -n "${REPO}" ]]; then
  echo "error: choose exactly one of --org or --repo" >&2
  exit 2
fi
if [[ -z "${ORG}" && -z "${REPO}" ]]; then
  echo "error: one of --org or --repo is required" >&2
  usage >&2
  exit 2
fi
if [[ -z "${TOKEN}" ]]; then
  echo "error: no token; pass --token or set GH_SCALER_GITHUB_TOKEN" >&2
  exit 2
fi

api_curl() {
  "${CURL_BIN}" --fail --silent --show-error --location \
    -H "Authorization: Bearer ${TOKEN}" \
    -H "Accept: application/vnd.github+json" \
    -H "X-GitHub-Api-Version: 2022-11-28" \
    "$@"
}

if [[ -n "${ORG}" ]]; then
  TARGET_KIND="org"
  RUNNER_URL="${API_BASE}/orgs/${ORG}/actions/runners"
  HOOK_BASE="${API_BASE}/orgs/${ORG}/hooks"
else
  TARGET_KIND="repo"
  RUNNER_URL="${API_BASE}/repos/${REPO}/actions/runners"
  HOOK_BASE="${API_BASE}/repos/${REPO}/hooks"
fi

echo "Validating token against ${RUNNER_URL}"
status="$(
  "${CURL_BIN}" --silent --show-error --location --output /dev/null --write-out '%{http_code}' \
    -H "Authorization: Bearer ${TOKEN}" \
    -H "Accept: application/vnd.github+json" \
    -H "X-GitHub-Api-Version: 2022-11-28" \
    "${RUNNER_URL}"
)"
if [[ "${status}" != "2"* ]]; then
  echo "error: token cannot list self-hosted runners for the target (HTTP ${status})" >&2
  echo "the token needs:" >&2
  echo "  - org target:  Organization > Self-hosted runners, Read and write" >&2
  echo "  - repo target: Repository > Administration, Write" >&2
  echo "a fine-grained PAT must also have access to the target org/repo" >&2
  exit 1
fi
echo "Token can manage self-hosted runners."

if [[ -z "${WEBHOOK_URL}" ]]; then
  echo "Skipping webhook setup (no --webhook-url)."
  echo "Create it manually with this payload URL and secret:"
  echo "  URL:    <your public https URL ending in />"
  echo "  Secret: ${WEBHOOK_SECRET:-<the same value as GH_WEBHOOK_SECRET on the scaler host>}"
  echo "  Events: workflow_job$([ "${PUSH_EVENTS}" -eq 1 ] && echo ", push")"
else
  if [[ -z "${WEBHOOK_SECRET}" ]]; then
    echo "error: no webhook secret; pass --webhook-secret or set GH_WEBHOOK_SECRET" >&2
    exit 2
  fi
  if [[ "${PUSH_EVENTS}" -eq 1 ]]; then
    EVENTS='["workflow_job","push"]'
  else
    EVENTS='["workflow_job"]'
  fi

  echo "Reading existing webhooks from ${HOOK_BASE}"
  hooks_json="$(api_curl "${HOOK_BASE}?per_page=100")"
  existing_id="$(jq -er --arg url "${WEBHOOK_URL}" '.hooks[] | select(.config.url == $url) | .id' <<<"${hooks_json}" | head -n1 || true)"

  payload="$(jq -cn \
    --arg url "${WEBHOOK_URL}" \
    --arg secret "${WEBHOOK_SECRET}" \
    --argjson events "${EVENTS}" \
    '{name:"web",active:true,events:$events,config:{url:$url,content_type:"json",secret:$secret,insecure_ssl:"0"}}')"

  if [[ -n "${existing_id}" ]]; then
    echo "Updating existing webhook ${HOOK_BASE}/${existing_id}"
    api_curl -X PATCH "${HOOK_BASE}/${existing_id}" -d "${payload}" >/dev/null
    echo "Webhook updated."
  else
    echo "Creating webhook"
    api_curl -X POST "${HOOK_BASE}" -d "${payload}" >/dev/null
    echo "Webhook created."
  fi
fi

echo
echo "GitHub side is configured for ${TARGET_KIND} ${ORG}${REPO}."
echo
echo "Set these secrets on the scaler host (e.g. /etc/gh-runner-scaler/env):"
echo "  GH_WEBHOOK_SECRET=${WEBHOOK_SECRET:-<set to the webhook secret>}"
echo
echo "Route jobs onto this target by adding a class label to your workflow:"
echo "  runs-on: [${LABELS//,/, }, runner-class-<id>]"
echo "The <id> must match the [[runner_classes]] id in your config.toml, and"
echo "the class labels must include every label listed above."
