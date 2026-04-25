#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEMPLATE_DIR="${SCRIPT_DIR}/repo-template"

usage() {
  cat <<'EOF'
Usage: create-test-repo.sh <owner/name> [output-dir] [--push] [--public]

Seeds a standalone runner load-test repo from the local template. With
--push, the script also creates the GitHub repo via `gh repo create`
and pushes the initial commit.
EOF
}

if [[ $# -lt 1 ]]; then
  usage >&2
  exit 1
fi

REPO_FULL_NAME="$1"
shift

OUTPUT_DIR=""
PUSH=0
VISIBILITY="--private"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --push)
      PUSH=1
      ;;
    --public)
      VISIBILITY="--public"
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      if [[ -n "${OUTPUT_DIR}" ]]; then
        echo "error: unexpected argument: $1" >&2
        exit 1
      fi
      OUTPUT_DIR="$1"
      ;;
  esac
  shift
done

REPO_NAME="${REPO_FULL_NAME##*/}"
if [[ -z "${OUTPUT_DIR}" ]]; then
  OUTPUT_DIR="/tmp/${REPO_NAME}"
fi

if [[ ! -d "${TEMPLATE_DIR}" ]]; then
  echo "error: missing template directory: ${TEMPLATE_DIR}" >&2
  exit 1
fi

if [[ -e "${OUTPUT_DIR}" ]]; then
  echo "error: output path already exists: ${OUTPUT_DIR}" >&2
  exit 1
fi

mkdir -p "$(dirname "${OUTPUT_DIR}")"
cp -a "${TEMPLATE_DIR}/." "${OUTPUT_DIR}"

git -C "${OUTPUT_DIR}" init -b main
git -C "${OUTPUT_DIR}" add .
git -C "${OUTPUT_DIR}" -c commit.gpgsign=false commit -m "Seed runner load lab"

if [[ "${PUSH}" -eq 1 ]]; then
  if ! command -v gh >/dev/null 2>&1; then
    echo "error: gh is required for --push" >&2
    exit 1
  fi
  gh auth status >/dev/null
  gh repo create "${REPO_FULL_NAME}" "${VISIBILITY}" --source "${OUTPUT_DIR}" --remote origin --push
fi

echo "Seeded test repo at ${OUTPUT_DIR}"
