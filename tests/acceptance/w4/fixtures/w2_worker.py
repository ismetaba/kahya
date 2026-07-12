#!/usr/bin/env python3
"""w2_worker.py - W4-07 scenario A fixture: stands in for the real
claude-agent-sdk worker (kahya_worker, W12-09) for exactly one purpose -
calling the dev-only w2_slow_stub MCP tool (kahyad/internal/server/
devstub.go) through the REAL kahya-mcp stdio<->UDS bridge, exactly the way
the real worker would call any MCP tool, so the W4-07 acceptance gate
drives the REAL kahyad/internal/task.Receipts intent->executing->receipt
lifecycle end to end (not a fake stand-in for it).

Protocol (mirrors tests/w3/fixtures/gate5_worker.py's own "direct call
standing in for the worker" pattern):
  - drains stdin (the task envelope JSON) - never parsed; every parameter
    this fixture needs travels via environment variables instead (set on
    the kahyad DAEMON process itself before it ever spawns this fixture -
    kahyad/internal/spawn.BuildEnv inherits the daemon's own os.Environ()
    for every worker it spawns, filtered only for a small secret
    denylist none of these names are in).
  - reads KAHYA_W2_STUB_DURATION_MS / KAHYA_W2_STUB_COUNTER_FILE.
  - execs $KAHYA_MCP_BRIDGE as a child process (inheriting this process's
    OWN environment - including KAHYA_SOCKET, KAHYA_TRACE_ID, KAHYA_TASK_ID,
    and KAHYA_MCP_REQUEST_TIMEOUT_S if the caller widened it for a long
    W4_REAL run), writes ONE JSON-RPC "tools/call" request line for
    w2_slow_stub to its stdin, and reads exactly one response line back.
  - on a genuine kill -9 of THIS process (W4-07 scenario A's whole point),
    this never runs to completion at all - the bridge's own already-issued
    HTTP request to kahyad keeps running to completion independently (see
    devstub.go's own doc comment for why that is safe and correct), this
    fixture simply never gets to see the response.
  - always emits a normal-looking worker JSONL result protocol so the SSE
    task itself completes cleanly (on the RESUMED invocation, where the
    call this time hits Receipts.Execute's idempotent-replay path and
    returns near-instantly).
"""
import json
import os
import subprocess
import sys
import time


def emit(obj):
    sys.stdout.write(json.dumps(obj, ensure_ascii=False) + "\n")
    sys.stdout.flush()


def main():
    # Written FIRST, before anything else: kahyad's own brain.db connection
    # pool is a single connection (kahyad/internal/store's own
    # SetMaxOpenConns(1) - long transactions block everything), and
    # Receipts.Execute holds that ONE connection for this effect's ENTIRE
    # duration_ms (BeginTx before calling effect, Commit only after it
    # returns) - so `GET /v1/task/status`'s own PID lookup would itself
    # block for the full duration_ms once the effect starts (it needs the
    # SAME connection). The gate's own "kill -9 the worker pid mid-call"
    # step therefore reads the pid from THIS file directly, never through
    # kahyad's HTTP API, so the harness can reliably kill the worker WHILE
    # the tool call is still genuinely in flight instead of discovering the
    # pid only after the call (and the kill window) has already passed.
    pid_file = os.environ.get("KAHYA_W2_STUB_PID_FILE", "")
    if pid_file:
        with open(pid_file, "w", encoding="utf-8") as f:
            f.write(str(os.getpid()))

    envelope_raw = sys.stdin.buffer.read()

    duration_ms = int(os.environ.get("KAHYA_W2_STUB_DURATION_MS", "0"))
    counter_file = os.environ.get("KAHYA_W2_STUB_COUNTER_FILE", "")
    bridge_path = os.environ.get("KAHYA_MCP_BRIDGE", "")

    # kahyad/internal/session_taint enforces monotonic taint per session_id
    # (a SECOND "clean" session_started insert for the SAME session_id is
    # refused) - a resumed spawn's envelope carries the ORIGINAL session_id
    # back (kahyad/internal/outbox.Dispatcher's buildResumeEnvelope), so
    # this fixture must echo THAT SAME id back on resume (never mint a
    # fresh one then), and mint a genuinely fresh one only on a first spawn
    # (envelope.session_id absent/null) - exactly what a real
    # claude-agent-sdk worker's own session lifecycle does.
    session_id = None
    try:
        envelope = json.loads(envelope_raw)
        session_id = envelope.get("session_id")
    except (json.JSONDecodeError, AttributeError):
        pass
    if not session_id:
        session_id = "w2-worker-session-%d-%d" % (os.getpid(), int(time.time() * 1000))

    emit({"type": "session", "session_id": session_id})

    if not counter_file or not bridge_path:
        emit({"type": "error", "message": "w2_worker: KAHYA_W2_STUB_COUNTER_FILE/KAHYA_MCP_BRIDGE not set"})
        return 1

    request = json.dumps({
        "jsonrpc": "2.0",
        "id": 1,
        "method": "tools/call",
        "params": {
            "name": "w2_slow_stub",
            "arguments": {"duration_ms": duration_ms, "counter_file": counter_file},
        },
    })

    try:
        proc = subprocess.Popen(
            [bridge_path],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
            env=os.environ.copy(), text=True,
        )
    except OSError as e:
        emit({"type": "error", "message": "w2_worker: exec kahya-mcp bridge failed: %s" % e})
        return 1

    try:
        proc.stdin.write(request + "\n")
        proc.stdin.flush()
        proc.stdin.close()
        response_line = proc.stdout.readline()
    finally:
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()

    if not response_line.strip():
        emit({"type": "error", "message": "w2_worker: kahya-mcp bridge returned no response"})
        return 1

    try:
        parsed = json.loads(response_line)
    except json.JSONDecodeError as e:
        emit({"type": "error", "message": "w2_worker: undecodable bridge response: %s" % e})
        return 1

    if parsed.get("error") is not None:
        emit({"type": "error", "message": "w2_worker: JSON-RPC error: %s" % parsed["error"].get("message", "")})
        return 1

    result = parsed.get("result", {})
    if result.get("isError"):
        texts = [c.get("text", "") for c in result.get("content", [])]
        emit({"type": "error", "message": "w2_worker: tool error: %s" % " ".join(texts)})
        return 1

    emit({"type": "delta", "text": "w2_worker done"})
    emit({"type": "result", "status": "ok"})
    return 0


if __name__ == "__main__":
    sys.exit(main())
