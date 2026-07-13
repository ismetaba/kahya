#!/usr/bin/env python3
"""stt_or_echo_worker.py - W6-02 kahyad/internal/server test fixture: a
FAKE worker (never real mlx-whisper/claude-agent-sdk) reused for BOTH
spawns a POST /v1/task audio request makes - kahyad/internal/server's own
transcribeAudioLocally (mode="stt") AND, when transcription succeeds, the
ordinary "chat" spawn built from the resulting transcript.

mode="stt": looks up a FIXED transcript by input_audio_path's basename
(the test picks the basename to select behavior - this script never
actually reads/transcribes a real wav) and reports it as one "delta" line
+ terminal "result" - the exact stdout protocol kahyad/internal/spawn.Run
already parses for every other mode (kahyad/internal/reader.
WorkerCloudModel's own reader-mode precedent - see kahyad/internal/server/
stt.go's doc comment).

Any other mode (ordinary chat): echoes the raw envelope JSON back as one
"delta" line (byte-exact - matches echo_worker.py's own convention in this
directory) so the test can assert exactly what prompt/lane/category ended
up on THIS second, real spawn's envelope.
"""
import json
import os
import sys

# basename -> fixed fake transcript, keyed by input_audio_path's basename
# (the wav never actually exists on disk for this fake - only its name
# matters). "IBAN'ım ..." is the exact string kahyad/internal/secretlane's
# own classifier_test.go already proves triggers secret-lane finans.
TRANSCRIPTS = {
    "ordinary.wav": "yarın dokuzda toplantım var",
    "finance.wav": "IBAN'ım TR330006100519786457841326",
    "empty.wav": "   ",
}


def emit(obj):
    sys.stdout.write(json.dumps(obj, ensure_ascii=False) + "\n")
    sys.stdout.flush()


def main():
    raw = sys.stdin.buffer.read().decode("utf-8")
    env = json.loads(raw)

    if env.get("mode") == "stt":
        base = os.path.basename(env.get("input_audio_path", ""))
        text = TRANSCRIPTS.get(base, "FAKE-TRANSCRIPT:" + base)
        if not text.strip():
            # Mirrors worker/kahya_worker/__main__.py's own _run_stt_only
            # empty-transcript branch (MSG_EMPTY_TRANSCRIPT) - proves
            # kahyad/internal/server.transcribeAudioLocally propagates
            # ANY worker-signaled STT failure verbatim and never proceeds
            # to spawn the second (chat) worker.
            emit({"type": "error", "message": "Ses anlaşılamadı — lütfen tekrar deneyin"})
            return
        emit({"type": "delta", "text": text})
        emit({"type": "result", "status": "ok"})
        return

    emit({"type": "delta", "text": raw})
    emit({"type": "result", "status": "ok"})


if __name__ == "__main__":
    main()
