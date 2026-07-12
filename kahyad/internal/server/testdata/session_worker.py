#!/usr/bin/env python3
"""session_worker.py - W4-03 task_test.go fixture: reads the envelope JSON
off stdin, emits a "session" line whose session_id is derived
deterministically from the envelope's own task_id (so a test can predict
it without any extra plumbing), one delta line, then reports a normal
"ok" result. NOT the real worker - this only exercises kahyad's own
OnSession -> persistSessionStarted plumbing (W4-03 task spec step 1a).
"""
import json
import sys


def emit(obj):
    sys.stdout.write(json.dumps(obj, ensure_ascii=False) + "\n")
    sys.stdout.flush()


def main():
    raw = sys.stdin.buffer.read().decode("utf-8")
    envelope = json.loads(raw)
    session_id = "sess-for-" + envelope["task_id"]
    emit({"type": "session", "session_id": session_id})
    emit({"type": "delta", "text": "ok"})
    emit({"type": "result", "status": "ok"})


if __name__ == "__main__":
    main()
