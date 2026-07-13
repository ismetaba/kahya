"""worker/tests/test_w6_gate.py - W6-04 gate1 (voice loop, %100 yerel), the
OFFLINE-STT-within-the-task-loop half.

Where test_stt.py exercises kahya_worker.stt.transcribe() as a library
function directly, THIS test drives the worker's OWN task-loop entrypoint -
``python -m kahya_worker`` (kahya_worker.__main__.main -> _run_stt_only) - the
exact dispatch kahyad/internal/server/stt.go spawns for a Mode==stt task,
under HF_HUB_OFFLINE=1. It asserts the emitted stdout protocol is a
"delta"+"result" pair whose transcript is non-empty and contains the
byte-exact Turkish token "toplantı" (NO .lower()/.casefold() - Python case
folding maps I->i, never ı, silently corrupting a Turkish comparison;
tasks/README.md byte-exact-fixture rule).

GUARDED to SKIP cleanly (never fail) when mlx_whisper is not importable or
the whisper model is not in the local Hugging Face cache - so `make test`
stays green on a machine without either - reusing test_stt.py's own skip
guards and fixture path/asserted token verbatim.
"""

from __future__ import annotations

import importlib.util
import json
import os
import subprocess
import sys
import tempfile
import unittest

import _pathfix  # noqa: F401

# Reuse test_stt.py's fixture path + asserted token EXACTLY (same wav, same
# byte-exact Turkish token).
FIXTURE_PATH = os.path.join(os.path.dirname(__file__), "fixtures", "tr_toplanti.wav")
EXPECTED_TOKEN = "toplantı"

WORKER_DIR = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def _mlx_whisper_available() -> bool:
    return importlib.util.find_spec("mlx_whisper") is not None


def _model_available() -> bool:
    """True iff whisper-large-v3-turbo resolves from the local Hugging Face
    cache with no network access (imports kahya_worker.stt lazily so this
    check never itself requires mlx_whisper)."""
    from kahya_worker import stt

    try:
        stt.resolve_model()
        return True
    except stt.SttModelMissingError:
        return False


@unittest.skipUnless(_mlx_whisper_available(), "mlx_whisper not installed - skipping live STT gate")
@unittest.skipUnless(_model_available(), "whisper-large-v3-turbo not in local HF cache - skipping live STT gate")
class TestW6GateOfflineSttWithinTaskLoop(unittest.TestCase):
    def test_stt_dispatch_entrypoint_transcribes_offline(self) -> None:
        self.assertTrue(
            os.path.exists(FIXTURE_PATH),
            f"fixture missing: {FIXTURE_PATH} - run scripts/make-stt-fixture.sh",
        )

        envelope = {
            "schema_version": 1,
            "task_id": "w6-gate-stt-task",
            "trace_id": "w6-gate-stt-trace",
            "session_id": None,
            "kind": "chat",
            # placeholder prompt: _run_stt_only ignores envelope.prompt, but
            # parse_envelope still requires a non-blank one for every kind.
            "prompt": "(ses girişi transkribe ediliyor)",
            "model": "claude-opus-4-8",
            "memory_injection": False,
            "created_at": "2026-01-01T00:00:00Z",
            "mode": "stt",
            "input_audio_path": FIXTURE_PATH,
        }

        with tempfile.TemporaryDirectory() as log_dir:
            env = dict(os.environ)
            env["HF_HUB_OFFLINE"] = "1"  # prove no network download happens
            env["KAHYA_LOG_DIR"] = log_dir
            env["KAHYA_TRACE_ID"] = envelope["trace_id"]
            env["PYTHONPATH"] = WORKER_DIR + os.pathsep + env.get("PYTHONPATH", "")
            # _run_stt_only deletes the audio only when it lives under TmpDir;
            # our fixture is outside any tmp dir, so it is left in place.

            proc = subprocess.run(
                [sys.executable, "-m", "kahya_worker"],
                input=json.dumps(envelope).encode("utf-8"),
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                env=env,
                cwd=WORKER_DIR,
                timeout=300,
            )

        self.assertEqual(
            proc.returncode, 0,
            f"worker exited {proc.returncode}\nstdout={proc.stdout!r}\nstderr={proc.stderr!r}",
        )

        # Parse the stdout protocol: one delta (the transcript) + a terminal
        # result, exactly what kahyad/internal/server joins into the prompt.
        transcript = ""
        got_result = False
        for line in proc.stdout.decode("utf-8").splitlines():
            line = line.strip()
            if not line:
                continue
            obj = json.loads(line)
            if obj.get("type") == "delta":
                transcript += obj.get("text", "")
            elif obj.get("type") == "result":
                self.assertEqual(obj.get("status"), "ok", f"non-ok result: {obj}")
                got_result = True
            elif obj.get("type") == "error":
                self.fail(f"stt dispatch emitted an error line: {obj}")

        self.assertTrue(got_result, "no terminal result protocol line emitted by the stt dispatch")
        self.assertTrue(transcript.strip(), "transcript was empty/whitespace-only")
        # Byte-exact containment - NEVER .lower()/.casefold() (Turkish).
        self.assertIn(EXPECTED_TOKEN, transcript)


