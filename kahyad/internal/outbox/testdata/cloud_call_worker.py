#!/usr/bin/env python3
"""cloud_call_worker.py - dispatcher_test.go fixture (W4-04): reads the
envelope off stdin (ignored beyond consuming it), then makes ONE real
HTTP POST to $ANTHROPIC_BASE_URL/v1/messages carrying the per-task token
in X-Kahya-Task-Token (read from $ANTHROPIC_CUSTOM_HEADERS, FIX-I-1) plus a
placeholder Authorization - exactly the real worker CLI's passthrough
shape against kahyad's own per-task anthproxy.Proxy listener - and reports
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


def _task_token():
    """FIX-I-1: the per-task token travels in ANTHROPIC_CUSTOM_HEADERS
    ("X-Kahya-Task-Token: kahya-task-<hex32>") in BOTH credential modes;
    ANTHROPIC_API_KEY is only set in keychain mode (legacy carrier)."""
    for line in os.environ.get("ANTHROPIC_CUSTOM_HEADERS", "").splitlines():
        name, sep, value = line.partition(":")
        if sep and name.strip().lower() == "x-kahya-task-token":
            return value.strip()
    return os.environ.get("ANTHROPIC_API_KEY", "")


def _auth_headers():
    """Exactly the header pair the real worker's CLI sends in passthrough
    mode: the local token in its own header + the CLI's own upstream
    credential (here a fixture placeholder - kahyad never inspects its
    value, it only checks that one exists)."""
    return {
        "Content-Type": "application/json",
        "X-Kahya-Task-Token": _task_token(),
        "Authorization": "Bearer fixture-upstream-credential",
    }


def main():
    sys.stdin.buffer.read()  # consume the envelope; contents unused
    emit({"type": "session", "session_id": "cloud-call-session"})

    base_url = os.environ.get("ANTHROPIC_BASE_URL", "")
    body = json.dumps({"model": "claude-sonnet-5-5", "messages": []}).encode("utf-8")
    req = urllib.request.Request(
        base_url + "/v1/messages",
        data=body,
        method="POST",
        headers=_auth_headers(),
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
