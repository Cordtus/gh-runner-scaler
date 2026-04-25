#!/usr/bin/env python3

import sys
import time
import urllib.request


def main() -> None:
    if len(sys.argv) < 2:
        raise SystemExit("usage: wait_for_http.py <url> [timeout-seconds]")

    url = sys.argv[1]
    timeout = float(sys.argv[2]) if len(sys.argv) > 2 else 20.0
    deadline = time.time() + timeout

    while time.time() < deadline:
        try:
            with urllib.request.urlopen(url, timeout=1.0):
                return
        except Exception:
            time.sleep(0.5)

    raise SystemExit(f"timed out waiting for {url}")


if __name__ == "__main__":
    main()
