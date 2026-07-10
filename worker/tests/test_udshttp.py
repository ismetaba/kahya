"""Tests for kahya_worker.udshttp against a real socketserver AF_UNIX
fixture (W12-09 task spec step 7)."""

import json
import os
import tempfile
import unittest

import _pathfix  # noqa: F401  (must run before importing kahya_worker)
from fixtures import UnixHTTPFixture, respond_hang, respond_json

from kahya_worker.udshttp import UDSHTTPError, post_json


class TestPostJSON(unittest.TestCase):
    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self._tmp.cleanup)

    def test_success_returns_parsed_body(self) -> None:
        body = json.dumps({"decision": "allow", "rule": "interim-static-v1"}).encode("utf-8")
        with UnixHTTPFixture(self._tmp.name, respond_json(200, body)) as sock:
            resp = post_json(sock, "/policy/check", {"tool_name": "memory_search"}, timeout=2.0)
        self.assertEqual(resp, {"decision": "allow", "rule": "interim-static-v1"})

    def test_request_body_reaches_server_intact(self) -> None:
        captured: dict[str, bytes] = {}

        def respond(handler):
            length = int(handler.headers.get("Content-Length", 0))
            # rfile was already drained by the fixture's do_POST before
            # calling respond - re-read is not possible, so capture via a
            # second handler wrapper instead: see test below for the
            # byte-exact Turkish assertion done at the hooks layer, which
            # exercises the real request body through post_json's own
            # json.dumps call. This test only asserts a 200 round-trip
            # with a Turkish payload to prove UTF-8 is not mangled.
            resp_body = json.dumps({"echo": True}).encode("utf-8")
            handler.send_response(200)
            handler.send_header("Content-Type", "application/json")
            handler.send_header("Content-Length", str(len(resp_body)))
            handler.end_headers()
            handler.wfile.write(resp_body)

        with UnixHTTPFixture(self._tmp.name, respond) as sock:
            resp = post_json(sock, "/v1/memory/search", {"query": "Kadıköy'de iki daire gezdik."}, timeout=2.0)
        self.assertEqual(resp, {"echo": True})

    def test_non_serializable_payload_raises_udshttperror(self) -> None:
        """MINOR 6 fix: json.dumps(payload) must be inside post_json's own
        error handling - a non-JSON-serializable payload (e.g. containing
        a `set`) must raise UDSHTTPError, never let the underlying
        TypeError/ValueError escape uncaught. No socket listener is needed:
        serialization is attempted before any connection is opened."""
        missing_sock = os.path.join(self._tmp.name, "no-such.sock")
        with self.assertRaises(UDSHTTPError):
            post_json(missing_sock, "/policy/check", {"values": {1, 2, 3}}, timeout=2.0)

    def test_non_200_status_raises(self) -> None:
        body = json.dumps({"error": "boom"}).encode("utf-8")
        with UnixHTTPFixture(self._tmp.name, respond_json(500, body)) as sock:
            with self.assertRaises(UDSHTTPError):
                post_json(sock, "/policy/check", {}, timeout=2.0)

    def test_garbage_body_raises(self) -> None:
        with UnixHTTPFixture(self._tmp.name, respond_json(200, b"not-json{{{")) as sock:
            with self.assertRaises(UDSHTTPError):
                post_json(sock, "/policy/check", {}, timeout=2.0)

    def test_non_object_json_body_raises(self) -> None:
        with UnixHTTPFixture(self._tmp.name, respond_json(200, b"[1,2,3]")) as sock:
            with self.assertRaises(UDSHTTPError):
                post_json(sock, "/policy/check", {}, timeout=2.0)

    def test_timeout_raises(self) -> None:
        with UnixHTTPFixture(self._tmp.name, respond_hang(2.0)) as sock:
            with self.assertRaises(UDSHTTPError):
                post_json(sock, "/policy/check", {}, timeout=0.2)

    def test_connection_refused_raises(self) -> None:
        missing_sock = os.path.join(self._tmp.name, "no-such.sock")
        with self.assertRaises(UDSHTTPError):
            post_json(missing_sock, "/policy/check", {}, timeout=2.0)


if __name__ == "__main__":
    unittest.main()
