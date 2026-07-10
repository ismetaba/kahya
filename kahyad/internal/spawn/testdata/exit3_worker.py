#!/usr/bin/env python3
"""exit3_worker.py - W12-07 spawn_test.go fixture (c): drains stdin, emits
one delta, then exits 3 WITHOUT ever sending a terminal "result"/"error"
line - the "worker exit != 0 without a result line" case task.go must turn
into a task_error.
"""
import sys


def main():
    sys.stdin.buffer.read()
    sys.stdout.write('{"type":"delta","text":"before-exit"}\n')
    sys.stdout.flush()
    sys.exit(3)


if __name__ == "__main__":
    main()
