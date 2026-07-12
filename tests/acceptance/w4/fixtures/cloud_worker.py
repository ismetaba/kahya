#!/usr/bin/env python3
"""cloud_worker.py - W4-07 scenario B fixture: stands in for the real
claude-agent-sdk worker for exactly one purpose - making ONE direct HTTP
call to kahyad's own per-task Anthropic forward-proxy listener
($ANTHROPIC_BASE_URL/v1/messages), exactly mirroring
tests/w3/fixtures/gate5_worker.py's own established "a direct HTTP call
standing in for the worker" pattern, so the W4-07 acceptance gate drives
the REAL kahyad/internal/anthproxy retry/park machinery + task/cloudretry.go
end to end (not a fake stand-in for it).

Whatever kahyad's proxy answers - a genuine upstream success relayed
through, or its own W4-04 "kahya_cloud_unreachable: upstream retries
exhausted after inline backoff" marker embedded in a 503 once its inline
retry budget is exhausted (kahyad/internal/anthproxy.MsgCloudUnreachableMarker,
byte-exact) - this fixture reacts exactly the way worker/kahya_worker/
__main__.py's own _is_cloud_unreachable branch does: the marker means emit
{"event": "cloud_unreachable"} (spawn.go's own StatusCloudUnreachable
protocol line), any other non-2xx means an ordinary {"type": "error"}, and
a 2xx means an ordinary successful result.
"""
import json
import sys
import urllib.error
import urllib.request

_CLOUD_UNREACHABLE_MARKER = "kahya_cloud_unreachable"


def emit(obj):
    sys.stdout.write(json.dumps(obj, ensure_ascii=False) + "\n")
    sys.stdout.flush()


def main():
    import os
    import time

    envelope_raw = sys.stdin.buffer.read()

    # See tests/acceptance/w4/fixtures/w2_worker.py's identical comment:
    # kahyad/internal/taint enforces monotonic taint per session_id, so a
    # redispatched (parked-then-retried) spawn's envelope-carried
    # session_id must be echoed back unchanged, never re-minted.
    session_id = None
    try:
        envelope = json.loads(envelope_raw)
        session_id = envelope.get("session_id")
    except (json.JSONDecodeError, AttributeError):
        pass
    if not session_id:
        session_id = "cloud-worker-session-%d-%d" % (os.getpid(), int(time.time() * 1000))
    emit({"type": "session", "session_id": session_id})

    base_url = os.environ.get("ANTHROPIC_BASE_URL", "")
    api_key = os.environ.get("ANTHROPIC_API_KEY", "")

    body = json.dumps({
        "model": "claude-sonnet-5",
        "max_tokens": 16,
        "messages": [{"role": "user", "content": "W4-07 scenario B probe"}],
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
        with urllib.request.urlopen(req, timeout=630) as resp:
            status = resp.getcode()
            resp_body = resp.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        status = e.code
        resp_body = e.read().decode("utf-8", "replace")
    except Exception as e:  # noqa: BLE001 - fixture script, broad catch is fine
        resp_body = "EXC:" + repr(e)

    if status >= 200 and status < 300:
        emit({"type": "delta", "text": "cloud_worker done: %d" % status})
        emit({"type": "result", "status": "ok"})
        return 0

    if _CLOUD_UNREACHABLE_MARKER in resp_body:
        emit({"event": "cloud_unreachable"})
        return 3

    emit({"type": "error", "message": "cloud_worker: upstream call failed (%d): %s" % (status, resp_body)})
    return 1


if __name__ == "__main__":
    sys.exit(main())
