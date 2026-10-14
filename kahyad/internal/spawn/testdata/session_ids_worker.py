#!/usr/bin/env python3
"""session_ids_worker.py - spawn_test.go fixture for
TestRunStartsWorkerAsSessionLeader (G03 / B-14): reports this process's
own pid, process-group id and session id as one "delta" line (JSON text),
then a normal "ok" result. NOT the real worker - this only exercises how
kahyad's spawn.Run starts a worker process.
"""
import json
import os
import sys


def emit(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()


def main():
    sys.stdin.read()
    ids = {"pid": os.getpid(), "pgid": os.getpgid(0), "sid": os.getsid(0)}
    emit({"type": "delta", "text": json.dumps(ids)})
    emit({"type": "result", "status": "ok"})


if __name__ == "__main__":
    main()
