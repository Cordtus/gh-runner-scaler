#!/usr/bin/env bash
set -euo pipefail

REPOSITORY="${RUNNER_DISTRIBUTION_REPOSITORY:-actions/runner}"
PLATFORM="${RUNNER_DISTRIBUTION_PLATFORM:-linux-x64}"
CACHE_DIR="${RUNNER_DISTRIBUTION_CACHE_DIR:-/var/lib/gh-runner-scaler/runner-distributions}"
VERSION_FILE="${RUNNER_DISTRIBUTION_VERSION_FILE:-${CACHE_DIR}/current-version}"
RETAIN_VERSIONS="${RUNNER_DISTRIBUTION_RETAIN_VERSIONS:-2}"
TEMPLATE="${RUNNER_TEMPLATE:-gh-runner-template}"
INSTALL_ROOT="/home/runner/actions-runner"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

for command in curl jq sha256sum lxc; do
  command -v "${command}" >/dev/null || {
    echo "error: required command is missing: ${command}" >&2
    exit 1
  }
done

install -d -m 0755 "${CACHE_DIR}"
auth_args=()
if [[ -n "${GH_SCALER_GITHUB_TOKEN:-}" ]]; then
  auth_args=(-H "Authorization: Bearer ${GH_SCALER_GITHUB_TOKEN}")
fi

release_json="$(mktemp)"
download_tmp=""
template_started=0
cleanup() {
  rm -f "${release_json}"
  [[ -z "${download_tmp}" ]] || rm -f "${download_tmp}"
  if [[ "${template_started}" -eq 1 ]]; then
    lxc stop "${TEMPLATE}" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

curl --fail --silent --show-error --location \
  -H "Accept: application/vnd.github+json" \
  -H "X-GitHub-Api-Version: 2022-11-28" \
  "${auth_args[@]}" \
  "https://api.github.com/repos/${REPOSITORY}/releases/latest" >"${release_json}"

tag="$(jq -er '.tag_name' "${release_json}")"
version="${tag#v}"
asset_name="actions-runner-${PLATFORM}-${version}.tar.gz"
asset_url="$(jq -er --arg name "${asset_name}" '.assets[] | select(.name == $name) | .browser_download_url' "${release_json}")"
digest="$(jq -er --arg name "${asset_name}" '.assets[] | select(.name == $name) | .digest' "${release_json}")"
if [[ ! "${digest}" =~ ^sha256:[0-9a-fA-F]{64}$ ]]; then
  echo "error: GitHub did not publish a usable SHA-256 digest for ${asset_name}" >&2
  exit 1
fi
expected_sha="${digest#sha256:}"
archive="${CACHE_DIR}/${asset_name}"

if [[ -f "${VERSION_FILE}" && "$(<"${VERSION_FILE}")" == "${version}" && -f "${archive}" ]]; then
  actual_sha="$(sha256sum "${archive}" | awk '{print $1}')"
  if [[ "${actual_sha}" == "${expected_sha}" ]]; then
    echo "runner distribution ${version} is already verified and installed"
    exit 0
  fi
fi

download_tmp="$(mktemp "${CACHE_DIR}/.${asset_name}.XXXXXX")"
curl --fail --silent --show-error --location \
  -H "Accept: application/octet-stream" \
  "${auth_args[@]}" \
  "${asset_url}" >"${download_tmp}"
actual_sha="$(sha256sum "${download_tmp}" | awk '{print $1}')"
if [[ "${actual_sha}" != "${expected_sha}" ]]; then
  echo "error: checksum mismatch for ${asset_name}" >&2
  exit 1
fi
chmod 0644 "${download_tmp}"
mv -f "${download_tmp}" "${archive}"
download_tmp=""

status="$(lxc info "${TEMPLATE}" | awk '/^Status:/ {print $2}')"
if [[ "${status}" != "STOPPED" ]]; then
  echo "error: template ${TEMPLATE} must be stopped, found ${status:-unknown}" >&2
  exit 1
fi

lxc start "${TEMPLATE}"
template_started=1
"${SCRIPT_DIR}/wait-for-lxc-ready.sh" "${TEMPLATE}"
lxc file push "${archive}" "${TEMPLATE}/tmp/${asset_name}"
lxc exec "${TEMPLATE}" -- env \
  RUNNER_VERSION="${version}" \
  RUNNER_ARCHIVE="/tmp/${asset_name}" \
  INSTALL_ROOT="${INSTALL_ROOT}" \
  RETAIN_VERSIONS="${RETAIN_VERSIONS}" \
  bash -c '
    set -euo pipefail
    version_dir="${INSTALL_ROOT}/versions/${RUNNER_VERSION}"
    install -d -m 0755 "${version_dir}"
    tar -xzf "${RUNNER_ARCHIVE}" -C "${version_dir}"
    chown -R runner:runner "${INSTALL_ROOT}"
    link="${INSTALL_ROOT}/.current-${RUNNER_VERSION}"
    ln -s "${version_dir}" "${link}"
    mv -Tf "${link}" "${INSTALL_ROOT}/current"
    ln -sfn "${INSTALL_ROOT}/current/_diag" /home/runner/_diag
    ln -sfn "${INSTALL_ROOT}/current/_work" /home/runner/_work
    rm -f "${RUNNER_ARCHIVE}"
    mapfile -t versions < <(find "${INSTALL_ROOT}/versions" -mindepth 1 -maxdepth 1 -type d -printf "%f\n" | sort -Vr)
    for ((i=RETAIN_VERSIONS; i<${#versions[@]}; i++)); do
      rm -rf -- "${INSTALL_ROOT}/versions/${versions[$i]}"
    done
  '
lxc stop "${TEMPLATE}"
template_started=0

version_tmp="$(mktemp "${CACHE_DIR}/.current-version.XXXXXX")"
printf '%s\n' "${version}" >"${version_tmp}"
chmod 0644 "${version_tmp}"
mv -f "${version_tmp}" "${VERSION_FILE}"

mapfile -t archives < <(find "${CACHE_DIR}" -maxdepth 1 -type f -name "actions-runner-${PLATFORM}-*.tar.gz" -printf "%f\n" | sort -Vr)
for ((i=RETAIN_VERSIONS; i<${#archives[@]}; i++)); do
  rm -f -- "${CACHE_DIR}/${archives[$i]}"
done

echo "installed verified runner distribution ${version} into stopped template ${TEMPLATE}"
