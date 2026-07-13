"""stt.py — thin ``mlx-whisper`` wrapper (W6-02, ``docs/ipc.md``/HANDOFF §4
stack row: ``whisper-large-v3-turbo`` (mlx-whisper, ``language=tr`` sabit),
push-to-talk``).

HANDOFF §4 IPC ⚑ locks the process model: "``mlx-whisper`` bir sunucu
değil, worker içinde **kütüphane** olarak" — this module is called
in-process by the worker (see ``kahya_worker.__main__``'s ``mode="stt"``
branch), never run as a standalone server and never invoked from kahyad
(Go) directly.

Two invariants this file is the sole source of truth for:

- ``language="tr"`` is a FIXED LITERAL below, in the one ``transcribe()``
  call — never a parameter, never read from the envelope or any config.
  ``grep -n 'language="tr"' worker/kahya_worker/stt.py`` is this file's
  own regression proof (tasks/w6-voice/W6-02's acceptance criterion).
- The model is resolved with ``huggingface_hub.snapshot_download(...,
  local_files_only=True)`` — a cache miss raises immediately (fail-closed:
  never a network download at task time). ``KAHYA_WHISPER_MODEL_DIR``
  overrides the whole resolution (tests / a pre-resolved model dir), so a
  test can point this at a fixture directory without ever touching the
  Hugging Face cache or the network.
"""

from __future__ import annotations

import os

from huggingface_hub import snapshot_download

# HANDOFF §9 models: the ONE STT model this repo ever uses (W0-03 already
# downloaded it into the local Hugging Face cache).
MODEL_REPO = "mlx-community/whisper-large-v3-turbo"

# Fail-closed Turkish error, byte-exact (tasks/w6-voice/W6-02-ptt-whisper.md
# step 2 / tasks/README.md's byte-exact-fixture rule): emitted whenever the
# model is not already present in the local Hugging Face cache. NEVER
# triggers a network download in response — see resolve_model's own doc
# comment.
MSG_MODEL_MISSING = "STT modeli indirilmemiş (W0-03) — ağdan indirme yapılmadı"


class SttModelMissingError(Exception):
    """Raised by ``resolve_model()`` when ``whisper-large-v3-turbo`` is not
    already present in the local Hugging Face cache (or at
    ``KAHYA_WHISPER_MODEL_DIR``, when that override is set). The caller
    (``kahya_worker.__main__``'s ``mode="stt"`` branch) reports this as
    ``MSG_MODEL_MISSING`` verbatim — it never attempts a network download
    in response; this is the fail-closed path HANDOFF §4/§5 require."""


def resolve_model() -> str:
    """Resolves the local snapshot directory for ``whisper-large-v3-turbo``,
    fail-closed: ``local_files_only=True`` means ``huggingface_hub`` never
    makes a network request here — a cache miss raises immediately (no
    files are ever written, no download is ever attempted) instead of
    downloading. ``W0-03`` already populated the cache; this call only
    ever reads what it left behind.

    ``KAHYA_WHISPER_MODEL_DIR`` overrides the whole resolution when set
    (used by tests, and by anyone who has already resolved the snapshot
    dir themselves) — no ``huggingface_hub`` call happens at all in that
    case, so a test can run under ``HF_HUB_OFFLINE=1`` pointed at a
    fixture directory with no real cache involved.
    """
    override = os.environ.get("KAHYA_WHISPER_MODEL_DIR", "").strip()
    if override:
        return override
    try:
        return snapshot_download(MODEL_REPO, local_files_only=True)
    except Exception as e:  # huggingface_hub raises its own (Local)EntryNotFoundError/OSError subtypes here
        raise SttModelMissingError(MSG_MODEL_MISSING) from e


def transcribe(path: str) -> str:
    """Transcribes the wav at ``path`` entirely on-device via
    ``mlx_whisper.transcribe``, returning the stripped transcript text.

    ``language="tr"`` is a FIXED LITERAL in the call below (HANDOFF §4
    stack row: "``language=tr`` sabit") — never a parameter of this
    function, never sourced from the envelope or any config schema. This
    is the one and only place this repo ever calls ``mlx_whisper``.
    """
    import mlx_whisper  # imported lazily: a deployment that never handles
    # voice input (mode != "stt") must not need mlx-whisper installed at
    # all to run an ordinary chat task — see kahya_worker.__main__'s own
    # mode=="stt" dispatch, which is the only call site that imports this
    # module.

    model_dir = resolve_model()
    result = mlx_whisper.transcribe(path, path_or_hf_repo=model_dir, language="tr")
    return result["text"].strip()
