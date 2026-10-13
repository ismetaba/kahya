"""Smoke test for the worker toolchain: SDK importable, lock discipline intact."""

import os
import re
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

LOCK_FILE = Path(__file__).resolve().parents[1] / "requirements.lock"
REPO_ROOT = Path(__file__).resolve().parents[2]


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



@unittest.skipUnless(shutil.which("make"), "make not on PATH")
class TestMakeVenvTarget(unittest.TestCase):
    """G12-N3-E1: `make venv` must never leave a half-built venv behind.

    The spawn/e2e Go tests only probe for worker/.venv/bin/python, so a
    venv whose pip install failed (python3 -m venv ok, claude_agent_sdk
    never installed) would turn them red against a worker that cannot
    even import its SDK. A WORKING venv (claude_agent_sdk importable) must
    survive a failed re-install, though: deleting it would force a full
    reinstall over one transient failure. Hermetic: PIP_NO_INDEX=1 plus a
    lock naming a package that cannot exist, so pip fails without ever
    touching the network; VENV/WORKER_LOCK point at a temp dir, never at
    the real worker/.venv.
    """

    BOGUS_PIN = "kahya-nonexistent-package-for-make-venv-test==0.0.0\n"

    def _make_venv(self, venv: Path, lock: Path) -> subprocess.CompletedProcess:
        env = dict(os.environ, PIP_NO_INDEX="1", PIP_DISABLE_PIP_VERSION_CHECK="1")
        # Isolate from a parent `make test` (jobserver fds, -k/-s flags).
        for var in ("MAKEFLAGS", "MFLAGS", "MAKELEVEL"):
            env.pop(var, None)
        return subprocess.run(
            [
                "make",
                "--no-print-directory",
                "-C",
                str(REPO_ROOT),
                "venv",
                f"VENV={venv}",
                f"WORKER_LOCK={lock}",
            ],
            env=env,
            capture_output=True,
            text=True,
            timeout=300,
        )

    @unittest.skipUnless(shutil.which("python3"), "python3 not on PATH")
    def test_half_built_venv_is_removed_when_pip_fails(self):
        with tempfile.TemporaryDirectory() as d:
            lock = Path(d) / "bogus.lock"
            lock.write_text(self.BOGUS_PIN, encoding="utf-8")
            venv = Path(d) / "venv"
            r = self._make_venv(venv, lock)
            self.assertNotEqual(r.returncode, 0, r.stdout + r.stderr)
            self.assertFalse(
                venv.exists(),
                "make venv left a half-built venv behind after pip failed:\n"
                + r.stdout
                + r.stderr,
            )

    def test_working_venv_is_kept_when_pip_fails(self):
        with tempfile.TemporaryDirectory() as d:
            lock = Path(d) / "bogus.lock"
            lock.write_text(self.BOGUS_PIN, encoding="utf-8")
            venv = Path(d) / "venv"
            # --without-pip: fast, and `python -m pip` then fails, which is
            # exactly the failed-install path under test.
            subprocess.run(
                [sys.executable, "-m", "venv", "--without-pip", str(venv)],
                check=True,
                capture_output=True,
                timeout=120,
            )
            vpy = venv / "bin" / "python"
            purelib = subprocess.run(
                [str(vpy), "-c", "import sysconfig; print(sysconfig.get_paths()['purelib'])"],
                check=True,
                capture_output=True,
                text=True,
                timeout=60,
            ).stdout.strip()
            # A stand-in module: the venv now counts as a working worker venv.
            Path(purelib, "claude_agent_sdk.py").write_text("", encoding="utf-8")
            r = self._make_venv(venv, lock)
            self.assertNotEqual(r.returncode, 0, r.stdout + r.stderr)
            self.assertTrue(
                vpy.exists(),
                "make venv deleted a working venv over one failed pip install:\n"
                + r.stdout
                + r.stderr,
            )


if __name__ == "__main__":
    unittest.main()
