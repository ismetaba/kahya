#!/usr/bin/env python3
"""hang_worker.py - dispatcher_timeout_test.go fixture (FIX-I-3): drains
stdin, spawns a grandchild subprocess in the SAME process group (so the
test can prove the redispatch deadline kills the whole group, not just
this script's own pid), then hangs forever without ever sending a terminal
stdout line. A copy of kahyad/internal/halt/testdata/hang_worker.py (this
package owns its own testdata, the repo's per-package convention). Only a
process-group kill - Dispatcher's per-row task deadline, or its base
(daemon lifecycle) context ending - ever stops it.
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
