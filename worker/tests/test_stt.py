"""worker/tests/test_stt.py — W6-02 task spec step 8: offline
transcription test.

Two independent things are tested here:

1. ``TestTranscribeFixtureOffline`` — the ONE real, model-required test
   this task ships: with ``HF_HUB_OFFLINE=1`` set, transcribe the
   committed ``tr_toplanti.wav`` fixture and assert the byte-exact
   substring ``"toplantı"`` is IN the result — NO ``.lower()``/
   ``.casefold()`` (``tasks/README.md``'s byte-exact-fixture rule: Python
   case folding maps ``I``->``i``, never ``ı``, silently corrupting
   Turkish comparisons; Whisper already emits the word lowercase
   mid-sentence, so no folding is ever needed to match it). GUARDED to
   skip cleanly (never fail) when ``mlx_whisper`` is not importable or the
   model is not in the local Hugging Face cache — so ``make test`` stays
   green on a machine without either — but it actually RUNS and PASSES on
   this dev machine (``mlx-whisper`` pinned in ``worker/requirements.lock``;
   model already present per W0-03).

2. ``TestResolveModelFailsClosed`` — fully hermetic (huggingface_hub only,
   no mlx_whisper/model needed, never skipped): proves the fail-closed
   contract ``stt.resolve_model()`` relies on — ``local_files_only=True``
   never downloads/writes on a cache miss (a REAL ``snapshot_download``
   call against an empty, explicit ``cache_dir``) — and that
   ``resolve_model()`` itself converts ANY resolution failure into the
   typed, byte-exact ``SttModelMissingError``.
"""

from __future__ import annotations

import importlib.util
import os
import tempfile
import unittest
from unittest import mock

import _pathfix  # noqa: F401

FIXTURE_PATH = os.path.join(os.path.dirname(__file__), "fixtures", "tr_toplanti.wav")


def _mlx_whisper_available() -> bool:
    return importlib.util.find_spec("mlx_whisper") is not None


def _model_available() -> bool:
    """True iff whisper-large-v3-turbo resolves from the local Hugging
    Face cache with no network access. Imports kahya_worker.stt lazily so
    this check itself never requires mlx_whisper to be installed (stt.py's
    own top-level imports are huggingface_hub only, not mlx_whisper -
    mlx_whisper is imported lazily inside stt.transcribe())."""
    from kahya_worker import stt

    try:
        stt.resolve_model()
        return True
    except stt.SttModelMissingError:
        return False


def _snapshot_files(path: str) -> list[str]:
    found = []
    for root, _dirs, files in os.walk(path):
        for name in files:
            found.append(os.path.relpath(os.path.join(root, name), path))
    return sorted(found)


@unittest.skipUnless(_mlx_whisper_available(), "mlx_whisper not installed - skipping live STT test")
@unittest.skipUnless(_model_available(), "whisper-large-v3-turbo not in local HF cache - skipping live STT test")
class TestTranscribeFixtureOffline(unittest.TestCase):
    def test_transcribes_fixture_offline_and_contains_toplanti(self) -> None:
        self.assertTrue(
            os.path.exists(FIXTURE_PATH),
            f"fixture missing: {FIXTURE_PATH} - run scripts/make-stt-fixture.sh",
        )

        old_offline = os.environ.get("HF_HUB_OFFLINE")
        os.environ["HF_HUB_OFFLINE"] = "1"
        try:
            from kahya_worker import stt

            text = stt.transcribe(FIXTURE_PATH)
        finally:
            if old_offline is None:
                os.environ.pop("HF_HUB_OFFLINE", None)
            else:
                os.environ["HF_HUB_OFFLINE"] = old_offline

        # Byte-exact containment - NEVER .lower()/.casefold(): Python's
        # case folding maps I->i, never ı, which would silently corrupt a
        # Turkish comparison (tasks/README.md's byte-exact fixture rule).
        self.assertIn("toplantı", text)


class TestResolveModelFailsClosed(unittest.TestCase):
    """Hermetic - no mlx_whisper/real model needed, never skipped."""

    def test_local_files_only_never_downloads_or_writes_on_a_miss(self) -> None:
        # The REAL huggingface_hub call (no mocking) against an explicit,
        # empty cache_dir - proves the library contract resolve_model()
        # relies on: local_files_only=True raises immediately on a miss,
        # with zero network access and zero cache writes. cache_dir is
        # passed explicitly (not via HF_HOME/env) so this is immune to
        # huggingface_hub's constants being computed once at import time.
        from huggingface_hub import snapshot_download

        with tempfile.TemporaryDirectory() as cache_dir:
            before = _snapshot_files(cache_dir)
            with self.assertRaises(Exception):
                snapshot_download(
                    "mlx-community/whisper-large-v3-turbo",
                    local_files_only=True,
                    cache_dir=cache_dir,
                )
            after = _snapshot_files(cache_dir)
            self.assertEqual(before, after, "local_files_only=True must never write to the cache on a miss")
            self.assertEqual(after, [], "an empty cache_dir must stay empty - no download attempted")

    def test_resolve_model_wraps_any_resolution_failure_as_typed_error(self) -> None:
        from kahya_worker import stt

        with mock.patch.object(stt, "snapshot_download", side_effect=OSError("not cached locally")):
            with mock.patch.dict(os.environ, {}, clear=True):  # no KAHYA_WHISPER_MODEL_DIR override
                with self.assertRaises(stt.SttModelMissingError) as ctx:
                    stt.resolve_model()
                self.assertEqual(str(ctx.exception), stt.MSG_MODEL_MISSING)

    def test_kahya_whisper_model_dir_override_skips_snapshot_download(self) -> None:
        from kahya_worker import stt

        # A VALID (existing, non-empty) override dir is returned verbatim, no
        # snapshot_download call.
        with tempfile.TemporaryDirectory() as d:
            with open(os.path.join(d, "config.json"), "w") as f:
                f.write("{}")
            with mock.patch.dict(os.environ, {"KAHYA_WHISPER_MODEL_DIR": d}, clear=True):
                with mock.patch.object(stt, "snapshot_download") as fake_download:
                    self.assertEqual(stt.resolve_model(), d)
                    fake_download.assert_not_called()

    def test_kahya_whisper_model_dir_override_missing_fails_closed(self) -> None:
        """W6-02 review BLOCKER regression: a KAHYA_WHISPER_MODEL_DIR override
        pointing at a nonexistent (or empty) directory must FAIL CLOSED, never
        be handed to mlx_whisper (which would treat it as a hub repo id and
        download it)."""
        from kahya_worker import stt

        bogus = os.path.join(tempfile.gettempdir(), "kahya-nonexistent-whisper-dir-xyz")
        self.assertFalse(os.path.exists(bogus))
        with mock.patch.dict(os.environ, {"KAHYA_WHISPER_MODEL_DIR": bogus}, clear=True):
            with mock.patch.object(stt, "snapshot_download") as fake_download:
                with self.assertRaises(stt.SttModelMissingError) as ctx:
                    stt.resolve_model()
                self.assertEqual(str(ctx.exception), stt.MSG_MODEL_MISSING)
                fake_download.assert_not_called()

        # And an EXISTING but EMPTY override dir also fails closed.
        with tempfile.TemporaryDirectory() as empty:
            with mock.patch.dict(os.environ, {"KAHYA_WHISPER_MODEL_DIR": empty}, clear=True):
                with self.assertRaises(stt.SttModelMissingError):
                    stt.resolve_model()


if __name__ == "__main__":
    unittest.main()
