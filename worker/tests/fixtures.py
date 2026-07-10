"""Shared test fixture: a minimal HTTP/1.0 server bound to an AF_UNIX
socket, used to exercise `kahya_worker.udshttp` (and, through it,
`kahya_worker.hooks`) against a real socket instead of a mock - per the
W12-09 task spec's "udshttp against a socketserver AF_UNIX fixture".

HTTP/1.0 (rather than the default HTTP/1.1 keep-alive) is deliberate: it
makes each handled request close its own connection, so a single-threaded
serve loop can't get stuck mid-`shutdown()` waiting on a client that
never sends a second request on the same connection.
"""

from __future__ import annotations

import http.server
import os
import socketserver
import threading
from typing import Callable


class _UnixHTTPServer(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
    daemon_threads = True
    allow_reuse_address = True

    def server_bind(self) -> None:
        socketserver.UnixStreamServer.server_bind(self)
        # http.server.BaseHTTPRequestHandler doesn't strictly require
        # these, but setting them avoids relying on TCPServer's own
        # server_bind (which assumes a (host, port) sockname - AF_UNIX's
        # sockname is a bare path string, not a 2-tuple).
        self.server_name = "localhost"
        self.server_port = 0


def _make_handler(respond: Callable[[http.server.BaseHTTPRequestHandler], None]):
    class Handler(http.server.BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.0"

        def log_message(self, *_args: object) -> None:  # silence default stderr logging
            pass

        def do_POST(self) -> None:  # noqa: N802 - http.server's naming convention
            length = int(self.headers.get("Content-Length", 0))
            self.rfile.read(length)
            respond(self)

    return Handler


class UnixHTTPFixture:
    """Context manager: starts a background-threaded HTTP server bound to
    a fresh AF_UNIX socket under `tmp_dir`, dispatching every POST to
    `respond(handler)`. `respond` is responsible for writing a full
    response (status + headers + body) via the handler it's given.

    Usage::

        with UnixHTTPFixture(tmp_dir, respond) as socket_path:
            ...
    """

    def __init__(self, tmp_dir: str, respond: Callable[[http.server.BaseHTTPRequestHandler], None]):
        self._tmp_dir = tmp_dir
        self._respond = respond
        self.socket_path: str = ""
        self._server: _UnixHTTPServer | None = None
        self._thread: threading.Thread | None = None

    def __enter__(self) -> str:
        self.socket_path = os.path.join(self._tmp_dir, "fixture.sock")
        handler_cls = _make_handler(self._respond)
        self._server = _UnixHTTPServer(self.socket_path, handler_cls)
        self._thread = threading.Thread(target=self._server.serve_forever, daemon=True)
        self._thread.start()
        return self.socket_path

    def __exit__(self, *exc_info: object) -> None:
        assert self._server is not None
        self._server.shutdown()
        self._server.server_close()
        if self._thread is not None:
            self._thread.join(timeout=5)


def respond_json(status: int, body: bytes) -> Callable[[http.server.BaseHTTPRequestHandler], None]:
    """Returns a `respond` callback that answers every request with a
    fixed status code and body (Content-Type: application/json)."""

    def respond(handler: http.server.BaseHTTPRequestHandler) -> None:
        handler.send_response(status)
        handler.send_header("Content-Type", "application/json")
        handler.send_header("Content-Length", str(len(body)))
        handler.end_headers()
        handler.wfile.write(body)

    return respond


def respond_hang(delay_s: float) -> Callable[[http.server.BaseHTTPRequestHandler], None]:
    """Returns a `respond` callback that sleeps past the client's own
    timeout without ever writing a response - simulating a hung/slow
    kahyad so the caller's own socket timeout fires."""
    import time

    def respond(_handler: http.server.BaseHTTPRequestHandler) -> None:
        time.sleep(delay_s)

    return respond
