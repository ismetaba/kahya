#!/usr/bin/env python3
"""models_list_server.py - supervisor_test.go fixture: binds
127.0.0.1:<argv[1]> and answers GET /v1/models with an OpenAI-compatible
`{"object":"list","data":[...]}` body - NO "status" field at all. Stands in
for the real mlx_lm.server's own `/v1/models` endpoint (W3-08's Qwen
secret-lane server has no bespoke `/health` the way mlx/embed/server.py
does), exercising pingHealth's "no status field present -> healthy" branch
(see supervisor.go's own doc comment on pingHealth).
"""
import http.server
import json
import sys


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

    def log_message(self, *args):
        pass


def main():
    port = int(sys.argv[1])
    http.server.HTTPServer(("127.0.0.1", port), Handler).serve_forever()


if __name__ == "__main__":
    main()
