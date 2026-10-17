#!/usr/bin/env python3
"""answer_worker.py - resume_result_test.go fixture: drains the envelope,
streams an answer as two "delta" lines, then ends the way argv[1] says:

  ok     a normal {"type":"result","status":"ok"}
  long   one 5000-rune delta, then ok (the delivery bound)
  empty  no delta at all, then ok
  error  the worker's own terminal {"type":"error",...} line
  crash  exit 1 with no terminal line
  hang   no terminal line, never exits (the per-row deadline kills it)

NOT the real worker - it only drives kahyad/internal/outbox's OnDelta
plumbing for the resumed-answer delivery (FIX-FINAL
tux-resumed-task-answer-never-delivered).
"""
import json
import sys
import time


def emit(obj):
    sys.stdout.write(json.dumps(obj, ensure_ascii=False) + "\n")
    sys.stdout.flush()


def main():
    mode = sys.argv[1] if len(sys.argv) > 1 else "ok"
    sys.stdin.buffer.read()
    emit({"type": "session", "session_id": "sess-answer"})
    if mode == "long":
        emit({"type": "delta", "text": "ş" * 5000})
    elif mode != "empty":
        emit({"type": "delta", "text": "Merhaba, "})
        emit({"type": "delta", "text": "dünya."})
    if mode in ("ok", "long", "empty"):
        emit({"type": "result", "status": "ok"})
    elif mode == "error":
        emit({"type": "error", "message": "Model çağrısı başarısız oldu."})
        sys.exit(1)
    elif mode == "crash":
        sys.exit(1)
    elif mode == "hang":
        while True:
            time.sleep(3600)


if __name__ == "__main__":
    main()
