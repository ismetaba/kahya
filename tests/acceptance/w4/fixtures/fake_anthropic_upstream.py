#!/usr/bin/env python3
"""fake_anthropic_upstream.py - W4-07 scenario B's "network is back" fixture
for scripts/accept_w4.sh (the Go acceptance test uses an in-process
net/http.Server for the identical purpose instead - see scenario_b_test.go's
startFakeHealthyUpstream). Standalone HTTP server, standing in for the real
api.anthropic.com: answers every POST /v1/messages with a minimal 200 OK
Anthropic-shaped body, mirroring kahyad/internal/anthproxy's own W4-04 test
fixtures' "healthy responder" role.

Usage: fake_anthropic_upstream.py <port>
"""
import json
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer


class Handler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):  # noqa: A002 - quiet by default
        pass

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        self.rfile.read(length)
        body = json.dumps({
            "id": "msg_fake",
            "type": "message",
            "role": "assistant",
            "content": [{"type": "text", "text": "ok"}],
            "model": "claude-sonnet-5",
            "usage": {"input_tokens": 1, "output_tokens": 1},
        }).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


def main():
    if len(sys.argv) != 2:
        print("usage: fake_anthropic_upstream.py <port>", file=sys.stderr)
        return 2
    port = int(sys.argv[1])
    server = HTTPServer(("127.0.0.1", port), Handler)
    server.serve_forever()
    return 0


if __name__ == "__main__":
    sys.exit(main())
