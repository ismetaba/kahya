#!/usr/bin/env python3
"""gate5_worker.py - W3-10 Gate 5 fixture: stands in for the real
claude-agent-sdk worker (kahya_worker, W12-09) for exactly one purpose -
proving the W3-08 proxy backstop (kahyad/internal/secretlane.
NewProxyBackstopHook) independently blocks a cloud call once a task's
persisted lane flips to "secret", even for a task whose own worker WAS
already spawned while lane was still "normal" (task.go's own comment:
"the SECOND, independent layer of defense in case [the lane==secret
skip] ever changes").

Protocol (all via environment variables kahyad's spawn.BuildEnv already
sets/inherits - see kahyad/internal/spawn/spawn.go):
  - drains stdin (the task envelope JSON), like every other test fixture
    worker in kahyad/internal/spawn/testdata/*.py.
  - polls for KAHYA_GATE5_SIGNAL_FILE to exist (created by the Go test
    once it has flipped the task's tasks.lane row to 'secret' directly in
    brain.db - see gate_test.go's own doc comment for why this direct
    write is necessary: kahyad/internal/secretlane.Escalate, the
    production function that would do this, has no wired caller yet).
  - once signaled, POSTs one OpenAI/Anthropic-shaped request to
    "$ANTHROPIC_BASE_URL/v1/messages" with header
    "x-api-key: $ANTHROPIC_API_KEY" (exactly the real worker's own
    authentication convention, HANDOFF S4 IPC - the API key never leaves
    kahyad, this per-task local token is the only thing this process
    ever sees) - a "direct HTTP call standing in for the worker" per this
    task's own spec wording.
  - writes "STATUS:<code>\n<response body>" to KAHYA_GATE5_OUT_FILE so the
    Go test can read the ACTUAL response the W3-08 backstop returned,
    without needing to know the ephemeral per-task proxy's own address
    (which is never exposed outside kahyad's own process - only this
    worker, which the daemon spawns WITH that address as an env var, ever
    sees it).
  - always emits a normal-looking worker JSONL result so the SSE task
    itself completes cleanly regardless of what the backstop did.
"""
import json
import os
import sys
import time
import urllib.error
import urllib.request


def emit(obj):
    sys.stdout.write(json.dumps(obj, ensure_ascii=False) + "\n")
    sys.stdout.flush()


def main():
    sys.stdin.buffer.read()

    signal_path = os.environ.get("KAHYA_GATE5_SIGNAL_FILE", "")
    out_path = os.environ.get("KAHYA_GATE5_OUT_FILE", "")
    base_url = os.environ.get("ANTHROPIC_BASE_URL", "")
    api_key = os.environ.get("ANTHROPIC_API_KEY", "")

    deadline = time.time() + 20.0
    while signal_path and not os.path.exists(signal_path):
        if time.time() > deadline:
            emit({"type": "result", "status": "error", "message": "gate5_worker: signal file never appeared"})
            return
        time.sleep(0.05)

    body = json.dumps({
        "model": "claude-sonnet-5",
        "max_tokens": 16,
        "messages": [{"role": "user", "content": "IBAN TR33 0006 1005 1978 6457 8413 26 icin odeme talimati"}],
    }).encode("utf-8")

    status = -1
    resp_body = ""
    try:
        req = urllib.request.Request(
            base_url.rstrip("/") + "/v1/messages",
            data=body,
            headers={"Content-Type": "application/json", "x-api-key": api_key},
            method="POST",
        )
        with urllib.request.urlopen(req, timeout=10) as resp:
            status = resp.getcode()
            resp_body = resp.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        status = e.code
        resp_body = e.read().decode("utf-8", "replace")
    except Exception as e:  # noqa: BLE001 - fixture script, broad catch is fine
        resp_body = "EXC:" + repr(e)

    if out_path:
        with open(out_path, "w", encoding="utf-8") as f:
            f.write("STATUS:%d\n%s" % (status, resp_body))

    emit({"type": "delta", "text": "gate5_worker done"})
    emit({"type": "result", "status": "ok"})


if __name__ == "__main__":
    main()
