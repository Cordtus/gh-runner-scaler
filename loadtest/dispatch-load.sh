#!/usr/bin/env bash

set -euo pipefail

usage() {
  cat <<'EOF'
Usage: dispatch-load.sh <owner/name> <profile>

Profiles:
  cache-warm
  queue-burst
  steady-polyglot
  browser-spike
  mixed-peak
EOF
}

if [[ $# -ne 2 ]]; then
  usage >&2
  exit 1
fi

REPO="$1"
PROFILE="$2"

if ! command -v gh >/dev/null 2>&1; then
  echo "error: gh is required" >&2
  exit 1
fi

gh auth status >/dev/null

dispatch() {
  local workflow="$1"
  shift
  echo "Dispatching ${workflow} -> ${REPO}"
  gh workflow run "${workflow}" -R "${REPO}" "$@"
}

case "${PROFILE}" in
  cache-warm)
    dispatch cache-warm.yml -f matrix='[1]' -f max_parallel='1'
    ;;
  queue-burst)
    dispatch queue-burst.yml -f matrix='[1,2,3,4,5,6,7,8,9,10,11,12]' -f max_parallel='12' -f sleep_seconds='45'
    ;;
  steady-polyglot)
    dispatch node-cache-load.yml -f matrix='[1,2,3]' -f max_parallel='3'
    dispatch python-cache-load.yml -f matrix='[1,2,3]' -f max_parallel='3'
    dispatch go-cache-load.yml -f matrix='[1,2]' -f max_parallel='2'
    ;;
  browser-spike)
    dispatch browser-load.yml -f matrix='[1,2,3,4]' -f max_parallel='4'
    dispatch queue-burst.yml -f matrix='[1,2,3,4]' -f max_parallel='4' -f sleep_seconds='20'
    ;;
  mixed-peak)
    dispatch queue-burst.yml -f matrix='[1,2,3,4,5,6,7,8]' -f max_parallel='8' -f sleep_seconds='30'
    dispatch node-cache-load.yml -f matrix='[1,2,3,4]' -f max_parallel='4'
    dispatch python-cache-load.yml -f matrix='[1,2,3,4]' -f max_parallel='4'
    dispatch go-cache-load.yml -f matrix='[1,2,3]' -f max_parallel='3'
    dispatch browser-load.yml -f matrix='[1,2]' -f max_parallel='2'
    ;;
  *)
    echo "error: unknown profile: ${PROFILE}" >&2
    usage >&2
    exit 1
    ;;
esac

echo
echo "Recent runs:"
gh run list -R "${REPO}" --limit 20
