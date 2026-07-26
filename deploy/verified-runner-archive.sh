#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: $0 ARCHIVE EXPECTED_SHA256" >&2
  exit 2
fi

archive="$1"
expected_sha="$2"
[[ -f "${archive}" ]] || exit 1
actual_sha="$(sha256sum "${archive}" | awk '{print $1}')"
[[ "${actual_sha}" == "${expected_sha}" ]]
