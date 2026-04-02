#!/usr/bin/env python3
"""GitHub webhook listener for runner auto-scaler.

Listens for workflow_job events and triggers the scaler immediately
when jobs are queued. Debounces rapid bursts so concurrent queued
events within 2s collapse into a single scaler invocation.
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
PORT = int(os.environ.get("WEBHOOK_PORT", "9876"))
DEBOUNCE_SECONDS = 2

_debounce_timer = None
_debounce_lock = threading.Lock()


def verify_signature(payload: bytes, signature: str) -> bool:
    if not WEBHOOK_SECRET:
        return False
    expected = "sha256=" + hmac.new(
        WEBHOOK_SECRET.encode(), payload, hashlib.sha256
    ).hexdigest()
    return hmac.compare_digest(expected, signature)


def trigger_scaler():
    """Run the scaler script. Called after debounce window closes."""
    try:
        subprocess.run(
            [SCALER_SCRIPT],
            timeout=120,
            capture_output=True,
        )
    except subprocess.TimeoutExpired:
        print("WARNING: scaler timed out after 120s", flush=True)
    except Exception as e:
        print(f"ERROR: scaler failed: {e}", flush=True)


def debounced_trigger():
    """Schedule a scaler run, debouncing rapid bursts."""
    global _debounce_timer
    with _debounce_lock:
        if _debounce_timer is not None:
            _debounce_timer.cancel()
        _debounce_timer = threading.Timer(DEBOUNCE_SECONDS, trigger_scaler)
        _debounce_timer.start()


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

        if event == "workflow_job":
            data = json.loads(payload)
            action = data.get("action", "")
            job = data.get("workflow_job", {})
            name = job.get("name", "unknown")
            repo = data.get("repository", {}).get("full_name", "unknown")

            if action == "queued":
                print(f"Job queued: {repo} / {name} -- scheduling scaler", flush=True)
                debounced_trigger()
            elif action == "completed":
                # Trigger cleanup check after job completion
                print(f"Job completed: {repo} / {name} -- scheduling cleanup", flush=True)
                debounced_trigger()

        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"ok")

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
    server.serve_forever()
