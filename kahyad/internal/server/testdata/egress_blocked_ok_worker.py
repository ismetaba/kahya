#!/usr/bin/env python3
"""egress_blocked_ok_worker.py - FIX-U-4 server test fixture (a sanitized,
signal-file-free copy of tests/w3/fixtures/gate5_worker.py's shape): reads
the envelope off stdin, makes ONE model call to $ANTHROPIC_BASE_URL/
v1/messages through the per-task proxy (FIX-I-1 auth shape: X-Kahya-Task-
Token from $ANTHROPIC_CUSTOM_HEADERS + a placeholder Authorization),
IGNORES the response status entirely (a 403 from kahyad's own egress gate
included) and reports {"type":"result","status":"ok"} with exit 0 - the
"never trust the worker's terminal line" scenario: kahyad's own egress
verdict must still decide the task.
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
    for line in os.environ.get("ANTHROPIC_CUSTOM_HEADERS", "").splitlines():
        name, sep, value = line.partition(":")
        if sep and name.strip().lower() == "x-kahya-task-token":
            return value.strip()
    return os.environ.get("ANTHROPIC_API_KEY", "")


def main():
    json.loads(sys.stdin.buffer.read().decode("utf-8"))
    body = json.dumps({"model": "claude-sonnet-5-5", "messages": []}).encode("utf-8")
    req = urllib.request.Request(
        os.environ.get("ANTHROPIC_BASE_URL", "") + "/v1/messages",
        data=body,
        method="POST",
        headers={
            "Content-Type": "application/json",
            "X-Kahya-Task-Token": _task_token(),
            "Authorization": "Bearer fixture-upstream-credential",
        },
    )
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            resp.read()
    except urllib.error.HTTPError:
        pass  # deliberately ignored: this worker always claims success
    emit({"type": "result", "status": "ok"})
    sys.exit(0)


if __name__ == "__main__":
    main()
