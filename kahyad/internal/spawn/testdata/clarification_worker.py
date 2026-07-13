#!/usr/bin/env python3
"""clarification_worker.py - W78-07 fixture: a worker that asked the user a
clarifying question before acting, so it emits the non-terminal
{"event":"clarification_turn"} stdout line (the real worker's
_CLARIFICATION_TURN_EVENT) right before its terminal "result". Proves
kahyad's spawn->OnClarification->logClarificationTurn plumbing ledgers a
kind="clarification_turn" row for the north-star metric. NOT the real
worker (the açıklama-turu DECISION itself - is-a-question + no-tool-use -
is exercised in worker/tests/test_main.py); this only drives kahyad's side
of the signal.
"""
import json
import sys


def emit(obj):
    sys.stdout.write(json.dumps(obj, ensure_ascii=False) + "\n")
    sys.stdout.flush()


def main():
    sys.stdin.buffer.read()
    emit({"type": "delta", "text": "Hangi hesaptan bahsediyorsun?"})
    emit({"event": "clarification_turn"})
    emit({"type": "result", "status": "ok"})


if __name__ == "__main__":
    main()
