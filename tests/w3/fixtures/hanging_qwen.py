#!/usr/bin/env python3
"""hanging_qwen.py - W3-10 Gate 5's ordering-invariant fixture: a minimal
stand-in for mlx_lm.server (kahyad/internal/mlx.Supervisor spawns this
exact argv shape: "--model <path> --host <h> --port <p>" appended by
kahyad/main.go) that answers GET /v1/models fast (so the supervisor's own
health poll - kahyad/internal/mlx.Supervisor's HealthURL - reports healthy
quickly, the SAME "warm" state a real loaded model would reach) but NEVER
responds to POST /v1/chat/completions within any bound this test cares
about - simulating "classification is still in flight" so the gate test
can assert zero bytes ever reached the mock Anthropic upstream while a
classification call was genuinely pending (HANDOFF S4 flag ordering
invariant: "Hicbir bayt, gizli-serit siniflandirmasi yerel/deterministik
olarak tamamlanmadan bulut modele gitmez").

Threaded stdlib-only HTTP server: /v1/models must keep answering promptly
even while a /v1/chat/completions request is parked mid-handler.
"""
import argparse
import http.server
import json
import socketserver
import time


class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):  # noqa: A003 - stdlib override
        pass  # keep stdout/stderr quiet; the Go test does not need this noise

    def do_GET(self):
        if self.path.startswith("/v1/models"):
            body = json.dumps({"object": "list", "data": [{"id": "hanging-qwen-fixture"}]}).encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        self.send_response(404)
        self.end_headers()

    def do_POST(self):
        if self.path.startswith("/v1/chat/completions"):
            # Deliberately hang well past this test's own client-side
            # timeout - the point is that this response is NEVER observed
            # by the caller within the test's own bounded wait.
            time.sleep(30)
            body = json.dumps({"choices": [{"message": {"role": "assistant", "content": "{}"}}]}).encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        self.send_response(404)
        self.end_headers()


class ThreadingHTTPServer(socketserver.ThreadingMixIn, http.server.HTTPServer):
    daemon_threads = True


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--model")
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, required=True)
    args, _ = parser.parse_known_args()

    srv = ThreadingHTTPServer((args.host, args.port), Handler)
    srv.serve_forever()


if __name__ == "__main__":
    main()
