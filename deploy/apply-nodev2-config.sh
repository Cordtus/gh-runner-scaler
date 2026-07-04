#!/usr/bin/env bash

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"

GH_RUNNER_SCALER_CONFIG_SOURCE=deploy/nodev2.config.toml ./deploy/update-server.sh
