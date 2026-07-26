#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work_dir="$(mktemp -d)"
trap 'rm -rf "${work_dir}"' EXIT

attempt_file="${work_dir}/attempts"
fake_lxc="${work_dir}/lxc"
cat >"${fake_lxc}" <<'EOF'
#!/usr/bin/env bash
attempts=0
[[ ! -f "${ATTEMPT_FILE}" ]] || attempts="$(<"${ATTEMPT_FILE}")"
attempts=$((attempts + 1))
printf '%s\n' "${attempts}" >"${ATTEMPT_FILE}"
[[ "${attempts}" -ge 3 ]]
EOF
chmod 0755 "${fake_lxc}"

ATTEMPT_FILE="${attempt_file}" \
LXC_BIN="${fake_lxc}" \
"${repo_root}/deploy/wait-for-lxc-ready.sh" --timeout 2 --interval 0.01 gh-runner-template

attempts="$(<"${attempt_file}")"
if [[ "${attempts}" -ne 3 ]]; then
  echo "expected readiness probe to succeed on attempt 3, got ${attempts}" >&2
  exit 1
fi
