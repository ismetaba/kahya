"""Tests for kahya_worker.hooks: the UserPromptSubmit memory-injection
hook and the can_use_tool fail-closed policy gate (W12-09 task spec step
7). Both are exercised end-to-end against the real socketserver AF_UNIX
fixture (fixtures.UnixHTTPFixture) rather than mocked at the udshttp
layer, so these tests also double-check the two modules integrate
correctly.
"""

import asyncio
import json
import tempfile
import unittest

import _pathfix  # noqa: F401
from fixtures import UnixHTTPFixture, respond_hang, respond_json, respond_json_capturing

from kahya_worker import logging as wlog
from kahya_worker.hooks import (
    POLICY_FAIL_CLOSED_MESSAGE,
    POLICY_NEEDS_APPROVAL_NON_MCP_MESSAGE,
    make_can_use_tool,
    make_user_prompt_submit_hook,
)

from claude_agent_sdk import PermissionResultAllow, PermissionResultDeny

# The exact Turkish fixture text this task's spec pins byte-exact.
KADIKOY_FIXTURE_TEXT = "Kadıköy'de iki daire gezdik."


class TestUserPromptSubmitHook(unittest.IsolatedAsyncioTestCase):
    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self._tmp.cleanup)
        wlog.configure(self._tmp.name, "trace-hooks-test")

    async def test_injects_hafiza_block_byte_exact(self) -> None:
        block = f"<hafiza>\n{KADIKOY_FIXTURE_TEXT}\n</hafiza>"
        body = json.dumps({"results": [], "hafiza_block": block}).encode("utf-8")
        with UnixHTTPFixture(self._tmp.name, respond_json(200, body)) as sock:
            hook = make_user_prompt_submit_hook(sock, "t_1", "trace-1", memory_injection=True)
            result = await hook({"prompt": "iki daire nerede?"}, None, {})

        additional_context = result["hookSpecificOutput"]["additionalContext"]
        # Byte-exact: compare the UTF-8 encoded bytes, not just str equality,
        # so no normalization could silently paper over a mismatch.
        self.assertEqual(additional_context.encode("utf-8"), block.encode("utf-8"))
        self.assertIn(KADIKOY_FIXTURE_TEXT, additional_context)

    async def test_returns_empty_when_memory_injection_disabled(self) -> None:
        # No server needed at all: the hook must not even attempt a UDS
        # call when memory_injection is False.
        hook = make_user_prompt_submit_hook("/nonexistent.sock", "t_1", "trace-1", memory_injection=False)
        result = await hook({"prompt": "merhaba"}, None, {})
        self.assertEqual(result, {})

    async def test_continues_without_injection_on_search_timeout(self) -> None:
        with UnixHTTPFixture(self._tmp.name, respond_hang(2.0)) as sock:
            hook = make_user_prompt_submit_hook(sock, "t_1", "trace-1", memory_injection=True)
            # hooks.py's own MEMORY_SEARCH_TIMEOUT_S is 5s; patch it down so
            # this test doesn't itself take 5s per HANDOFF's fail-closed
            # 5s-timeout invariant while still exercising a genuine timeout.
            import kahya_worker.hooks as hooks_mod

            original_timeout = hooks_mod.MEMORY_SEARCH_TIMEOUT_S
            hooks_mod.MEMORY_SEARCH_TIMEOUT_S = 0.2
            try:
                result = await hook({"prompt": "iki daire nerede?"}, None, {})
            finally:
                hooks_mod.MEMORY_SEARCH_TIMEOUT_S = original_timeout

        self.assertEqual(result, {})

    async def test_returns_empty_when_hafiza_block_is_empty(self) -> None:
        body = json.dumps({"results": [], "hafiza_block": ""}).encode("utf-8")
        with UnixHTTPFixture(self._tmp.name, respond_json(200, body)) as sock:
            hook = make_user_prompt_submit_hook(sock, "t_1", "trace-1", memory_injection=True)
            result = await hook({"prompt": "merhaba"}, None, {})
        self.assertEqual(result, {})

    async def test_user_prompt_submit_does_not_block_event_loop(self) -> None:
        """A 1 s kahyad round trip must not freeze the asyncio loop the
        SDK's stdout reader / control writer run on (FIX-I-16)."""
        ticks = 0

        async def ticker() -> None:
            nonlocal ticks
            for _ in range(20):
                await asyncio.sleep(0.05)
                ticks += 1

        with UnixHTTPFixture(self._tmp.name, respond_hang(1.0)) as sock:
            hook = make_user_prompt_submit_hook(sock, "t_1", "trace-1", memory_injection=True)
            hook_task = asyncio.create_task(hook({"prompt": "iki daire nerede?"}, None, {}))
            ticker_task = asyncio.create_task(ticker())
            result = await hook_task
            ticks_at_hook_return = ticks
            await ticker_task
        # respond_hang never writes a response: the server closes the
        # connection after ~1 s, http.client raises -> UDSHTTPError -> the
        # hook continues without injection.
        self.assertEqual(result, {})
        self.assertGreaterEqual(
            ticks_at_hook_return, 5, "event loop was blocked while the UDS memory search was in flight"
        )


