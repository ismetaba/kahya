#!/usr/bin/env python3
"""echo_session_worker.py - dispatcher_test.go fixture: reads the envelope
JSON off stdin, echoes it back byte-exact as a single "delta" line (so the
test can assert the RESUMED envelope's session_id/resume/task_id/trace_id
fields arrived intact), emits a "session" line (its own new session id if
this is a fresh spawn, or the envelope's own resumed session_id if
resume:true was set), then reports a normal "ok" result. NOT the real
worker - this only exercises kahyad/internal/outbox's own envelope/spawn
plumbing.
"""
import json
import sys


def emit(obj):
    sys.stdout.write(json.dumps(obj, ensure_ascii=False) + "\n")
    sys.stdout.flush()


def main():
    raw = sys.stdin.buffer.read().decode("utf-8")
    envelope = json.loads(raw)
    emit({"type": "delta", "text": raw})
    session_id = envelope.get("session_id") or "new-session-from-worker"
    emit({"type": "session", "session_id": session_id})
    emit({"type": "result", "status": "ok"})


if __name__ == "__main__":
    main()
