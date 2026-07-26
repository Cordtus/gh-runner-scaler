#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work_dir="$(mktemp -d)"
trap 'rm -rf "${work_dir}"' EXIT

attempt_file="${work_dir}/attempts"
fake_curl="${work_dir}/curl"
cat >"${fake_curl}" <<'EOF'
#!/usr/bin/env bash
attempts=0
[[ ! -f "${ATTEMPT_FILE}" ]] || attempts="$(<"${ATTEMPT_FILE}")"
attempts=$((attempts + 1))
printf '%s\n' "${attempts}" >"${ATTEMPT_FILE}"
[[ "${attempts}" -ge 3 ]]
EOF
chmod 0755 "${fake_curl}"

ATTEMPT_FILE="${attempt_file}" \
CURL_BIN="${fake_curl}" \
"${repo_root}/deploy/wait-for-http-ready.sh" --timeout 2 --interval 0.01 http://127.0.0.1:9876/statusz

attempts="$(<"${attempt_file}")"
if [[ "${attempts}" -ne 3 ]]; then
  echo "expected HTTP readiness on attempt 3, got ${attempts}" >&2
  exit 1
fi
