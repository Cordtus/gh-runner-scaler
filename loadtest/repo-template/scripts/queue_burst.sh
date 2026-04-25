#!/usr/bin/env bash

set -euo pipefail

SHARD="${1:-0}"
SLEEP_SECONDS="${2:-20}"
OUT_DIR="tmp/queue-${SHARD}"

rm -rf "${OUT_DIR}"
mkdir -p "${OUT_DIR}"

for index in $(seq 1 16); do
  python3 - "${SHARD}" "${index}" "${OUT_DIR}/blob-${index}.txt" <<'PY'
import pathlib
import sys

shard, index, destination = sys.argv[1:]
payload = (f"runner-load-{shard}-{index}\n" * 12000).encode("utf-8")
pathlib.Path(destination).write_bytes(payload[:131072])
PY
done

sha256sum "${OUT_DIR}"/*.txt > "${OUT_DIR}/checksums.txt"
sleep "${SLEEP_SECONDS}"

echo "queue burst shard ${SHARD} finished"
