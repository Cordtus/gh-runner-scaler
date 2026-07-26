#!/usr/bin/env bash
set -euo pipefail

timeout=60
interval=0.5
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
  echo "usage: $0 [--timeout seconds] [--interval seconds] CONTAINER" >&2
  exit 2
fi

container="$1"
lxc_bin="${LXC_BIN:-lxc}"
deadline=$((SECONDS + timeout))
while ! "${lxc_bin}" exec "${container}" -- true >/dev/null 2>&1; do
  if (( SECONDS >= deadline )); then
    echo "error: container ${container} did not become exec-ready within ${timeout}s" >&2
    exit 1
  fi
  sleep "${interval}"
done
