#!/usr/bin/env python3
"""fake_briefing_worker.py - worker_test.go fixture: reads the envelope
JSON off stdin, writes a diagnostic line to STDERR describing
mode/schema/model (so the test can confirm
kahyad/internal/briefing.ProcessSpawner built the envelope it was
supposed to), then emits a canned briefing_summary_v1 JSON object split
across TWO delta lines (proving ProcessSpawner concatenates multiple
deltas correctly, exactly like the real worker's own streamed text blocks
would arrive), then reports a normal "ok" result. NOT the real worker -
this only exercises kahyad's own envelope-building/delta-accumulation
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

    sys.stderr.write("mode=%s schema=%s model=%s\n"
                      % (envelope.get("mode"), envelope.get("schema"), envelope.get("model")))
    sys.stderr.flush()

    canned = '{"lines":["3 acik PR var.","Bugun 1 takvim etkinligi var."]}'
    half = len(canned) // 2
    emit({"type": "delta", "text": canned[:half]})
    emit({"type": "delta", "text": canned[half:]})
    emit({"type": "result", "status": "ok"})


if __name__ == "__main__":
    main()
