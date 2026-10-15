#!/usr/bin/env python3
"""retrying_cloud_worker.py - a cloud-calling worker fixture that retries a
503 the way the real bundled claude CLI does (it retries 5xx on its own,
after kahyad's proxy already parked the task), then answers - FIX-FINAL
park-then-cli-retry-success-reruns-task."""
import json, os, sys, time, urllib.error, urllib.request

def emit(o):
    sys.stdout.write(json.dumps(o, ensure_ascii=False) + "\n"); sys.stdout.flush()

def tok():
    for line in os.environ.get("ANTHROPIC_CUSTOM_HEADERS", "").splitlines():
        n, s, v = line.partition(":")
        if s and n.strip().lower() == "x-kahya-task-token":
            return v.strip()
    return os.environ.get("ANTHROPIC_API_KEY", "")

def main():
    sys.stdin.buffer.read()
    emit({"type": "session", "session_id": "retry-session"})
    url = os.environ["ANTHROPIC_BASE_URL"] + "/v1/messages"
    body = json.dumps({"model": "claude-sonnet-5-5", "messages": []}).encode()
    for attempt in range(3):
        req = urllib.request.Request(url, data=body, method="POST", headers={
            "Content-Type": "application/json", "X-Kahya-Task-Token": tok(),
            "Authorization": "Bearer fixture-upstream-credential"})
        try:
            with urllib.request.urlopen(req, timeout=10) as r:
                st = r.getcode()
        except urllib.error.HTTPError as e:
            st = e.code
        if st == 200:
            emit({"type": "delta", "text": "cevap"})
            emit({"type": "result", "status": "ok"})
            return
        time.sleep(0.05)
    emit({"type": "error", "message": "gave up"}); sys.exit(1)

main()
