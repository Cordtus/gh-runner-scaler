#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work_dir="$(mktemp -d)"
trap 'rm -rf "${work_dir}"' EXIT

fake_curl="${work_dir}/curl"
cat >"${fake_curl}" <<'EOF'
#!/usr/bin/env bash
prev=""
for arg in "$@"; do
  if [[ "${prev}" == "--data-binary" && "${arg}" == "@-" ]]; then
    cat >"${FAKE_CAPTURED_PAYLOAD:?}"
  fi
  prev="${arg}"
done
printf '%s' '{"status":"success","uid":"test-uid-1"}'
EOF
chmod 0755 "${fake_curl}"

captured="${work_dir}/captured"
: >"${captured}"

set +e
CURL_BIN="${fake_curl}" \
FAKE_CAPTURED_PAYLOAD="${captured}" \
  "${repo_root}/deploy/deploy-grafana-dashboard.sh" \
  --url https://grafana.example.com \
  --token grafana-token \
  --datasource-uid my-loki \
  >"${work_dir}/deploy.out" 2>&1
rc=$?
set -e

if [[ "${rc}" -ne 0 ]]; then
  echo "deploy-grafana-dashboard failed (rc=${rc}):" >&2
  cat "${work_dir}/deploy.out" >&2
  exit 1
fi

if ! grep -q "Dashboard imported (uid test-uid-1)" "${work_dir}/deploy.out"; then
  echo "expected import confirmation:" >&2
  cat "${work_dir}/deploy.out" >&2
  exit 1
fi

if ! command -v jq >/dev/null 2>&1; then
  echo "jq not available; skipping payload assertions"
  exit 0
fi

if [[ ! -s "${captured}" ]]; then
  echo "expected captured POST body" >&2
  exit 1
fi
if ! jq -e '.overwrite == true' "${captured}" >/dev/null; then
  echo "expected overwrite=true in import body" >&2
  jq . "${captured}" >&2
  exit 1
fi
if [[ "$(jq -r '.dashboard.title' "${captured}")" != "GitHub Runner Scaler" ]]; then
  echo "unexpected dashboard title in import body" >&2
  jq '.dashboard.title' "${captured}" >&2
  exit 1
fi
if [[ "$(jq -r '.dashboard.panels[0].datasource.uid' "${captured}")" != "my-loki" ]]; then
  echo "expected datasource uid rewrite to my-loki" >&2
  jq '.dashboard.panels[0].datasource' "${captured}" >&2
  exit 1
fi

echo "deploy-grafana-dashboard tests passed"
