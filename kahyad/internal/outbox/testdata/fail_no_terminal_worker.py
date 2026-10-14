#!/usr/bin/env python3
"""fail_no_terminal_worker.py - dispatcher_test.go fixture: drains stdin,
emits one delta, then exits 1 WITHOUT ever sending a terminal
"result"/"error" line - the "worker exit != 0 without a result line" case.
Since G10-E1 the Dispatcher acks that row (outbox.redispatch_crashed) and
leaves the task 'executing': only the resume scan's counted path
(task_max_crash_resumes) redispatches it, never a lease-expiry re-claim.
"""
import sys


def main():
    sys.stdin.buffer.read()
    sys.stdout.write('{"type":"delta","text":"before-exit"}\n')
    sys.stdout.flush()
    sys.exit(1)


if __name__ == "__main__":
    main()
