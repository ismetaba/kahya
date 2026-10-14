#!/usr/bin/env python3
"""no_stdin_read_worker.py - I-15 fixture: never reads stdin, never writes a
protocol line, just sleeps - a worker stuck before it ever consumes its
envelope (e.g. a Python import hang, or a process waiting on a lock). Only a
process-group kill ends it.
"""
import time

time.sleep(3600)
