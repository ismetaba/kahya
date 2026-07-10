#!/usr/bin/env python3
"""echo_worker.py - W12-07 spawn_test.go fixture (a): reads the envelope
JSON off stdin, echoes it back byte-exact as a single "delta" line so the
test can assert the envelope (including a Turkish prompt) arrived intact,
then echoes every KAHYA_*/ANTHROPIC_* env var kahyad's spawn.BuildEnv is
supposed to set, then reports a normal "ok" result. NOT the real worker
(W12-09) - this only exercises kahyad's spawn/env plumbing.
"""
import json
import os
import sys


def emit(obj):
    sys.stdout.write(json.dumps(obj, ensure_ascii=False) + "\n")
    sys.stdout.flush()


def main():
    raw = sys.stdin.buffer.read().decode("utf-8")
    emit({"type": "delta", "text": raw})
    for name in (
        "KAHYA_TASK_ID",
        "KAHYA_TRACE_ID",
        "KAHYA_SOCKET",
        "KAHYA_LOG_DIR",
        "ANTHROPIC_BASE_URL",
        "ANTHROPIC_API_KEY",
    ):
        emit({"type": "delta", "text": "%s=%s" % (name, os.environ.get(name, ""))})
    emit({"type": "result", "status": "ok"})


if __name__ == "__main__":
    main()
