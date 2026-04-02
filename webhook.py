#!/usr/bin/env python3
"""GitHub webhook listener for runner auto-scaler.

Central event coordinator for the self-hosted runner infrastructure.
Handles all org-level webhook events and dispatches to appropriate handlers:

- workflow_job.queued    -> trigger scaler (spin up runners)
- workflow_job.completed -> trigger scaler (cleanup)
- push to deps           -> sync shared dependencies on cache volume
"""

import hashlib
import hmac
import json
import os
import subprocess
import sys
import threading
import time
from http.server import HTTPServer, BaseHTTPRequestHandler

WEBHOOK_SECRET = os.environ.get("GH_WEBHOOK_SECRET", "")
SCALER_SCRIPT = os.environ.get("SCALER_SCRIPT", "/home/bv/gh-runner-scaler/gh-runner-scaler.sh")
SYNC_SCRIPT = os.environ.get("SYNC_SCRIPT", "/home/bv/gh-runner-scaler/sync-cache-deps.sh")
PORT = int(os.environ.get("WEBHOOK_PORT", "9876"))
DEBOUNCE_SECONDS = 2

# Repos whose pushes to main should trigger a cache volume sync.
# Map of repo full_name -> path on the cache volume.
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


def run_script(script: str, args: list[str] | None = None, label: str = "script"):
    """Run a shell script with optional args. Logs failures."""
    cmd = [script] + (args or [])
    try:
        subprocess.run(cmd, timeout=120, capture_output=True)
    except subprocess.TimeoutExpired:
        print(f"WARNING: {label} timed out after 120s", flush=True)
    except Exception as e:
        print(f"ERROR: {label} failed: {e}", flush=True)


def debounced(key: str, delay: float, fn, *args):
    """Schedule fn(*args) after delay seconds, collapsing rapid calls with the same key."""
    with _debounce_lock:
        existing = _debounce_timers.get(key)
        if existing is not None:
            existing.cancel()
        timer = threading.Timer(delay, fn, args=args)
        _debounce_timers[key] = timer
        timer.start()


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
            debounced("scaler", DEBOUNCE_SECONDS, run_script, SCALER_SCRIPT, None, "scaler")
        elif action == "completed":
            print(f"Job completed: {repo} / {name} -- scheduling cleanup", flush=True)
            debounced("scaler", DEBOUNCE_SECONDS, run_script, SCALER_SCRIPT, None, "scaler")

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
        debounced(
            f"sync-{repo_name}", DEBOUNCE_SECONDS,
            run_script, SYNC_SCRIPT, [repo, cache_path], f"sync-{repo_name}"
        )

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