class TestCanUseTool(unittest.IsolatedAsyncioTestCase):
    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self._tmp.cleanup)
        wlog.configure(self._tmp.name, "trace-policy-test")

    def _log_lines(self) -> list[dict]:
        import os

        path = os.path.join(self._tmp.name, "worker.jsonl")
        if not os.path.exists(path):
            return []
        with open(path, encoding="utf-8") as f:
            return [json.loads(line) for line in f if line.strip()]

    async def test_allow(self) -> None:
        body = json.dumps({"decision": "allow", "rule": "interim-static-v1"}).encode("utf-8")
        with UnixHTTPFixture(self._tmp.name, respond_json(200, body)) as sock:
            can_use_tool = make_can_use_tool(sock, "t_1", "trace-1", None)
            result = await can_use_tool("mcp__kahya_memory__memory_search", {"query": "x"}, {})

        self.assertIsInstance(result, PermissionResultAllow)
        lines = self._log_lines()
        self.assertTrue(any(l.get("event") == "tool_gate" and l.get("decision") == "allow" for l in lines))
        self.assertTrue(all(l.get("trace_id") == "trace-policy-test" for l in lines))

    async def test_allow_with_error_field_is_self_contradictory_denies_fail_closed(self) -> None:
        """MINOR 5 fix: a 200 body of {"decision":"allow","error":"..."}
        is self-contradictory - kahyad reporting an "error" alongside
        "allow" means something went wrong forming the response, not that
        it deliberately allowed the call. Must be treated exactly like any
        other malformed response: fail-closed DENY, never a trusting
        ALLOW."""
        body = json.dumps({"decision": "allow", "error": "internal inconsistency"}).encode("utf-8")
        with UnixHTTPFixture(self._tmp.name, respond_json(200, body)) as sock:
            can_use_tool = make_can_use_tool(sock, "t_1", "trace-1", None)
            result = await can_use_tool("mcp__kahya_memory__memory_search", {"query": "x"}, {})

        self.assertIsInstance(result, PermissionResultDeny)
        self.assertEqual(result.message, POLICY_FAIL_CLOSED_MESSAGE)

    async def test_non_serializable_tool_input_denies_fail_closed_without_raising(self) -> None:
        """MINOR 6 fix: a non-JSON-serializable tool_input (e.g. a `set`)
        must not let a TypeError escape can_use_tool - it must surface as
        one more fail-closed deny, exactly like any other post_json
        failure."""
        can_use_tool = make_can_use_tool("/nonexistent.sock", "t_1", "trace-1", None)
        # A `set` is never JSON-serializable - json.dumps would otherwise
        # raise a TypeError straight out of udshttp.post_json.
        result = await can_use_tool("mcp__kahya_memory__memory_write", {"values": {1, 2, 3}}, {})

        self.assertIsInstance(result, PermissionResultDeny)
        self.assertEqual(result.message, POLICY_FAIL_CLOSED_MESSAGE)

    async def test_deny_with_server_reason(self) -> None:
        reason = "W3 politika altyapısı gelene dek yalnız hafıza araması (memory_search) açık."
        body = json.dumps({"decision": "deny", "reason": reason, "rule": "interim-static-v1"}).encode("utf-8")
        with UnixHTTPFixture(self._tmp.name, respond_json(200, body)) as sock:
            can_use_tool = make_can_use_tool(sock, "t_1", "trace-1", None)
            result = await can_use_tool("mcp__kahya_memory__memory_write", {}, {})

        self.assertIsInstance(result, PermissionResultDeny)
        self.assertEqual(result.message, reason)

    async def test_timeout_denies_fail_closed(self) -> None:
        with UnixHTTPFixture(self._tmp.name, respond_hang(2.0)) as sock:
            can_use_tool = make_can_use_tool(sock, "t_1", "trace-1", None)
            import kahya_worker.hooks as hooks_mod

            original_timeout = hooks_mod.POLICY_CHECK_TIMEOUT_S
            hooks_mod.POLICY_CHECK_TIMEOUT_S = 0.2
            try:
                result = await can_use_tool("mcp__kahya_memory__memory_write", {}, {})
            finally:
                hooks_mod.POLICY_CHECK_TIMEOUT_S = original_timeout

        self.assertIsInstance(result, PermissionResultDeny)
        self.assertEqual(result.message, POLICY_FAIL_CLOSED_MESSAGE)

    async def test_http_500_denies_fail_closed(self) -> None:
        body = json.dumps({"error": "internal"}).encode("utf-8")
        with UnixHTTPFixture(self._tmp.name, respond_json(500, body)) as sock:
            can_use_tool = make_can_use_tool(sock, "t_1", "trace-1", None)
            result = await can_use_tool("mcp__kahya_memory__memory_write", {}, {})

        self.assertIsInstance(result, PermissionResultDeny)
        self.assertEqual(result.message, POLICY_FAIL_CLOSED_MESSAGE)

    async def test_garbage_body_denies_fail_closed(self) -> None:
        with UnixHTTPFixture(self._tmp.name, respond_json(200, b"not-json{{{")) as sock:
            can_use_tool = make_can_use_tool(sock, "t_1", "trace-1", None)
            result = await can_use_tool("mcp__kahya_memory__memory_write", {}, {})

        self.assertIsInstance(result, PermissionResultDeny)
        self.assertEqual(result.message, POLICY_FAIL_CLOSED_MESSAGE)

    async def test_well_formed_but_unrecognized_decision_denies_fail_closed(self) -> None:
        # Valid JSON, valid HTTP 200, but no usable "decision" field at all
        # - a different flavor of "garbage" than invalid JSON.
        body = json.dumps({"foo": "bar"}).encode("utf-8")
        with UnixHTTPFixture(self._tmp.name, respond_json(200, body)) as sock:
            can_use_tool = make_can_use_tool(sock, "t_1", "trace-1", None)
            result = await can_use_tool("mcp__kahya_memory__memory_write", {}, {})

        self.assertIsInstance(result, PermissionResultDeny)
        self.assertEqual(result.message, POLICY_FAIL_CLOSED_MESSAGE)

    # ---- B-1: preflight + needs_approval deferral ----

    async def test_needs_approval_for_kahya_mcp_tool_defers_to_binding_gate(self) -> None:
        """B-1: needs_approval for one of kahyad's own MCP tools is ALLOWED
        through to kahyad's binding gate on /v1/mcp, which parks the call
        for the human decision and executes exactly the approved bytes -
        before B-1 this fell into the garbage branch and denied."""
        body = json.dumps({
            "decision": "needs_approval",
            "reason": "Bu eylem için onay gerekiyor (otonomi seviyesi yetersiz).",
            "rule": "ladder-v1",
        }).encode("utf-8")
        with UnixHTTPFixture(self._tmp.name, respond_json(200, body)) as sock:
            can_use_tool = make_can_use_tool(sock, "t_1", "trace-1", None)
            result = await can_use_tool(
                "mcp__kahya_memory__fs_write", {"path": "~/x.md", "content_base64": "aGk="}, {}
            )

        self.assertIsInstance(result, PermissionResultAllow)
        lines = self._log_lines()
        self.assertTrue(
            any(l.get("event") == "tool_gate" and l.get("decision") == "defer" for l in lines),
            lines,
        )
        self.assertTrue(all(l.get("trace_id") == "trace-policy-test" for l in lines))

    async def test_needs_approval_for_non_mcp_tool_denies_with_turkish_message(self) -> None:
        """B-1: an SDK built-in tool has no kahyad-side executor to defer
        to - needs_approval for it fails closed with the exact Turkish
        message."""
        body = json.dumps({"decision": "needs_approval", "reason": "x", "rule": "ladder-v1"}).encode("utf-8")
        with UnixHTTPFixture(self._tmp.name, respond_json(200, body)) as sock:
            can_use_tool = make_can_use_tool(sock, "t_1", "trace-1", None)
            result = await can_use_tool("Bash", {"command": "ls"}, {})

        self.assertIsInstance(result, PermissionResultDeny)
        self.assertEqual(result.message, POLICY_NEEDS_APPROVAL_NON_MCP_MESSAGE)
        self.assertEqual(
            POLICY_NEEDS_APPROVAL_NON_MCP_MESSAGE.encode("utf-8"),
            "Bu araç için onay gerekiyor ancak bu yüzeyden onaylanamaz (reddedildi).".encode("utf-8"),
        )

    async def test_needs_approval_with_error_field_denies_fail_closed(self) -> None:
        """B-1: a self-contradictory needs_approval body (an "error"
        alongside it) is garbage - fail-closed, never a deferral."""
        body = json.dumps({"decision": "needs_approval", "error": "x"}).encode("utf-8")
        with UnixHTTPFixture(self._tmp.name, respond_json(200, body)) as sock:
            can_use_tool = make_can_use_tool(sock, "t_1", "trace-1", None)
            result = await can_use_tool("mcp__kahya_memory__memory_write", {}, {})

        self.assertIsInstance(result, PermissionResultDeny)
        self.assertEqual(result.message, POLICY_FAIL_CLOSED_MESSAGE)

    async def test_policy_check_request_carries_preflight_true(self) -> None:
        """B-1: every /policy/check the worker sends is a preflight, so
        kahyad mints no pending approval/token for it."""
        captured: list = []
        body = json.dumps({"decision": "allow", "rule": "ladder-v1"}).encode("utf-8")
        with UnixHTTPFixture(self._tmp.name, respond_json_capturing(200, body, captured)) as sock:
            can_use_tool = make_can_use_tool(sock, "t_1", "trace-1", "sess-1")
            result = await can_use_tool("mcp__kahya_memory__memory_search", {"query": "x"}, {})

        self.assertIsInstance(result, PermissionResultAllow)
        self.assertEqual(len(captured), 1)
        self.assertIs(captured[0]["preflight"], True)
        self.assertEqual(captured[0]["tool_name"], "mcp__kahya_memory__memory_search")
        self.assertEqual(captured[0]["task_id"], "t_1")

    async def test_connection_refused_denies_fail_closed(self) -> None:
        """Fail-closed proven with kahyad's socket absent entirely (the
        W12-09 acceptance criterion): can_use_tool must deny, well under
        the 5s budget, with the exact Turkish fail-closed message."""
        import os
        import time

        missing_sock = os.path.join(self._tmp.name, "no-such-kahyad.sock")
        can_use_tool = make_can_use_tool(missing_sock, "t_1", "trace-1", None)

        start = time.monotonic()
        result = await can_use_tool("mcp__kahya_memory__memory_write", {}, {})
        elapsed = time.monotonic() - start

        self.assertIsInstance(result, PermissionResultDeny)
        self.assertEqual(result.message, POLICY_FAIL_CLOSED_MESSAGE)
        self.assertLess(elapsed, 6.0)

    async def test_can_use_tool_does_not_block_event_loop(self) -> None:
        """A 1 s kahyad round trip must not freeze the asyncio loop the
        SDK's stdout reader / control writer run on (FIX-I-16)."""
        ticks = 0

        async def ticker() -> None:
            nonlocal ticks
            for _ in range(20):
                await asyncio.sleep(0.05)
                ticks += 1

        with UnixHTTPFixture(self._tmp.name, respond_hang(1.0)) as sock:
            gate = make_can_use_tool(sock, "t1", "trace-1", None)
            gate_task = asyncio.create_task(gate("mcp__kahya_memory__memory_search", {"q": "x"}, None))
            ticker_task = asyncio.create_task(ticker())
            result = await gate_task
            ticks_at_gate_return = ticks
            await ticker_task
        # fixtures.respond_hang sleeps delay_s and NEVER writes a response:
        # the handler returns, the server closes the connection, http.client
        # raises -> UDSHTTPError -> fail-closed deny after ~1 s.
        self.assertIsInstance(result, PermissionResultDeny)
        self.assertEqual(result.message, POLICY_FAIL_CLOSED_MESSAGE)
        self.assertGreaterEqual(ticks_at_gate_return, 5, "event loop was blocked while the UDS call was in flight")


if __name__ == "__main__":
    unittest.main()
