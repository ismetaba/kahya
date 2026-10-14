#!/usr/bin/env python3
"""fake_qwen_server.py - supervisor_test.go/adapters_test.go fixture: binds
127.0.0.1:<argv[1]> and answers GET /v1/models with an OpenAI-compatible
`{"object":"list","data":[...]}` body - the exact shape the real
mlx_lm.server answers, standing in for it here (no MLX/model dependency at
all) so this package's OWN spawn/health/idle-unload/adapter tests never
need the real ~17GB Qwen3-30B-A3B download to run. Also answers POST
/chat/completions with a canned "secret_lane: false" classification JSON
(adapters_test.go's end-to-end Supervisor+QwenClassifierAdapter path).
An optional argv[2] delays every chat completion by that many milliseconds
(FIX-P-4: a warm classification at the slow end of the live-measured
300-900ms range, so a test can prove which warm budget the adapter applies).
"""
import http.server
import json
import sys
import time

CLASSIFY_DELAY_S = 0.0


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/v1/models":
            body = json.dumps(
                {"object": "list", "data": [{"id": "qwen3-30b-a3b", "object": "model"}]}
            ).encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        self.send_response(404)
        self.end_headers()

    def do_POST(self):
        if self.path == "/v1/chat/completions":
            if CLASSIFY_DELAY_S > 0:
                time.sleep(CLASSIFY_DELAY_S)
            content = json.dumps({"secret_lane": False, "category": "none"})
            body = json.dumps(
                {"choices": [{"message": {"role": "assistant", "content": content}}]}
            ).encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        self.send_response(404)
        self.end_headers()

    def log_message(self, *args):
        pass


def main():
    global CLASSIFY_DELAY_S
    port = int(sys.argv[1])
    if len(sys.argv) > 2:
        CLASSIFY_DELAY_S = int(sys.argv[2]) / 1000.0
    http.server.HTTPServer(("127.0.0.1", port), Handler).serve_forever()


if __name__ == "__main__":
    main()
