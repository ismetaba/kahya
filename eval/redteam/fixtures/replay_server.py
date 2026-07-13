#!/usr/bin/env python3
"""W78-02 local record-replay server (offline, deterministic).

Speaks just enough of the Anthropic Messages API shape to stand in for the
real upstream when a red-team scenario needs a worker/SDK round-trip. The dev
worker is spawned with ANTHROPIC_BASE_URL pointing here, so no scenario ever
touches the network or a real cloud key (HANDOFF §6 W7-8 ⚑ record-replay).

It serves a canned response from transcripts/<name>.json selected by the
X-Kahya-Redteam-Scenario request header (falling back to a minimal stub). The
transcripts are synthetic attacker payloads only — never real user memory.

Usage:
    python3 replay_server.py [--port 0] [--transcripts DIR]

Prints the chosen port as the first stdout line ("PORT <n>") so a harness can
read it back, then serves until terminated.
"""
import argparse
import json
import os
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

TRANSCRIPTS_DIR = os.path.join(os.path.dirname(os.path.abspath(__file__)), "transcripts")

STUB_RESPONSE = {
    "id": "msg_redteam_stub",
    "type": "message",
    "role": "assistant",
    "model": "replay-offline",
    "content": [{"type": "text", "text": "[redteam replay stub]"}],
    "stop_reason": "end_turn",
    "usage": {"input_tokens": 1, "output_tokens": 1},
}


def _load(scenario: str, transcripts_dir: str):
    if scenario:
        path = os.path.join(transcripts_dir, scenario + ".json")
        if os.path.isfile(path):
            with open(path, "r", encoding="utf-8") as f:
                return json.load(f)
    return STUB_RESPONSE


class Handler(BaseHTTPRequestHandler):
    transcripts_dir = TRANSCRIPTS_DIR

    def log_message(self, *args):  # silence default stderr access log
        pass

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0") or "0")
        _ = self.rfile.read(length)  # drain request body; replay ignores it
        scenario = self.headers.get("X-Kahya-Redteam-Scenario", "")
        body = json.dumps(_load(scenario, self.transcripts_dir)).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, default=0)
    ap.add_argument("--transcripts", default=TRANSCRIPTS_DIR)
    args = ap.parse_args()

    Handler.transcripts_dir = args.transcripts
    httpd = ThreadingHTTPServer(("127.0.0.1", args.port), Handler)
    print("PORT %d" % httpd.server_address[1], flush=True)
    try:
        httpd.serve_forever()
    except KeyboardInterrupt:
        pass
    return 0


if __name__ == "__main__":
    sys.exit(main())
