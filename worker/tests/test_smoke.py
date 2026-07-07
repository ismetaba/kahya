"""Smoke test for the worker toolchain: SDK importable, lock discipline intact."""

import re
import unittest
from pathlib import Path

LOCK_FILE = Path(__file__).resolve().parents[1] / "requirements.lock"


class TestWorkerToolchain(unittest.TestCase):
    def test_claude_agent_sdk_importable(self):
        import claude_agent_sdk  # noqa: F401

    def test_lock_pins_claude_agent_sdk_exactly(self):
        lock_lines = LOCK_FILE.read_text(encoding="utf-8").splitlines()
        pins = [l for l in lock_lines if l.startswith("claude-agent-sdk==")]
        self.assertEqual(
            len(pins), 1, "requirements.lock must pin claude-agent-sdk exactly once"
        )
        self.assertRegex(pins[0], r"^claude-agent-sdk==[0-9][A-Za-z0-9.]*$")


if __name__ == "__main__":
    unittest.main()
