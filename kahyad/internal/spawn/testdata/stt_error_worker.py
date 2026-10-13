#!/usr/bin/env python3
"""stt_error_worker.py - W6-02 kahyad/internal/server test fixture: a FAKE
mode="stt" worker that always reports the byte-exact "STT modeli ...
yerelde bulunamadı" fail-closed error (worker/kahya_worker/stt.py's own
MSG_MODEL_MISSING - keep this literal byte-identical to it), so kahyad/internal/server's transcribeAudioLocally
(stt.go) can be tested against a failing STT phase without a real
mlx-whisper/model dependency. Any other mode is never exercised by the
tests that use this fixture.
"""
import json
import sys


def main():
    raw = sys.stdin.buffer.read().decode("utf-8")
    json.loads(raw)  # envelope must at least be well-formed JSON
    sys.stdout.write(
        json.dumps(
            {"type": "error", "message": "STT modeli (whisper-large-v3-turbo) yerelde bulunamadı — ağdan indirme yapılmadı"},
            ensure_ascii=False,
        )
        + "\n"
    )
    sys.stdout.flush()


if __name__ == "__main__":
    main()
