#!/usr/bin/env python3
"""error_line_worker.py - FIX-I-2 task_resilience_test.go fixture: drains the
envelope, prints ONE explicit terminal error line (the worker itself saying
"this task is over", like the real worker's model-call-failed line) and
exits 1. kahyad must move the task to 'failed' (nothing is left to resume),
unlike an exit without any terminal line, which stays 'executing' for the
resume scan.
"""
import json
import sys


def main():
    sys.stdin.buffer.read()
    sys.stdout.write(json.dumps({"type": "error", "message": "model hata"}, ensure_ascii=False) + "\n")
    sys.stdout.flush()
    sys.exit(1)


if __name__ == "__main__":
    main()
