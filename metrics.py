#!/usr/bin/env python3
"""Push GitHub Actions runner and workflow metrics to Grafana Cloud Loki.

Runs on a schedule (systemd timer) alongside the scaler. Queries the
GitHub API for runner state and recent workflow runs, then pushes
structured log entries to Loki for dashboard visualization.
"""

import json
import os
import subprocess
import sys
import time
import requests

SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))
CONFIG_FILE = os.environ.get("GH_SCALER_CONFIG", os.path.join(SCRIPT_DIR, "config"))

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
LXC_REMOTE = config.get("LXC_REMOTE", "")
CACHE_POOL = config.get("CACHE_POOL", "")

LOKI_PUSH_URL = os.environ.get("LOKI_PUSH_URL", "")
LOKI_USERNAME = os.environ.get("LOKI_USERNAME", "")
GRAFANA_CLOUD_API_KEY = os.environ.get("GRAFANA_CLOUD_API_KEY", "")

API = "https://api.github.com"
HEADERS = {
    "Authorization": f"Bearer {GITHUB_TOKEN}",
    "Accept": "application/vnd.github+json",
}

if not GITHUB_TOKEN:
    print("ERROR: GITHUB_TOKEN not found in config", file=sys.stderr)
    sys.exit(1)
if not LOKI_PUSH_URL or not LOKI_USERNAME:
    print("ERROR: LOKI_PUSH_URL and LOKI_USERNAME env vars must be set", file=sys.stderr)
    sys.exit(1)
if not GRAFANA_CLOUD_API_KEY:
    print("ERROR: GRAFANA_CLOUD_API_KEY env var not set", file=sys.stderr)
    sys.exit(1)


def api_get(path: str) -> dict | None:
    try:
        resp = requests.get(f"{API}{path}", headers=HEADERS, timeout=10)
        resp.raise_for_status()
        return resp.json()
    except Exception as e:
        print(f"API error {path}: {e}", file=sys.stderr)
        return None


def push_to_loki(stream_labels: dict, metrics: dict):
    now_ns = str(int(time.time() * 1e9))
    payload = {
        "streams": [{
            "stream": stream_labels,
            "values": [[now_ns, json.dumps(metrics)]],
        }]
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


def collect_runner_metrics() -> dict:
    """Runner pool state from GitHub API."""
    data = api_get(f"/orgs/{ORG}/actions/runners?per_page=100")
    if not data:
        return {}

    runners = data.get("runners", [])
    total = data.get("total_count", len(runners))
    busy = sum(1 for r in runners if r.get("busy"))
    online = sum(1 for r in runners if r.get("status") == "online")
    auto = sum(1 for r in runners if r.get("name", "").startswith(PREFIX))

    return {
        "total_runners": total,
        "busy_runners": busy,
        "idle_runners": total - busy,
        "online_runners": online,
        "offline_runners": total - online,
        "auto_runners": auto,
        "permanent_runners": total - auto,
        "utilization_pct": round(busy / total * 100, 1) if total > 0 else 0,
        "runners": [
            {
                "name": r["name"],
                "status": r["status"],
                "busy": r["busy"],
                "is_auto": r["name"].startswith(PREFIX),
            }
            for r in runners
        ],
    }


def collect_workflow_metrics() -> list[dict]:
    """Recent workflow run durations and outcomes per repo."""
    repos = api_get(f"/orgs/{ORG}/repos?per_page=100&type=all")
    if not repos:
        return []

    results = []
    for repo in repos:
        repo_name = repo["full_name"]
        runs_data = api_get(f"/repos/{repo_name}/actions/runs?per_page=5&status=completed")
        if not runs_data or not runs_data.get("workflow_runs"):
            continue

        for run in runs_data["workflow_runs"][:5]:
            created = run.get("created_at", "")
            updated = run.get("updated_at", "")
            duration_s = 0
            if created and updated:
                try:
                    from datetime import datetime, timezone
                    t0 = datetime.fromisoformat(created.replace("Z", "+00:00"))
                    t1 = datetime.fromisoformat(updated.replace("Z", "+00:00"))
                    duration_s = int((t1 - t0).total_seconds())
                except Exception:
                    pass

            results.append({
                "repo": repo_name.split("/")[-1],
                "workflow": run.get("name", "unknown"),
                "conclusion": run.get("conclusion", "unknown"),
                "duration_s": duration_s,
                "run_number": run.get("run_number", 0),
                "event": run.get("event", "unknown"),
                "branch": run.get("head_branch", "unknown"),
            })

    return results


def collect_host_metrics() -> dict:
    """LXC container count and storage pool usage."""
    lxc_prefix = f"{LXC_REMOTE}:" if LXC_REMOTE else ""
    metrics = {}

    try:
        result = subprocess.run(
            ["lxc", "list", lxc_prefix, "--format", "json"],
            capture_output=True, text=True, timeout=10,
        )
        if result.returncode == 0:
            containers = json.loads(result.stdout)
            running = sum(1 for c in containers if c.get("status") == "Running")
            stopped = sum(1 for c in containers if c.get("status") == "Stopped")
            metrics["containers_running"] = running
            metrics["containers_stopped"] = stopped
    except Exception as e:
        print(f"LXC metrics error: {e}", file=sys.stderr)

    if CACHE_POOL:
        try:
            result = subprocess.run(
                ["lxc", "storage", "info", f"{lxc_prefix}{CACHE_POOL}", "--format", "json"],
                capture_output=True, text=True, timeout=10,
            )
            if result.returncode == 0:
                info = json.loads(result.stdout)
                resources = info.get("resources", {})
                total = resources.get("space", {}).get("total", 0)
                used = resources.get("space", {}).get("used", 0)
                if total > 0:
                    metrics["cache_pool_used_gb"] = round(used / (1024**3), 1)
                    metrics["cache_pool_total_gb"] = round(total / (1024**3), 1)
                    metrics["cache_pool_pct"] = round(used / total * 100, 1)
        except Exception as e:
            print(f"Storage metrics error: {e}", file=sys.stderr)

    return metrics


def main():
    runner_metrics = collect_runner_metrics()
    if runner_metrics:
        push_to_loki(
            {"job": "gh-runner-scaler", "service": "runner-metrics", "org": ORG},
            runner_metrics,
        )
        total = runner_metrics.get("total_runners", 0)
        busy = runner_metrics.get("busy_runners", 0)
        auto = runner_metrics.get("auto_runners", 0)
        print(f"Runners: total={total} busy={busy} auto={auto} util={runner_metrics.get('utilization_pct', 0)}%")

    host_metrics = collect_host_metrics()
    if host_metrics:
        push_to_loki(
            {"job": "gh-runner-scaler", "service": "host-metrics", "org": ORG},
            host_metrics,
        )
        print(f"Host: containers={host_metrics.get('containers_running', '?')} cache={host_metrics.get('cache_pool_pct', '?')}%")

    workflow_metrics = collect_workflow_metrics()
    if workflow_metrics:
        push_to_loki(
            {"job": "gh-runner-scaler", "service": "workflow-metrics", "org": ORG},
            {"runs": workflow_metrics},
        )
        print(f"Workflows: {len(workflow_metrics)} recent runs collected")


if __name__ == "__main__":
    main()
