"""Fake worker that leaves a live child behind in its OWN process group.

kahyad/internal/spawn's TestRunReapsSurvivingGroupMemberOnNormalExit runs
this through the real spawn.Run. The worker leader forks a child that does
NOT setsid or setpgid (so it stays in the worker's process group),
redirects its own stdin/stdout/stderr to /dev/null (so it releases the
inherited pipes and Run sees a natural EOF, taking the normal-exit path
rather than a timeout), reports its pid, then sleeps. The leader waits for
the child to have reported its pid, prints a terminal result line, and
exits - leaving the child alive.

Run must SIGKILL the whole process group on this normal-exit path too (G03
/ B-14 robust review), not only on a timeout, so the child does not outlive
the task. argv[1] is the file the child writes its pid to. Stdlib only.
"""

import os
import sys
import time

PID_FILE = sys.argv[1]


def main():
    sys.stdin.read()  # the envelope spawn.Run writes after OnStart
    pid = os.fork()
    if pid == 0:
        # Child: same session AND same process group as the worker leader
        # (no setsid, no setpgid). Report the pid first, then drop the
        # inherited stdio so the leader's exit is the only thing holding the
        # pipes open.
        with open(PID_FILE + ".tmp", "w") as f:
            f.write(str(os.getpid()))
        os.rename(PID_FILE + ".tmp", PID_FILE)
        devnull = os.open(os.devnull, os.O_RDWR)
        for fd in (0, 1, 2):
            os.dup2(devnull, fd)
        time.sleep(30)  # outlive the task; the test cleans this up if the fix is absent
        os._exit(0)

    # Leader: wait until the child has published its pid, then finish.
    for _ in range(500):
        if os.path.exists(PID_FILE):
            break
        time.sleep(0.01)
    sys.stdout.write('{"type":"result","status":"ok"}\n')
    sys.stdout.flush()
    os._exit(0)


if __name__ == "__main__":
    main()
