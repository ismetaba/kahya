"""Stdlib-only HTTP-over-UDS client (``docs/ipc.md`` §5/§6, HANDOFF §4 IPC
⚑): every worker→kahyad call (``POST /v1/memory/search``,
``POST /policy/check``) goes over the control socket at ``KAHYA_SOCKET``
via ``http.client.HTTPConnection`` wired to connect over an ``AF_UNIX``
socket instead of TCP - no third-party HTTP client dependency, so
``worker/requirements.lock`` stays pinned to exactly
``claude-agent-sdk`` (W0-02's lock discipline).
"""

from __future__ import annotations

import http.client
import json
import socket
from typing import Any


class UDSHTTPError(Exception):
    """Raised for ANY failure translating a UDS HTTP call: a payload that
    is not JSON-serializable, a connect/read timeout, a refused/broken
    connection, a non-200 HTTP status, or a response body that is not
    valid JSON. Every caller in this package treats every one of these
    identically - see each caller's own doc comment for whether that means
    "fail closed" (``hooks.make_can_use_tool``) or "continue without
    enrichment" (``hooks.make_user_prompt_submit_hook``)."""


class _UnixHTTPConnection(http.client.HTTPConnection):
    """An ``http.client.HTTPConnection`` whose transport is an
    ``AF_UNIX`` stream socket instead of TCP. The host name passed to the
    base class is a fixed placeholder purely for the ``Host:`` header
    HTTP/1.1 requires - it is never resolved or dialed; ``connect``
    below always dials ``socket_path`` instead."""

    def __init__(self, socket_path: str, timeout: float) -> None:
        super().__init__("kahyad", timeout=timeout)
        self._socket_path = socket_path

    def connect(self) -> None:
        sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        # self.sock is assigned BEFORE connect() is attempted (rather than
        # after it succeeds) so that a failed connect (e.g. connection
        # refused - no listener at socket_path) still leaves the socket
        # reachable from self.close() below / HTTPConnection.close() - a
        # bare `raise` here would otherwise leak the file descriptor.
        self.sock = sock
        # One timeout covers both connect() and every subsequent
        # send/recv on this socket - http.client never calls
        # settimeout again after connect() hands it a socket.
        sock.settimeout(self.timeout)
        sock.connect(self._socket_path)


def post_json(socket_path: str, path: str, payload: dict[str, Any], timeout: float) -> dict[str, Any]:
    """POSTs ``payload`` (JSON-encoded) to ``path`` over the ``AF_UNIX``
    socket at ``socket_path``, with the given timeout budget, and returns
    the parsed JSON response body. Raises `UDSHTTPError` on any failure:
    ``payload`` not JSON-serializable, connect/read timeout, connection
    refused/reset, an HTTP status other than 200, or a response body that
    does not parse as JSON."""
    try:
        # MINOR 6 fix: json.dumps must be inside the same error handling as
        # the request itself - a non-JSON-serializable tool_input (e.g. a
        # `set`) previously raised an uncaught TypeError here, escaping
        # every caller's `except UDSHTTPError` fail-closed handling
        # (hooks.make_can_use_tool) instead of becoming one more reason to
        # deny.
        body = json.dumps(payload).encode("utf-8")
    except (TypeError, ValueError) as e:
        raise UDSHTTPError(f"payload is not JSON-serializable: {e}") from e

    conn = _UnixHTTPConnection(socket_path, timeout)
    try:
        try:
            conn.request(
                "POST",
                path,
                body=body,
                headers={
                    "Content-Type": "application/json",
                    "Content-Length": str(len(body)),
                },
            )
            resp = conn.getresponse()
            raw = resp.read()
        except OSError as e:
            # Covers connection-refused, connect/read timeouts (socket
            # timeouts subclass OSError), and a missing socket file.
            raise UDSHTTPError(f"uds request failed: {e}") from e
        except http.client.HTTPException as e:
            raise UDSHTTPError(f"http protocol error: {e}") from e
    finally:
        conn.close()

    if resp.status != 200:
        # kahyad's /policy/check still answers a well-formed JSON "deny"
        # body on some non-200 paths (docs/ipc.md §6: a malformed-body 400
        # still says {"decision":"deny",...}) - but this is a generic UDS
        # client with no opinion on any one endpoint's semantics, so a
        # non-200 status is uniformly an error here; every caller of
        # post_json already fails closed on ANY UDSHTTPError regardless of
        # cause (see hooks.py), so this loses nothing in practice.
        raise UDSHTTPError(f"unexpected HTTP status {resp.status}: {raw!r}")

    try:
        parsed = json.loads(raw)
    except json.JSONDecodeError as e:
        raise UDSHTTPError(f"response body is not valid JSON: {e}") from e

    if not isinstance(parsed, dict):
        raise UDSHTTPError(f"response body is not a JSON object: {raw!r}")

    return parsed
