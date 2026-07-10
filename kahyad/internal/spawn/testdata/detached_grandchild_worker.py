#!/usr/bin/env python3
"""detached_grandchild_worker.py - spawn_test.go regression fixture for
BLOCKER 2: spawns a grandchild that escapes THIS script's own process
GROUP via start_new_session=True (setsid) - so kahyad's killGroup
(kill(-pgid)) can never reach it, no matter what - then exits itself.
Popen here does not redirect the grandchild's stdout/stderr away from the
ones it inherits from this script, so the grandchild keeps holding a
write-end of both open even after this script's own process is gone,
meaning kahyad's reader goroutines never see a natural EOF from it either.

Uses a short sleep (not e.g. 3600s) specifically so the test needs no
explicit pid capture/cleanup of its own - the detached process exits on
its own well within the test run and leaves no stray process behind.
"""
import subprocess
import sys


def main():
    sys.stdin.buffer.read()
    subprocess.Popen(["sleep", "3"], start_new_session=True)


if __name__ == "__main__":
    main()
