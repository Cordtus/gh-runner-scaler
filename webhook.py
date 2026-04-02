#!/usr/bin/env python3
"""GitHub webhook listener for runner auto-scaler.

Central event coordinator for the self-hosted runner infrastructure.
Handles org-level webhook events:

- workflow_job.queued    -> trigger scaler (spin up runners)
- workflow_job.completed -> trigger scaler (cleanup)
- push to tracked repos  -> update cache volume via lxc exec
"""

import hashlib
import hmac
import json
import os
import subprocess
import sys
import threading
from http.server import HTTPServer, BaseHTTPRequestHandler

WEBHOOK_SECRET = os.environ.get("GH_WEBHOOK_SECRET", "")
SCALER_SCRIPT = os.environ.get("SCALER_SCRIPT", "/home/bv/gh-runner-scaler/gh-runner-scaler.sh")
PORT = int(os.environ.get("WEBHOOK_PORT", "9876"))
LXC_REMOTE = os.environ.get("LXC_REMOTE", "")
DEBOUNCE_SECONDS = 2

# Repos whose pushes to default branch trigger a cache volume sync.
# repo full_name -> cache path inside the container
SYNCED_REPOS = {
    "Axionic-Labs/axionic-ui": "/cache/axionic-ui",
}

_debounce_timers: dict[str, threading.Timer] = {}
_debounce_lock = threading.Lock()


def verify_signature(payload: bytes, signature: str) -> bool:
    if not WEBHOOK_SECRET:
        return False
    expected = "sha256=" + hmac.new(
        WEBHOOK_SECRET.encode(), payload, hashlib.sha256
    ).hexdigest()
    return hmac.compare_digest(expected, signature)


def debounced(key: str, delay: float, fn, *args):
    """Schedule fn(*args) after delay, collapsing rapid calls with the same key."""
    with _debounce_lock:
        existing = _debounce_timers.get(key)
        if existing is not None:
            existing.cancel()
        timer = threading.Timer(delay, fn, args=args)
        _debounce_timers[key] = timer
        timer.start()


def run_scaler(label: str = "scaler"):
    """Run the scaler script."""
    try:
        subprocess.run([SCALER_SCRIPT], timeout=120, capture_output=True)
    except subprocess.TimeoutExpired:
        print(f"WARNING: {label} timed out after 120s", flush=True)
    except Exception as e:
        print(f"ERROR: {label} failed: {e}", flush=True)


def find_running_container() -> str | None:
    """Find a running container to exec into for cache updates."""
    lxc_prefix = f"{LXC_REMOTE}:" if LXC_REMOTE else ""
    try:
        result = subprocess.run(
            ["/snap/bin/lxc", "list", lxc_prefix, "--format", "json"],
            capture_output=True, text=True, timeout=10,
        )
        if result.returncode != 0:
            return None
        containers = json.loads(result.stdout)
        running = [c["name"] for c in containers if c.get("status") == "Running"]
        # Prefer the permanent runner
        for name in running:
            if name == "gh-runner":
                return name
        return running[0] if running else None
    except Exception:
        return None


def sync_cache_repo(repo: str, cache_path: str):
    """Update a repo on the cache volume via lxc exec."""
    container = find_running_container()
    if not container:
        print(f"WARNING: no running container for cache sync of {repo}", flush=True)
        return

    lxc_prefix = f"{LXC_REMOTE}:" if LXC_REMOTE else ""
    target = f"{lxc_prefix}{container}"
    try:
        result = subprocess.run(
            ["/snap/bin/lxc", "exec", target, "--", "bash", "-c",
             f"git config --global --add safe.directory '{cache_path}' 2>/dev/null; "
             f"git -C '{cache_path}' fetch origin main 2>&1; "
             f"git -C '{cache_path}' reset --hard origin/main 2>&1; "
             f"git -C '{cache_path}' log --oneline -1"],
            capture_output=True, text=True, timeout=30,
        )
        print(f"Cache sync {repo} via {container}: {result.stdout.strip()}", flush=True)
    except Exception as e:
        print(f"ERROR: cache sync {repo} failed: {e}", flush=True)


class WebhookHandler(BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        payload = self.rfile.read(length)
        signature = self.headers.get("X-Hub-Signature-256", "")

        if not verify_signature(payload, signature):
            self.send_response(401)
            self.end_headers()
            self.wfile.write(b"invalid signature")
            return

        event = self.headers.get("X-GitHub-Event", "")
        data = json.loads(payload)

        if event == "workflow_job":
            self._handle_workflow_job(data)
        elif event == "push":
            self._handle_push(data)

        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"ok")

    def _handle_workflow_job(self, data: dict):
        action = data.get("action", "")
        job = data.get("workflow_job", {})
        name = job.get("name", "unknown")
        repo = data.get("repository", {}).get("full_name", "unknown")

        if action == "queued":
            print(f"Job queued: {repo} / {name} -- scheduling scaler", flush=True)
            debounced("scaler", DEBOUNCE_SECONDS, run_scaler, "scaler")
        elif action == "completed":
            print(f"Job completed: {repo} / {name} -- scheduling cleanup", flush=True)
            debounced("scaler", DEBOUNCE_SECONDS, run_scaler, "scaler")

    def _handle_push(self, data: dict):
        repo = data.get("repository", {}).get("full_name", "")
        ref = data.get("ref", "")
        after = data.get("after", "")[:7]

        if repo not in SYNCED_REPOS:
            return
        if ref != "refs/heads/main":
            return

        cache_path = SYNCED_REPOS[repo]
        repo_name = repo.split("/")[-1]
        print(f"Push to {repo} main ({after}) -- syncing {cache_path}", flush=True)
        debounced(f"sync-{repo_name}", DEBOUNCE_SECONDS, sync_cache_repo, repo, cache_path)

    def do_GET(self):
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"gh-runner-scaler webhook listener")

    def log_message(self, fmt, *args):
        print(f"{self.log_date_time_string()} {fmt % args}", flush=True)


if __name__ == "__main__":
    if not WEBHOOK_SECRET:
        print("ERROR: GH_WEBHOOK_SECRET env var not set", file=sys.stderr)
        sys.exit(1)
    server = HTTPServer(("0.0.0.0", PORT), WebhookHandler)
    print(f"Listening on :{PORT}", flush=True)
    print(f"Synced repos: {list(SYNCED_REPOS.keys())}", flush=True)
    server.serve_forever()
