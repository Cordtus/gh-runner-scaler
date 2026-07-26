#!/usr/bin/env bash
set -euo pipefail

timeout=30
interval=0.25
while [[ $# -gt 0 ]]; do
  case "$1" in
    --timeout)
      timeout="$2"
      shift 2
      ;;
    --interval)
      interval="$2"
      shift 2
      ;;
    *)
      break
      ;;
  esac
done

if [[ $# -ne 1 ]]; then
  echo "usage: $0 [--timeout seconds] [--interval seconds] URL" >&2
  exit 2
fi

url="$1"
curl_bin="${CURL_BIN:-curl}"
deadline=$((SECONDS + timeout))
while ! "${curl_bin}" --fail --silent --show-error --max-time 2 "${url}" >/dev/null 2>&1; do
  if (( SECONDS >= deadline )); then
    echo "error: ${url} did not become ready within ${timeout}s" >&2
    exit 1
  fi
  sleep "${interval}"
done
