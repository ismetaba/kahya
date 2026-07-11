#!/usr/bin/env python3
"""crash_immediately.py - supervisor_test.go fixture: exits nonzero right
away, every time it is started - exercises Supervisor's restart-with-
backoff path (TestSupervisorRestartsOnCrashWithBackoff) without ever
answering /health.
"""
import sys

if __name__ == "__main__":
    sys.exit(1)
