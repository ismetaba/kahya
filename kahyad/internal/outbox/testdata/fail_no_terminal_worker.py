#!/usr/bin/env python3
"""fail_no_terminal_worker.py - dispatcher_test.go fixture: drains stdin,
emits one delta, then exits 1 WITHOUT ever sending a terminal
"result"/"error" line - the "worker exit != 0 without a result line" case
Dispatcher must leave unacknowledged (lease expiry re-claims), never mark
delivered.
"""
import sys


def main():
    sys.stdin.buffer.read()
    sys.stdout.write('{"type":"delta","text":"before-exit"}\n')
    sys.stdout.flush()
    sys.exit(1)


if __name__ == "__main__":
    main()
