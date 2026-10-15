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
    once the task's tasks.lane row should be 'secret': the first Gate 5
    subtest flips it directly in brain.db (a task whose lane flips while
    its worker is alive, whatever the ingestion point), while
    TestGate5_FsReadSecretContentEscalatesAndBlocksCloud leaves it to the
    real production path - this worker's own fs_read below, escalated by
    FIX-B-6).
  - optional (FIX-B-6): when KAHYA_GATE5_FS_READ_PATH is set, calls the
    fs_read MCP tool itself over kahyad's UDS ($KAHYA_SOCKET, POST
    /v1/mcp, NO control secret - exactly the worker plane), declaring
    X-Kahya-Task-Id from KAHYA_GATE5_FORGED_TASK_ID when set (a forged
    claim kahyad must ignore: the caller is attributed by its UDS peer
    session) or KAHYA_TASK_ID otherwise, and writes "STATUS:<code>\n<body>"
    to KAHYA_GATE5_FS_READ_OUT_FILE.
  - once signaled (and after the optional fs_read), POSTs
    KAHYA_GATE5_CLOUD_CALLS (default 1) OpenAI/Anthropic-shaped requests to
    "$ANTHROPIC_BASE_URL/v1/messages" with the per-task local token in
    "X-Kahya-Task-Token" (read from $ANTHROPIC_CUSTOM_HEADERS, FIX-I-1)
    plus a placeholder Authorization - exactly the real worker CLI's
    passthrough shape (HANDOFF S4 IPC - the API key never leaves kahyad,
    this per-task local token is the only kahyad secret this process ever
    sees) - a "direct HTTP call standing in for the worker" per this
    task's own spec wording.
  - writes "STATUS:<code>\n<response body>" (one block per call, joined
    by a newline) to KAHYA_GATE5_OUT_FILE so the
    Go test can read the ACTUAL response the W3-08 backstop returned,
    without needing to know the ephemeral per-task proxy's own address
    (which is never exposed outside kahyad's own process - only this
    worker, which the daemon spawns WITH that address as an env var, ever
    sees it).
  - always emits a normal-looking worker JSONL result so the SSE task
    itself completes cleanly regardless of what the backstop did.
"""
import http.client
import json
import os
import socket
import sys
import time
import urllib.error
import urllib.request


class UnixHTTPConnection(http.client.HTTPConnection):
    """An HTTPConnection over kahyad's unix domain socket."""

    def __init__(self, path, timeout=10):
        super().__init__("kahyad", timeout=timeout)
        self._path = path

    def connect(self):
        sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        sock.settimeout(self.timeout)
        sock.connect(self._path)
        self.sock = sock


def fs_read_over_uds(path):
    """POST one fs_read tools/call to kahyad's /v1/mcp as this worker."""
    task_id = os.environ.get("KAHYA_GATE5_FORGED_TASK_ID", "") or os.environ.get("KAHYA_TASK_ID", "")
    body = json.dumps({
        "jsonrpc": "2.0", "id": 1, "method": "tools/call",
        "params": {"name": "fs_read", "arguments": {"path": path}},
    }).encode("utf-8")
    conn = UnixHTTPConnection(os.environ.get("KAHYA_SOCKET", ""))
    try:
        conn.request("POST", "/v1/mcp", body=body, headers={
            "Content-Type": "application/json",
            "Accept": "application/json, text/event-stream",
            "X-Kahya-Trace-Id": os.environ.get("KAHYA_TRACE_ID", ""),
            "X-Kahya-Task-Id": task_id,
        })
        resp = conn.getresponse()
        return resp.status, resp.read().decode("utf-8", "replace")
    except Exception as e:  # noqa: BLE001 - fixture script, broad catch is fine
        return -1, "EXC:" + repr(e)
    finally:
        conn.close()


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


def cloud_call(base_url, body):
    """POST one request to the per-task anthproxy; (status, body)."""
    try:
        req = urllib.request.Request(
            base_url.rstrip("/") + "/v1/messages",
            data=body,
            headers=_auth_headers(),
            method="POST",
        )
        with urllib.request.urlopen(req, timeout=10) as resp:
            return resp.getcode(), resp.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace")
    except Exception as e:  # noqa: BLE001 - fixture script, broad catch is fine
        return -1, "EXC:" + repr(e)


def emit(obj):
    sys.stdout.write(json.dumps(obj, ensure_ascii=False) + "\n")
    sys.stdout.flush()


def main():
    sys.stdin.buffer.read()

    signal_path = os.environ.get("KAHYA_GATE5_SIGNAL_FILE", "")
    out_path = os.environ.get("KAHYA_GATE5_OUT_FILE", "")
    base_url = os.environ.get("ANTHROPIC_BASE_URL", "")

    deadline = time.time() + 20.0
    while signal_path and not os.path.exists(signal_path):
        if time.time() > deadline:
            emit({"type": "result", "status": "error", "message": "gate5_worker: signal file never appeared"})
            return
        time.sleep(0.05)

    body = json.dumps({
        "model": "claude-sonnet-5-5",
        "max_tokens": 16,
        "messages": [{"role": "user", "content": "IBAN TR33 0006 1005 1978 6457 8413 26 icin odeme talimati"}],
    }).encode("utf-8")

    read_path = os.environ.get("KAHYA_GATE5_FS_READ_PATH", "")
    if read_path:
        read_status, read_body = fs_read_over_uds(read_path)
        read_out = os.environ.get("KAHYA_GATE5_FS_READ_OUT_FILE", "")
        if read_out:
            with open(read_out, "w", encoding="utf-8") as f:
                f.write("STATUS:%d\n%s" % (read_status, read_body))

    calls = max(1, int(os.environ.get("KAHYA_GATE5_CLOUD_CALLS", "1") or "1"))
    blocks = []
    for _ in range(calls):
        status, resp_body = cloud_call(base_url, body)
        blocks.append("STATUS:%d\n%s" % (status, resp_body))

    if out_path:
        with open(out_path, "w", encoding="utf-8") as f:
            f.write("\n".join(blocks))

    emit({"type": "delta", "text": "gate5_worker done"})
    emit({"type": "result", "status": "ok"})


if __name__ == "__main__":
    main()
