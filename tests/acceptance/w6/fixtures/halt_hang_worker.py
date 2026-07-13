#!/usr/bin/env python3
"""halt_hang_worker.py - W6-04 gate2 (halt survives a real daemon restart)
fixture: a FAKE worker that (1) records its own pid so the harness can prove
the halt killed it, (2) calls the dev-only w2_slow_stub MCP tool through the
REAL kahya-mcp stdio<->UDS bridge - which, since the harness does NOT promote
w2_slow_stub, makes kahyad mint a pending_approvals row keyed to the task
(the halt then invalidates it) - and (3) HANGS (never emits a terminal
result) so the task stays 'executing' with a live worker until the halt's
process-group SIGKILL reaches it.

Protocol (mirrors tests/acceptance/w4/fixtures/w2_worker.py's own "direct
call standing in for the worker" pattern):
  - writes os.getpid() to env KAHYA_W6_PID_FILE FIRST (before the bridge call
    that mints the approval), so the harness can capture the pid the halt
    must kill even while the tool call is in flight.
  - emits one {"type":"session","session_id":"w6-halt-<pid>"} line.
  - execs $KAHYA_MCP_BRIDGE, writes ONE JSON-RPC "tools/call" request line
    for w2_slow_stub, reads exactly one response line back and IGNORES it
    (an unpromoted W2 call returns a needs-approval response; this fixture
    does not act on it - its only purpose was to make kahyad mint the
    pending approval).
  - then sleeps ~60000 ms so the task remains genuinely executing with a
    live worker. NEVER emits a terminal "result" line - the worker is only
    ever ended by the halt's own SIGKILL of its process group.
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
    pid_file = os.environ.get("KAHYA_W6_PID_FILE", "")
    if pid_file:
        with open(pid_file, "w", encoding="utf-8") as f:
            f.write(str(os.getpid()))

    # Drain (never parsed) - every parameter travels via env vars, exactly
    # like w2_worker.py.
    _ = sys.stdin.buffer.read()

    emit({"type": "session", "session_id": "w6-halt-%d" % os.getpid()})

    duration_ms = int(os.environ.get("KAHYA_W2_STUB_DURATION_MS", "0"))
    counter_file = os.environ.get("KAHYA_W2_STUB_COUNTER_FILE", "")
    bridge_path = os.environ.get("KAHYA_MCP_BRIDGE", "")

    if bridge_path and counter_file:
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
            proc.stdin.write(request + "\n")
            proc.stdin.flush()
            proc.stdin.close()
            # Read exactly one response line (the needs-approval response for
            # an unpromoted W2 call) and ignore it. Its sole purpose was to
            # trigger kahyad's pending-approval mint.
            _ = proc.stdout.readline()
            try:
                proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                proc.kill()
        except OSError:
            # Even if the bridge could not be exec'd, still hang below so the
            # task stays executing - the harness's pending-approval poll will
            # be the thing that fails loudly (and correctly) in that case.
            pass

    # Hang so the task stays executing with a live worker until the halt's
    # process-group SIGKILL arrives. Never emit a terminal result.
    time.sleep(60)


if __name__ == "__main__":
    main()
