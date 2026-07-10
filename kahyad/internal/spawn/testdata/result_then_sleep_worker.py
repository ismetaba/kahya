#!/usr/bin/env python3
"""result_then_sleep_worker.py - spawn_test.go regression fixture for
BLOCKER 1: emits a terminal {"type":"result","status":"ok"} line
immediately, then keeps running (sleeping) well past a short ctx timeout.
Run must still report the ALREADY-observed "ok" result, not relabel it
StatusTimeout just because ctx's deadline arrived before this script got
around to exiting on its own - and it must still kill this process (no
orphan) rather than let it sleep out its full duration.
"""
import sys
import time


def main():
    sys.stdin.buffer.read()
    sys.stdout.write('{"type":"result","status":"ok"}\n')
    sys.stdout.flush()
    time.sleep(5)


if __name__ == "__main__":
    main()
