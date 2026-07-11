#!/usr/bin/env python3
"""slow_session_worker.py - dispatcher_test.go fixture (task durability
BLOCKER 2 regression coverage): reads the envelope off stdin, emits a
"session" line immediately (so OnSession/LiveRegistry.Register both fire
right away, exactly like a real worker that reports its session before
doing any real work), sleeps for argv[1] seconds (a stand-in for a
long-running task that outlives a short outbox lease), then reports a
normal "ok" result. Lets a test hold a live worker "running" for a known,
controllable duration while it exercises the dispatcher's lease-renewal /
IsLive-skip / overlap-guard logic.
"""
import json
import sys
import time


def emit(obj):
    sys.stdout.write(json.dumps(obj, ensure_ascii=False) + "\n")
    sys.stdout.flush()


def main():
    raw = sys.stdin.buffer.read().decode("utf-8")
    envelope = json.loads(raw)
    sleep_seconds = float(sys.argv[1]) if len(sys.argv) > 1 else 0.2
    session_id = envelope.get("session_id") or "new-session-from-worker"
    emit({"type": "session", "session_id": session_id})
    time.sleep(sleep_seconds)
    emit({"type": "result", "status": "ok"})


if __name__ == "__main__":
    main()
