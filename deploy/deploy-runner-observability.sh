#!/usr/bin/env bash
# Run on nodev2 from the gh-runner-scaler checkout:
#   sudo ./deploy/deploy-runner-observability.sh
set -euo pipefail

if [[ ${EUID} -ne 0 ]]; then
  exec sudo -- "$0" "$@"
fi

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
env_path=/etc/gh-runner-scaler/env
push_url="${RUNNER_LOG_LOKI_PUSH_URL:-http://192.168.0.157:3100/loki/api/v1/push}"
health_url="${RUNNER_LOG_LOKI_HEALTH_URL:-http://192.168.0.157:3100/ready}"

if [[ ! -f ${env_path} ]]; then
  echo "error: missing ${env_path}; refusing to create a secrets file" >&2
  exit 1
fi

echo "Checking internal Loki readiness: ${health_url}"
curl --fail --silent --show-error --max-time 5 "${health_url}" >/dev/null

tmp_env="$(mktemp)"
trap 'rm -f "${tmp_env}"' EXIT
awk -v push="${push_url}" -v health="${health_url}" '
  /^RUNNER_LOG_LOKI_PUSH_URL=/ { next }
  /^RUNNER_LOG_LOKI_HEALTH_URL=/ { next }
  /^RUNNER_LOG_LOKI_(USERNAME|PASSWORD|API_KEY)=/ { next }
  { print }
  END {
    print "RUNNER_LOG_LOKI_PUSH_URL=" push
    print "RUNNER_LOG_LOKI_HEALTH_URL=" health
  }
' "${env_path}" >"${tmp_env}"
install -m 0600 "${tmp_env}" "${env_path}"

cd "${repo_root}"
GH_RUNNER_SCALER_CONFIG_SOURCE=deploy/nodev2.config.toml ./deploy/update-server.sh

systemctl is-active --quiet gh-runner-scaler.service
./deploy/wait-for-http-ready.sh --timeout 30 http://127.0.0.1:9876/statusz
curl --fail --silent --show-error --max-time 5 http://127.0.0.1:9876/statusz
printf '\nDeployment complete. Trigger one disposable Actions job and inspect runner logs and issue-events in Grafana.\n'
