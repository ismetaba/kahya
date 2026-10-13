"""Smoke test for the worker toolchain: SDK importable, lock discipline intact."""

import fcntl
import os
import re
import shlex
import shutil
import subprocess
import sys
import tempfile
import time
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
@unittest.skipUnless(shutil.which("python3"), "python3 not on PATH")
class TestMakeVenvTarget(unittest.TestCase):
    """G12-N3-E1: `make venv` must never leave a half-built venv behind,
    and its cleanup (an rm -rf on an overridable VENV path) must never
    remove anything else.

    The spawn/e2e Go tests only probe for worker/.venv/bin/python, so a
    venv whose build failed (python3 -m venv ok, claude_agent_sdk never
    installed) would turn them red against a worker that cannot even
    import its SDK. But the cleanup is fenced (Makefile, venv target):
    only a directory `make venv` itself created (VENV_MARKER inside it),
    never through a symlink, never while claude_agent_sdk still imports
    (a working venv survives a failed re-install), VENV must be one plain
    path, and overlapping runs are serialized by a flock on the Makefile.
    Every fence is pinned below.

    Hermetic: PIP_NO_INDEX=1 plus a lock naming a package that cannot
    exist, so pip fails without ever touching the network; VENV and
    WORKER_LOCK point into a temp dir, never at the real worker/.venv.
    Every case also checks make's own echo of the path it worked on, so a
    Makefile that ignored the VENV override (and silently operated on
    worker/.venv) cannot pass vacuously.
    """

    BOGUS_PIN = "kahya-nonexistent-package-for-make-venv-test==0.0.0\n"
    # Must match VENV_MARKER in the Makefile.
    MARKER = ".kahya-make-venv"

    def _env(self, path_prefix=None):
        env = dict(os.environ, PIP_NO_INDEX="1", PIP_DISABLE_PIP_VERSION_CHECK="1")
        # Isolate from a parent `make test` (jobserver fds, -k/-s flags),
        # and never inherit the lock-held flag of an enclosing make venv.
        for var in ("MAKEFLAGS", "MFLAGS", "MAKELEVEL", "KAHYA_VENV_LOCKED"):
            env.pop(var, None)
        if path_prefix is not None:
            env["PATH"] = str(path_prefix) + os.pathsep + env.get("PATH", "")
        return env

    def _argv(self, venv, lock, target="venv"):
        return [
            "make",
            "--no-print-directory",
            "-C",
            str(REPO_ROOT),
            target,
            f"VENV={venv}",
            f"WORKER_LOCK={lock}",
        ]

    def _make_venv(self, venv, lock, target="venv", path_prefix=None):
        return subprocess.run(
            self._argv(venv, lock, target),
            env=self._env(path_prefix),
            capture_output=True,
            text=True,
            timeout=300,
        )

    def _bogus_lock(self, d) -> Path:
        lock = Path(d) / "bogus.lock"
        lock.write_text(self.BOGUS_PIN, encoding="utf-8")
        return lock

    def _assert_pip_ran_on(self, r, venv):
        self.assertIn(
            f"{venv}/bin/python -m pip install",
            r.stdout,
            "make venv never ran its pip step on the VENV it was given:\n"
            + r.stdout
            + r.stderr,
        )

    def _bare_venv(self, venv: Path, *, marker: bool, sdk: bool) -> Path:
        """A fast venv without pip, so `python -m pip` fails: exactly the
        failed-install path. marker=True makes it a venv `make venv` built;
        sdk=True drops in a stand-in claude_agent_sdk (a WORKING venv)."""
        subprocess.run(
            [sys.executable, "-m", "venv", "--without-pip", str(venv)],
            check=True,
            capture_output=True,
            timeout=120,
        )
        vpy = venv / "bin" / "python"
        if marker:
            (venv / self.MARKER).write_text("", encoding="utf-8")
        if sdk:
            purelib = subprocess.run(
                [str(vpy), "-c", "import sysconfig; print(sysconfig.get_paths()['purelib'])"],
                check=True,
                capture_output=True,
                text=True,
                timeout=60,
            ).stdout.strip()
            Path(purelib, "claude_agent_sdk.py").write_text("", encoding="utf-8")
        return vpy

    def test_half_built_venv_is_removed_when_pip_fails(self):
        with tempfile.TemporaryDirectory() as d:
            lock = self._bogus_lock(d)
            venv = Path(d) / "venv"
            r = self._make_venv(venv, lock)
            self.assertNotEqual(r.returncode, 0, r.stdout + r.stderr)
            self.assertIn(f"python3 -m venv {venv}", r.stdout, r.stdout + r.stderr)
            self._assert_pip_ran_on(r, venv)
            self.assertFalse(
                venv.exists(),
                "make venv left a half-built venv behind after pip failed:\n"
                + r.stdout
                + r.stderr,
            )

    def test_stale_half_built_venv_from_make_venv_is_removed(self):
        # A venv an earlier (interrupted) `make venv` built, SDK never
        # installed: the next failed run cleans it up.
        with tempfile.TemporaryDirectory() as d:
            lock = self._bogus_lock(d)
            venv = Path(d) / "venv"
            self._bare_venv(venv, marker=True, sdk=False)
            r = self._make_venv(venv, lock)
            self.assertNotEqual(r.returncode, 0, r.stdout + r.stderr)
            self._assert_pip_ran_on(r, venv)
            self.assertFalse(
                venv.exists(),
                "make venv kept its own half-built venv after pip failed:\n"
                + r.stdout
                + r.stderr,
            )

    def test_working_venv_is_kept_when_pip_fails(self):
        with tempfile.TemporaryDirectory() as d:
            lock = self._bogus_lock(d)
            venv = Path(d) / "venv"
            vpy = self._bare_venv(venv, marker=True, sdk=True)
            r = self._make_venv(venv, lock)
            self.assertNotEqual(r.returncode, 0, r.stdout + r.stderr)
            self._assert_pip_ran_on(r, venv)
            self.assertTrue(
                vpy.exists(),
                "make venv deleted a working venv over one failed pip install:\n"
                + r.stdout
                + r.stderr,
            )

    def test_non_venv_directory_survives_failed_make_venv(self):
        # The review repro: VENV pointed at a directory of user data.
        with tempfile.TemporaryDirectory() as d:
            lock = self._bogus_lock(d)
            venv = Path(d) / "precious"
            note = venv / "notes" / "a.txt"
            note.parent.mkdir(parents=True)
            note.write_text("important\n", encoding="utf-8")
            r = self._make_venv(venv, lock)
            self.assertNotEqual(r.returncode, 0, r.stdout + r.stderr)
            self._assert_pip_ran_on(r, venv)
            self.assertTrue(
                note.is_file(),
                "make venv rm -rf'd a directory it did not create:\n"
                + r.stdout
                + r.stderr,
            )
            self.assertEqual(note.read_text(encoding="utf-8"), "important\n")

    def test_foreign_venv_without_sdk_survives_failed_make_venv(self):
        # Another project's venv (or one built before the marker existed):
        # not make venv's to delete, even though it lacks claude_agent_sdk.
        with tempfile.TemporaryDirectory() as d:
            lock = self._bogus_lock(d)
            venv = Path(d) / "venv"
            vpy = self._bare_venv(venv, marker=False, sdk=False)
            r = self._make_venv(venv, lock)
            self.assertNotEqual(r.returncode, 0, r.stdout + r.stderr)
            self._assert_pip_ran_on(r, venv)
            self.assertTrue(
                vpy.exists(),
                "make venv deleted a venv it did not create:\n" + r.stdout + r.stderr,
            )

    def test_symlinked_venv_is_never_removed(self):
        with tempfile.TemporaryDirectory() as d:
            lock = self._bogus_lock(d)
            real = Path(d) / "real"
            vpy = self._bare_venv(real, marker=True, sdk=False)
            link = Path(d) / "link"
            link.symlink_to(real, target_is_directory=True)
            r = self._make_venv(link, lock)
            self.assertNotEqual(r.returncode, 0, r.stdout + r.stderr)
            self._assert_pip_ran_on(r, link)
            self.assertTrue(link.is_symlink(), "make venv removed a VENV symlink:\n" + r.stdout + r.stderr)
            self.assertTrue(vpy.exists(), r.stdout + r.stderr)

    def test_unsafe_venv_path_is_refused_and_touches_nothing(self):
        # Each value would turn the unquoted rm -rf into something else
        # (two paths, a glob, a symlink-following "dir/", an option, or
        # nothing); VENV must be refused before anything runs. A fresh
        # canary per case, so one case's damage cannot mask another's.
        cases = {
            "space splits into two paths": lambda area, canary: f"{area}/x {canary}",
            "glob": lambda area, canary: f"{area}/*",
            "trailing slash": lambda area, canary: f"{canary}/",
            "leading dash": lambda area, canary: "-rf",
            "empty": lambda area, canary: "",
        }
        for name, venv_for in cases.items():
            with self.subTest(name), tempfile.TemporaryDirectory() as d:
                lock = self._bogus_lock(d)
                area = Path(d) / "area"
                canary = area / "canary"
                note = canary / "a.txt"
                note.parent.mkdir(parents=True)
                note.write_text("keep\n", encoding="utf-8")
                venv = venv_for(area, canary)
                r = self._make_venv(venv, lock)
                out = r.stdout + r.stderr
                self.assertNotEqual(r.returncode, 0, out)
                self.assertTrue(note.is_file(), f"canary removed by VENV={venv!r}:\n{out}")
                self.assertEqual(sorted(p.name for p in area.iterdir()), ["canary"], out)
                self.assertEqual(sorted(p.name for p in canary.iterdir()), ["a.txt"], out)
                self.assertIn("make venv: refusing VENV=", r.stderr, out)
                self.assertNotIn("-m venv", r.stdout, out)
                self.assertNotIn("-m pip install", r.stdout, out)

    def test_failed_venv_creation_leaves_nothing_behind(self):
        # python3 -m venv itself fails half-way (e.g. ensurepip): the
        # directory make venv just created must not survive.
        with tempfile.TemporaryDirectory() as d:
            lock = self._bogus_lock(d)
            venv = Path(d) / "venv"
            fakebin = Path(d) / "fakebin"
            fakebin.mkdir()
            real_python3 = shutil.which("python3")
            fake = fakebin / "python3"
            fake.write_text(
                "#!/bin/sh\n"
                'if [ "$1" = "-m" ] && [ "$2" = "venv" ]; then\n'
                '  mkdir -p "$3/bin" && : > "$3/pyvenv.cfg"\n'
                '  echo "fake python3: venv creation failed" >&2\n'
                "  exit 1\n"
                "fi\n"
                f'exec {shlex.quote(real_python3)} "$@"\n',
                encoding="utf-8",
            )
            fake.chmod(0o755)
            r = self._make_venv(venv, lock, path_prefix=fakebin)
            out = r.stdout + r.stderr
            self.assertNotEqual(r.returncode, 0, out)
            self.assertIn(f"python3 -m venv {venv}", r.stdout, out)
            self.assertIn("fake python3: venv creation failed", r.stderr, out)
            self.assertFalse(
                venv.exists(),
                "make venv left a half-created venv behind after python3 -m venv failed:\n"
                + out,
            )

    def test_overlapping_runs_are_serialized_by_the_makefile_lock(self):
        # While another `make venv` holds the lock, a second run must not
        # touch its VENV at all (its probe could otherwise see the first
        # run's mid-reinstall state and rm -rf a good venv).
        with tempfile.TemporaryDirectory() as d:
            lock = self._bogus_lock(d)
            venv = Path(d) / "venv"
            held = open(REPO_ROOT / "Makefile", "rb")
            ran_early = None
            try:
                fcntl.flock(held.fileno(), fcntl.LOCK_EX)
                proc = subprocess.Popen(
                    self._argv(venv, lock),
                    env=self._env(),
                    stdout=subprocess.PIPE,
                    stderr=subprocess.PIPE,
                    text=True,
                )
                try:
                    deadline = time.monotonic() + 2.0
                    while time.monotonic() < deadline:
                        if venv.exists():
                            ran_early = "created VENV"
                            break
                        if proc.poll() is not None:
                            ran_early = "finished"
                            break
                        time.sleep(0.05)
                finally:
                    held.close()  # releases the lock
                    out, err = proc.communicate(timeout=300)
            finally:
                held.close()
            self.assertIsNone(
                ran_early,
                f"make venv {ran_early} while another run held the Makefile lock:\n"
                + out
                + err,
            )
            # Once the lock is released it runs (and cleans up) normally.
            self.assertNotEqual(proc.returncode, 0, out + err)
            self.assertIn(f"python3 -m venv {venv}", out, out + err)
            self.assertFalse(venv.exists(), out + err)

    def test_venv_locked_refuses_to_run_outside_the_lock(self):
        with tempfile.TemporaryDirectory() as d:
            lock = self._bogus_lock(d)
            venv = Path(d) / "venv"
            r = self._make_venv(venv, lock, target="venv-locked")
            self.assertNotEqual(r.returncode, 0, r.stdout + r.stderr)
            self.assertIn("internal target, run make venv", r.stderr, r.stdout + r.stderr)
            self.assertFalse(venv.exists(), r.stdout + r.stderr)


if __name__ == "__main__":
    unittest.main()
