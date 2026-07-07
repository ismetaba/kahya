"""W0-02 smoke test: the pinned worker environment is importable and lock discipline holds."""

import unittest
from pathlib import Path

import claude_agent_sdk


class SmokeTest(unittest.TestCase):
    def test_sdk_importable(self):
        self.assertTrue(hasattr(claude_agent_sdk, "__name__"))

    def test_lock_pins_exact_sdk_version(self):
        lock = Path(__file__).resolve().parents[1] / "requirements.lock"
        lines = lock.read_text(encoding="utf-8").splitlines()
        self.assertTrue(
            any(line.startswith("claude-agent-sdk==") for line in lines),
            "requirements.lock must pin claude-agent-sdk to an exact version",
        )


if __name__ == "__main__":
    unittest.main()
