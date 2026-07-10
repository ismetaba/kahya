"""Tests for kahya_worker.logging: every line is JSONL, carries trace_id
(HANDOFF §4 IPC ⚑), and lands in $KAHYA_LOG_DIR/worker.jsonl."""

import json
import os
import tempfile
import unittest

import _pathfix  # noqa: F401

from kahya_worker import logging as wlog


class TestLogging(unittest.TestCase):
    def test_every_line_carries_configured_trace_id(self) -> None:
        with tempfile.TemporaryDirectory() as log_dir:
            wlog.configure(log_dir, "trace-xyz")
            wlog.log("info", "one")
            wlog.log("warn", "two", detail="something")
            wlog.log("error", "three", tool="memory_write", decision="deny")

            path = os.path.join(log_dir, "worker.jsonl")
            with open(path, encoding="utf-8") as f:
                lines = [json.loads(l) for l in f if l.strip()]

        self.assertEqual(len(lines), 3)
        for line in lines:
            self.assertEqual(line["trace_id"], "trace-xyz")
            self.assertIn("ts", line)
            self.assertIn("level", line)
            self.assertIn("event", line)

    def test_reconfigure_switches_log_file_and_trace_id(self) -> None:
        with tempfile.TemporaryDirectory() as d1, tempfile.TemporaryDirectory() as d2:
            wlog.configure(d1, "trace-1")
            wlog.log("info", "first")

            wlog.configure(d2, "trace-2")
            wlog.log("info", "second")

            with open(os.path.join(d1, "worker.jsonl"), encoding="utf-8") as f:
                lines_d1 = [json.loads(l) for l in f if l.strip()]
            with open(os.path.join(d2, "worker.jsonl"), encoding="utf-8") as f:
                lines_d2 = [json.loads(l) for l in f if l.strip()]

        self.assertEqual(len(lines_d1), 1)
        self.assertEqual(lines_d1[0]["trace_id"], "trace-1")
        self.assertEqual(len(lines_d2), 1)
        self.assertEqual(lines_d2[0]["trace_id"], "trace-2")

    def test_preserves_turkish_text_byte_exact(self) -> None:
        with tempfile.TemporaryDirectory() as log_dir:
            wlog.configure(log_dir, "trace-tr")
            wlog.log("warn", "memory_search_failed", detail="Kadıköy'de iki daire gezdik.")

            path = os.path.join(log_dir, "worker.jsonl")
            with open(path, encoding="utf-8") as f:
                raw = f.read()

        self.assertIn("Kadıköy'de iki daire gezdik.", raw)


if __name__ == "__main__":
    unittest.main()
