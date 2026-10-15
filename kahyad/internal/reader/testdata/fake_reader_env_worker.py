#!/usr/bin/env python3
"""fake_reader_env_worker.py - cloud_model_test.go fixture: reports (on
STDERR) the forward-proxy environment kahyad/internal/spawn gave the worker
(base URL, whether the per-task token travels in ANTHROPIC_CUSTOM_HEADERS,
whether ANTHROPIC_API_KEY is set) and the envelope's task id, then answers
with an empty fact_extract_v1 object. Never prints a credential value
beyond comparing it with the expected token the test passes in
KAHYA_TEST_EXPECT_TOKEN. NOT the real worker - no SDK, no network.
"""
import json
import os
import sys


def emit(obj):
    sys.stdout.write(json.dumps(obj, ensure_ascii=False) + "\n")
    sys.stdout.flush()


def main():
    envelope = json.loads(sys.stdin.buffer.read().decode("utf-8"))
    headers = os.environ.get("ANTHROPIC_CUSTOM_HEADERS", "")
    expect = os.environ.get("KAHYA_TEST_EXPECT_TOKEN", "")
    sys.stderr.write(
        "base_url=%s task_id=%s token_header=%s api_key_set=%s\n"
        % (
            os.environ.get("ANTHROPIC_BASE_URL", ""),
            envelope.get("task_id", ""),
            "yes" if expect and headers == "X-Kahya-Task-Token: " + expect else "no",
            "yes" if os.environ.get("ANTHROPIC_API_KEY") else "no",
        )
    )
    sys.stderr.flush()
    emit({"type": "delta", "text": '{"entities":[],"facts":[]}'})
    emit({"type": "result", "status": "ok"})


if __name__ == "__main__":
    main()
