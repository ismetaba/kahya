#!/usr/bin/env python3
"""fake_reader_worker.py - cloud_model_test.go fixture: reads the envelope
JSON off stdin, writes a diagnostic line to STDERR describing
mode/schema/model/prompt_len (so the test can confirm
kahyad/internal/reader.WorkerCloudModel built the envelope it was
supposed to - via OnStderr, kept OUT of the stdout delta stream so it
never pollutes the JSON content itself), then emits a canned
mail_summary_v1 JSON object split across TWO delta lines (proving
kahyad/internal/reader.WorkerCloudModel concatenates multiple deltas
correctly, exactly like the real worker's own streamed text blocks would
arrive), then reports a normal "ok" result. NOT the real worker - this
only exercises kahyad's own envelope-building/delta-accumulation
plumbing, never claude-agent-sdk or a real network call.
"""
import json
import sys


def emit(obj):
    sys.stdout.write(json.dumps(obj, ensure_ascii=False) + "\n")
    sys.stdout.flush()


def main():
    raw = sys.stdin.buffer.read().decode("utf-8")
    envelope = json.loads(raw)

    sys.stderr.write("mode=%s schema=%s model=%s prompt_len=%d\n"
                      % (envelope.get("mode"), envelope.get("schema"), envelope.get("model"), len(envelope.get("prompt", ""))))
    sys.stderr.flush()

    canned = '{"from_display":"","subject":"","summary":"canned test summary","dates":[],"amounts":["4.250,00 TL"]}'
    half = len(canned) // 2
    emit({"type": "delta", "text": canned[:half]})
    emit({"type": "delta", "text": canned[half:]})
    emit({"type": "result", "status": "ok"})


if __name__ == "__main__":
    main()
