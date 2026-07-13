#!/usr/bin/env python3
"""hang_worker.py - W6-03 executor_test.go fixture: drains stdin, spawns a
grandchild subprocess in the SAME process group (NOT setsid'd, unlike
kahyad/internal/spawn's own detached_grandchild_worker.py fixture - this
one deliberately stays reachable by a process-group kill), then hangs
forever. Mirrors kahyad/internal/spawn/testdata/hang_worker.py exactly
(this package owns its own copy rather than reaching across a package
boundary for testdata, matching every other package's own-testdata-dir
convention in this repo) - "sleep 300", not 3600, to match this task's
own acceptance-criterion wording verbatim ("stub worker script that forks
a child (sleep 300 &)"). Only a process-GROUP kill (this package's own
Executor.HaltTask, or spawn.Run's timeout path) ever ends it.
"""
import subprocess
import sys
import time


def main():
    sys.stdin.buffer.read()
    subprocess.Popen(["sleep", "300"])
    time.sleep(3600)


if __name__ == "__main__":
    main()
