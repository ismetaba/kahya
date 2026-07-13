#!/usr/bin/env python3
"""stt_error_worker.py - W6-02 kahyad/internal/server test fixture: a FAKE
mode="stt" worker that always reports the byte-exact "STT modeli
indirilmemiş" fail-closed error (worker/kahya_worker/stt.py's own
MSG_MODEL_MISSING), so kahyad/internal/server's transcribeAudioLocally
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
            {"type": "error", "message": "STT modeli indirilmemiş (W0-03) — ağdan indirme yapılmadı"},
            ensure_ascii=False,
        )
        + "\n"
    )
    sys.stdout.flush()


if __name__ == "__main__":
    main()
