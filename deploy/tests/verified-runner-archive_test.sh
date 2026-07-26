#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work_dir="$(mktemp -d)"
trap 'rm -rf "${work_dir}"' EXIT

archive="${work_dir}/runner.tar.gz"
printf 'verified archive\n' >"${archive}"
digest="$(sha256sum "${archive}" | awk '{print $1}')"

"${repo_root}/deploy/verified-runner-archive.sh" "${archive}" "${digest}"

printf 'changed\n' >>"${archive}"
if "${repo_root}/deploy/verified-runner-archive.sh" "${archive}" "${digest}"; then
  echo "changed archive was incorrectly accepted" >&2
  exit 1
fi