# Model-missing Turkish string the worker emits when the whisper model is not
# resolvable offline - byte-exact, from kahya_worker.stt.MSG_MODEL_MISSING
# (CLAUDE.md language policy; never .lower()/.casefold()).
MSG_MODEL_MISSING = "STT modeli indirilmemiş (W0-03) — ağdan indirme yapılmadı"


class TestW6GateOfflineSttFailsClosedWithoutModel(unittest.TestCase):
    """Model-FREE half of the offline-STT gate: runs on EVERY `make test`
    (no ~1.5GB whisper model needed), proving the mode==stt __main__ dispatch
    fail-closes offline instead of reaching out to the network.

    With HF_HUB_OFFLINE=1 AND KAHYA_WHISPER_MODEL_DIR pointed at an EMPTY dir,
    kahya_worker.stt.resolve_model() raises SttModelMissingError before
    mlx_whisper is ever imported or any download is attempted, and
    _run_stt_only surfaces it as a terminal {"type":"error"} line carrying the
    fixed Turkish MSG_MODEL_MISSING - never a delta, never a network call.
    This is the offline-locality assertion the acceptance criterion names,
    exercised without the model the live-transcription test above needs."""

    def test_stt_dispatch_fails_closed_offline_without_model(self) -> None:
        envelope = {
            "schema_version": 1,
            "task_id": "w6-gate-stt-missing-task",
            "trace_id": "w6-gate-stt-missing-trace",
            "session_id": None,
            "kind": "chat",
            "prompt": "(ses girişi transkribe ediliyor)",
            "model": "claude-opus-4-8",
            "memory_injection": False,
            "created_at": "2026-01-01T00:00:00Z",
            "mode": "stt",
            "input_audio_path": FIXTURE_PATH,
        }

        with tempfile.TemporaryDirectory() as log_dir, tempfile.TemporaryDirectory() as empty_model_dir:
            env = dict(os.environ)
            env["HF_HUB_OFFLINE"] = "1"
            # An EXISTING but EMPTY dir: resolve_model()'s override branch
            # fails closed (not a dir with weights) with no network access.
            env["KAHYA_WHISPER_MODEL_DIR"] = empty_model_dir
            env["KAHYA_LOG_DIR"] = log_dir
            env["KAHYA_TRACE_ID"] = envelope["trace_id"]
            env["PYTHONPATH"] = WORKER_DIR + os.pathsep + env.get("PYTHONPATH", "")

            proc = subprocess.run(
                [sys.executable, "-m", "kahya_worker"],
                input=json.dumps(envelope).encode("utf-8"),
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                env=env,
                cwd=WORKER_DIR,
                timeout=60,
            )

        # The dispatch must emit a terminal error line carrying the fixed
        # Turkish model-missing string, and NEVER a transcript delta.
        got_error_msg = ""
        saw_delta = False
        for line in proc.stdout.decode("utf-8").splitlines():
            line = line.strip()
            if not line:
                continue
            obj = json.loads(line)
            if obj.get("type") == "error":
                got_error_msg = obj.get("message", "")
            elif obj.get("type") == "delta":
                saw_delta = True

        self.assertFalse(saw_delta, "offline model-missing stt dispatch emitted a transcript delta - it must fail closed")
        self.assertIn(
            MSG_MODEL_MISSING, got_error_msg,
            f"stt dispatch did not fail closed with the model-missing Turkish string; got error={got_error_msg!r} stderr={proc.stderr!r}",
        )


if __name__ == "__main__":
    unittest.main()
