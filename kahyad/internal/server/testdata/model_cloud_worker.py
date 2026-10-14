#!/usr/bin/env python3
"""model_cloud_worker.py - W4-08 router_task_test.go fixture: reads the
envelope JSON off stdin, then makes ONE real HTTP POST to
$ANTHROPIC_BASE_URL/v1/messages (FIX-I-1 auth shape: X-Kahya-Task-Token
from $ANTHROPIC_CUSTOM_HEADERS + a placeholder Authorization) using the
envelope's OWN "model" field in the request body (unlike kahyad/internal/outbox/testdata/cloud_call_worker.
py, which hardcodes "claude-sonnet-5" - that fixture exists to prove a
resumed cloud-lane redispatch reaches the cloud at all, not to prove WHICH
model the router chose). This is what lets a test assert the fake
upstream's own recorded request body actually carries the router-selected
model (e.g. claude-fable-5 for a "derin düşün" task) end to end through the
real per-task anthproxy.Proxy listener - including that listener's own
Fable-5 request-shaping (betas/fallbacks), reusing the SAME W4-04 fake-
upstream harness pattern kahyad/internal/anthproxy's own tests use, just
driven from a real POST /v1/task instead of a raw HTTP call against the
proxy directly.
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
    raw = sys.stdin.buffer.read().decode("utf-8")
    envelope = json.loads(raw)
    model = envelope.get("model", "")

    base_url = os.environ.get("ANTHROPIC_BASE_URL", "")
    body = json.dumps({"model": model, "messages": []}).encode("utf-8")
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
