#!/usr/bin/env python3
"""healthy_server.py - supervisor_test.go fixture: binds 127.0.0.1:<argv[1]>
and answers GET /health with {"status":"ok"} immediately. Runs until
killed - the "happy path" child the real mlx/embed/server.py stands in for
in these Go-side tests (no MLX/model dependency at all).
"""
import http.server
import json
import sys


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/health":
            body = json.dumps({"status": "ok"}).encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        self.send_response(404)
        self.end_headers()

    def log_message(self, *args):
        pass  # keep test output quiet


def main():
    port = int(sys.argv[1])
    http.server.HTTPServer(("127.0.0.1", port), Handler).serve_forever()


if __name__ == "__main__":
    main()
