#!/usr/bin/env python3
"""fake_reader_error_worker.py - cloud_model_test.go fixture: drains
stdin, then immediately reports a terminal error line, never a result -
the "worker itself reported failure" case
kahyad/internal/reader.WorkerCloudModel.Read must surface as a Go error.
"""
import json
import sys


def main():
    sys.stdin.buffer.read()
    sys.stdout.write(json.dumps({"type": "error", "message": "simulated failure"}) + "\n")
    sys.stdout.flush()


if __name__ == "__main__":
    main()
