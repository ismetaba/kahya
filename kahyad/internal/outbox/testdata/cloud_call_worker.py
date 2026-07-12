#!/usr/bin/env python3
"""cloud_call_worker.py - dispatcher_test.go fixture (W4-04): reads the
envelope off stdin (ignored beyond consuming it), then makes ONE real
HTTP POST to $ANTHROPIC_BASE_URL/v1/messages using $ANTHROPIC_API_KEY as
the x-api-key header - exactly the real worker's ANTHROPIC_BASE_URL
contract (kahyad's own per-task anthproxy.Proxy listener) - and reports
the outcome via the ordinary stdout protocol lines: HTTP 200 -> "result"
ok/exit 0; anything else -> "error"/exit 1. This fixture does NOT
implement the real worker's cloud_unreachable detection heuristic (that
is worker/kahya_worker's own job, covered by worker/tests) - it only
proves a genuinely fresh HTTP round-trip through a freshly-opened
anthproxy.Proxy succeeds once the fake upstream has healed.
"""
import json
import os
import sys
import urllib.error
import urllib.request


def emit(obj):
    sys.stdout.write(json.dumps(obj, ensure_ascii=False) + "\n")
    sys.stdout.flush()


def main():
    sys.stdin.buffer.read()  # consume the envelope; contents unused
    emit({"type": "session", "session_id": "cloud-call-session"})

    base_url = os.environ.get("ANTHROPIC_BASE_URL", "")
    api_key = os.environ.get("ANTHROPIC_API_KEY", "")
    body = json.dumps({"model": "claude-sonnet-5", "messages": []}).encode("utf-8")
    req = urllib.request.Request(
        base_url + "/v1/messages",
        data=body,
        method="POST",
        headers={"x-api-key": api_key, "Content-Type": "application/json"},
    )
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            status = resp.getcode()
    except urllib.error.HTTPError as e:
        status = e.code
    except Exception as e:  # noqa: BLE001 - any transport failure is a cloud-call failure
        emit({"type": "error", "message": "cloud call failed: %s" % e})
        sys.exit(1)

    if status == 200:
        emit({"type": "result", "status": "ok"})
        sys.exit(0)

    emit({"type": "error", "message": "cloud call failed with status %d" % status})
    sys.exit(1)


if __name__ == "__main__":
    main()
