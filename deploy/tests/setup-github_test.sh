#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work_dir="$(mktemp -d)"
trap 'rm -rf "${work_dir}"' EXIT

fake_curl="${work_dir}/curl"
cat >"${fake_curl}" <<'EOF'
#!/usr/bin/env bash
url=""
method="GET"
data=""
write_fmt=""
out=""
args=("$@")
i=0
while [[ ${i} -lt ${#args[@]} ]]; do
  a="${args[${i}]}"
  case "${a}" in
    -X|--request) method="${args[$((i+1))]}"; i=$((i+2)) ;;
    -d|--data|--data-binary) data="${args[$((i+1))]}"; i=$((i+2)) ;;
    -w|--write-out) write_fmt="${args[$((i+1))]}"; i=$((i+2)) ;;
    -o|--output) out="${args[$((i+1))]}"; i=$((i+2)) ;;
    -H|--header|--max-time) i=$((i+2)) ;;
    -s|--silent|--show-error|--location|-L|--fail) i=$((i+1)) ;;
    *) url="${a}"; i=$((i+1)) ;;
  esac
done

if [[ "${out}" == "/dev/null" && -n "${write_fmt}" ]]; then
  printf '%s' "${FAKE_RUNNERS_HTTP_CODE:-200}"
  exit 0
fi

case "${url}" in
  *"/actions/runners"*)
    printf '%s' '{"total_count":0,"runners":[]}'
    ;;
  *"/hooks"*)
    if [[ "${method}" == "GET" ]]; then
      printf '%s' "${FAKE_HOOKS_JSON:-{\"total_count\":0,\"hooks\":[]}}"
    else
      printf '%s\n%s' "${method}" "${data}" >"${FAKE_CAPTURED_PAYLOAD:?}"
      printf '%s' '{"id":42,"active":true}'
    fi
    ;;
  *)
    echo "error: unexpected URL in fake curl: ${url}" >&2
    exit 1
    ;;
esac
EOF
chmod 0755 "${fake_curl}"

captured="${work_dir}/captured"
: >"${captured}"

set +e
CURL_BIN="${fake_curl}" \
FAKE_CAPTURED_PAYLOAD="${captured}" \
  "${repo_root}/deploy/setup-github.sh" \
  --org ExampleOrg \
  --token test-token \
  --webhook-url https://gh-webhook.example.com/ \
  --webhook-secret topsecret \
  --push \
  --labels "self-hosted,linux,x64,nodev2" \
  >"${work_dir}/create.out" 2>&1
rc=$?
set -e

if [[ "${rc}" -ne 0 ]]; then
  echo "setup-github create path failed (rc=${rc}):" >&2
  cat "${work_dir}/create.out" >&2
  exit 1
fi

if ! grep -q "Webhook created" "${work_dir}/create.out"; then
  echo "expected 'Webhook created', output:" >&2
  cat "${work_dir}/create.out" >&2
  exit 1
fi
if ! grep -q "runs-on: \[self-hosted, linux, x64, nodev2, runner-class-<id>\]" "${work_dir}/create.out"; then
  echo "expected runs-on hint with supplied labels:" >&2
  cat "${work_dir}/create.out" >&2
  exit 1
fi

method="$(sed -n '1p' "${captured}")"
payload="$(sed -n '2,$p' "${captured}")"
if [[ "${method}" != "POST" ]]; then
  echo "expected POST for new webhook, got ${method}" >&2
  exit 1
fi
for needle in '"workflow_job"' '"push"' '"topsecret"' '"https://gh-webhook.example.com/"'; do
  if ! grep -q "${needle}" <<<"${payload}"; then
    echo "webhook payload missing ${needle}" >&2
    echo "${payload}" >&2
    exit 1
  fi
done

: >"${captured}"
set +e
CURL_BIN="${fake_curl}" \
FAKE_HOOKS_JSON='{"total_count":1,"hooks":[{"id":7,"config":{"url":"https://gh-webhook.example.com/"}}]}' \
FAKE_CAPTURED_PAYLOAD="${captured}" \
  "${repo_root}/deploy/setup-github.sh" \
  --org ExampleOrg \
  --token test-token \
  --webhook-url https://gh-webhook.example.com/ \
  --webhook-secret topsecret \
  >"${work_dir}/update.out" 2>&1
rc=$?
set -e

if [[ "${rc}" -ne 0 ]]; then
  echo "setup-github update path failed (rc=${rc}):" >&2
  cat "${work_dir}/update.out" >&2
  exit 1
fi
if ! grep -q "Webhook updated" "${work_dir}/update.out"; then
  echo "expected 'Webhook updated':" >&2
  cat "${work_dir}/update.out" >&2
  exit 1
fi
method="$(sed -n '1p' "${captured}")"
payload="$(sed -n '2,$p' "${captured}")"
if [[ "${method}" != "PATCH" ]]; then
  echo "expected PATCH for existing webhook, got ${method}" >&2
  exit 1
fi
if grep -q '"push"' <<<"${payload}"; then
  echo "update payload should not enable push events when --push is absent" >&2
  exit 1
fi

: >"${captured}"
set +e
CURL_BIN="${fake_curl}" \
FAKE_RUNNERS_HTTP_CODE=403 \
FAKE_CAPTURED_PAYLOAD="${captured}" \
  "${repo_root}/deploy/setup-github.sh" \
  --org ExampleOrg \
  --token test-token \
  >"${work_dir}/deny.out" 2>&1
rc=$?
set -e

if [[ "${rc}" -eq 0 ]]; then
  echo "expected failure for runner access denial" >&2
  cat "${work_dir}/deny.out" >&2
  exit 1
fi
if ! grep -q "cannot list self-hosted runners" "${work_dir}/deny.out"; then
  echo "expected permission hint on denial:" >&2
  cat "${work_dir}/deny.out" >&2
  exit 1
fi

echo "setup-github tests passed"
