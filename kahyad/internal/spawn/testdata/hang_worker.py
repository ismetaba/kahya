#!/usr/bin/env python3
"""hang_worker.py - W12-07 spawn_test.go fixture (b): drains stdin, spawns
a grandchild subprocess (so the test can prove killing the process GROUP
also reaps a process this script itself started, not just this script's
own pid), then hangs forever. Only SIGKILL (via Run's timeout path) ever
ends it.
"""
import subprocess
import sys
import time


def main():
    sys.stdin.buffer.read()
    subprocess.Popen(["sleep", "3600"])
    time.sleep(3600)


if __name__ == "__main__":
    main()
