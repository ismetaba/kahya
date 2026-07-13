#!/usr/bin/env python3
"""stt_local_worker.py - W6-04 gate1 (voice loop, %100 yerel) fixture: a
FAKE worker (never real mlx-whisper/claude-agent-sdk) reused for BOTH spawns
a POST /v1/task audio request makes - kahyad/internal/server's own
transcribeAudioLocally (mode="stt") AND, when transcription succeeds, the
ordinary "chat" spawn built from the resulting transcript.

mode=="stt":
  (a) records the value of env ANTHROPIC_BASE_URL (empty string if unset) to
      the file named by env KAHYA_W6_STT_ENV_FILE - the STRUCTURAL-locality
      probe: kahyad/internal/server/stt.go spawns the stt-mode worker with
      BLANK AnthropicBaseURL (no per-task forward-proxy listener is even
      opened), so this file must record an EMPTY string - proving the
      capture+transcription phase has no reachable network endpoint at all;
  (b) emits the byte-exact Turkish transcript as one "delta" line + a
      terminal "result" line (the exact stdout protocol kahyad already
      parses; the caller joins "delta" text into the transcript).

Any other mode (the ordinary chat spawn built from the transcript):
  (a) records the value of env ANTHROPIC_BASE_URL to the file named by env
      KAHYA_W6_CHAT_ENV_FILE - the POSITIVE CONTROL for the stt-locality
      probe above: the ordinary chat spawn DOES get a real per-task
      forward-proxy URL, so this file must record a NON-empty value, proving
      the empty stt-mode value is a deliberate stt-only property and not an
      artifact of no proxy ever being opened in this test;
  (b) echoes the raw envelope JSON back as one "delta" line (byte-exact) so
      the test can assert the transcript actually drove the task loop - the
      echoed envelope's "prompt" is the transcript verbatim.
"""
import json
import os
import sys

# Byte-exact Turkish transcript (tasks/README.md byte-exact-fixture rule -
# never .lower()/.casefold(), which would corrupt the dotless-i characters).
TRANSCRIPT = "yarın dokuzda toplantım var"


def emit(obj):
    sys.stdout.write(json.dumps(obj, ensure_ascii=False) + "\n")
    sys.stdout.flush()


def main():
    raw = sys.stdin.buffer.read().decode("utf-8")
    env = json.loads(raw)

    if env.get("mode") == "stt":
        env_file = os.environ.get("KAHYA_W6_STT_ENV_FILE", "")
        if env_file:
            # os.environ.get returns None when unset; record "" in that case
            # so the test's "must be EMPTY" assertion is unambiguous.
            base_url = os.environ.get("ANTHROPIC_BASE_URL", "")
            with open(env_file, "w", encoding="utf-8") as f:
                f.write(base_url)
        emit({"type": "delta", "text": TRANSCRIPT})
        emit({"type": "result", "status": "ok"})
        return

    chat_env_file = os.environ.get("KAHYA_W6_CHAT_ENV_FILE", "")
    if chat_env_file:
        with open(chat_env_file, "w", encoding="utf-8") as f:
            f.write(os.environ.get("ANTHROPIC_BASE_URL", ""))
    emit({"type": "delta", "text": raw})
    emit({"type": "result", "status": "ok"})


if __name__ == "__main__":
    main()
