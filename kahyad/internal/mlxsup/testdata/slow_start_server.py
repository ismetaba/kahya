#!/usr/bin/env python3
"""slow_start_server.py - supervisor_test.go fixture: sleeps <argv[2]>
seconds (simulating a slow first model load) before binding
127.0.0.1:<argv[1]> and answering GET /health with {"status":"ok"} -
exercises EnsureRunning's poll loop actually waiting across more than one
Config.PollInterval tick, rather than the trivial immediate-answer case
healthy_server.py covers.
"""
import http.server
import json
import sys
import time


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
        pass


def main():
    port = int(sys.argv[1])
    delay = float(sys.argv[2])
    time.sleep(delay)
    http.server.HTTPServer(("127.0.0.1", port), Handler).serve_forever()


if __name__ == "__main__":
    main()
