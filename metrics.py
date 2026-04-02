#!/usr/bin/env python3
"""Push GitHub Actions runner metrics to Grafana Cloud Loki.

Runs on a schedule (systemd timer) alongside the scaler. Queries the
GitHub API for runner state and pushes structured log entries to Loki
for dashboard visualization.
"""

import json
import os
import sys
import time
import requests

SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))
CONFIG_FILE = os.environ.get("GH_SCALER_CONFIG", os.path.join(SCRIPT_DIR, "config"))

# Load config (bash-style key=value)
config = {}
if os.path.exists(CONFIG_FILE):
    with open(CONFIG_FILE) as f:
        for line in f:
            line = line.strip()
            if line and not line.startswith("#") and "=" in line:
                key, _, val = line.partition("=")
                config[key.strip()] = val.strip().strip('"').strip("'")

GITHUB_TOKEN = config.get("GITHUB_TOKEN", "")
ORG = config.get("ORG", "Axionic-Labs")
PREFIX = config.get("PREFIX", "gh-runner-auto")

# Loki config
LOKI_PUSH_URL = os.environ.get("LOKI_PUSH_URL", "https://logs-prod-042.grafana.net/loki/api/v1/push")
LOKI_USERNAME = os.environ.get("LOKI_USERNAME", "1494650")
GRAFANA_CLOUD_API_KEY = os.environ.get("GRAFANA_CLOUD_API_KEY", "")

if not GITHUB_TOKEN:
    print("ERROR: GITHUB_TOKEN not found in config", file=sys.stderr)
    sys.exit(1)

if not GRAFANA_CLOUD_API_KEY:
    print("ERROR: GRAFANA_CLOUD_API_KEY env var not set", file=sys.stderr)
    sys.exit(1)


def get_runners():
    resp = requests.get(
        f"https://api.github.com/orgs/{ORG}/actions/runners?per_page=100",
        headers={
            "Authorization": f"Bearer {GITHUB_TOKEN}",
            "Accept": "application/vnd.github+json",
        },
        timeout=10,
    )
    resp.raise_for_status()
    return resp.json()


def push_to_loki(metrics: dict):
    now_ns = str(int(time.time() * 1e9))
    payload = {
        "streams": [
            {
                "stream": {
                    "job": "gh-runner-scaler",
                    "service": "runner-metrics",
                    "org": ORG,
                },
                "values": [
                    [now_ns, json.dumps(metrics)],
                ],
            }
        ]
    }
    resp = requests.post(
        LOKI_PUSH_URL,
        json=payload,
        auth=(LOKI_USERNAME, GRAFANA_CLOUD_API_KEY),
        headers={"Content-Type": "application/json"},
        timeout=10,
    )
    if resp.status_code not in (200, 204):
        print(f"Loki push failed ({resp.status_code}): {resp.text}", file=sys.stderr)


def main():
    data = get_runners()
    runners = data.get("runners", [])

    total = data.get("total_count", len(runners))
    busy = sum(1 for r in runners if r.get("busy"))
    idle = total - busy
    online = sum(1 for r in runners if r.get("status") == "online")
    offline = total - online
    auto = sum(1 for r in runners if r.get("name", "").startswith(PREFIX))
    permanent = total - auto

    metrics = {
        "total_runners": total,
        "busy_runners": busy,
        "idle_runners": idle,
        "online_runners": online,
        "offline_runners": offline,
        "auto_runners": auto,
        "permanent_runners": permanent,
        "runners": [
            {
                "name": r["name"],
                "status": r["status"],
                "busy": r["busy"],
                "is_auto": r["name"].startswith(PREFIX),
                "labels": [l["name"] for l in r.get("labels", [])],
            }
            for r in runners
        ],
    }

    push_to_loki(metrics)
    print(f"Pushed: total={total} busy={busy} idle={idle} auto={auto} permanent={permanent}")


if __name__ == "__main__":
    main()
