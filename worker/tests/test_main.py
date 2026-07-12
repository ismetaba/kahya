"""Tests for kahya_worker.__main__: envelope validation -> exit 2, the
step-6 startup env assertions (real-key refusal + credential-mode
checks), the stdout protocol framing, and the terminal
result/error/exception mapping.

The SDK client is mocked at the module boundary
(`kahya_worker.__main__.ClaudeSDKClient`) exactly per the task spec - no
test here ever spawns a real Claude Code CLI subprocess or needs a live
Anthropic credential/session.
"""

from __future__ import annotations

import contextlib
import io
import json
import os
import sys
import tempfile
import unittest
from unittest import mock

import _pathfix  # noqa: F401

from claude_agent_sdk import AssistantMessage, ResultMessage, TextBlock

import kahya_worker.__main__ as worker_main

VALID_ENVELOPE = {
    "schema_version": 1,
    "task_id": "t_9f2c1a4b5e6d7f8091a2b3c4d5e6f708",
    "trace_id": "3f9a6b2c1d4e5f60718293a4b5c6d7e8",
    "session_id": None,
    "kind": "chat",
    "prompt": "Kadıköy'deki randevuyu hatırlat",
    "model": "claude-sonnet-5",
    "memory_injection": True,
    "created_at": "2026-07-10T12:00:00Z",
}


class _FakeStdin:
    """Stands in for sys.stdin: __main__.main() only ever calls
    `sys.stdin.buffer.read()`."""

    def __init__(self, data: bytes) -> None:
        self.buffer = io.BytesIO(data)


class _FakeClaudeSDKClient:
    """Stands in for claude_agent_sdk.ClaudeSDKClient at the module
    boundary - implements exactly the async-context-manager +
    query()/receive_response() surface `_run_session` uses."""

    def __init__(
        self,
        options=None,
        messages: list | None = None,
        raise_on_connect: Exception | None = None,
    ) -> None:
        self.options = options
        self._messages = messages or []
        self._raise_on_connect = raise_on_connect
        self.queried_prompt: str | None = None

    async def __aenter__(self) -> "_FakeClaudeSDKClient":
        if self._raise_on_connect is not None:
            raise self._raise_on_connect
        return self

    async def __aexit__(self, exc_type, exc, tb) -> bool:
        return False

    async def query(self, prompt: str) -> None:
        self.queried_prompt = prompt

    async def receive_response(self):
        for m in self._messages:
            yield m


def fake_client_factory(messages=None, raise_on_connect=None):
    """Returns a callable with ClaudeSDKClient's own call shape
    (`ClaudeSDKClient(options=...)`) that always returns a
    `_FakeClaudeSDKClient` preconfigured with the given behavior."""

    def factory(options=None):
        return _FakeClaudeSDKClient(options=options, messages=messages, raise_on_connect=raise_on_connect)

    return factory


def run_main_with(
    envelope: dict,
    env: dict[str, str],
    client_factory=None,
) -> tuple[int, str]:
    """Runs kahya_worker.__main__.main() with a fake stdin carrying
    `envelope`, `env` as the complete process environment (clear=True -
    deterministic, no leakage from the real test-runner environment), and
    `client_factory` patched in place of ClaudeSDKClient. Returns
    (exit_code, captured_stdout)."""
    stdin = _FakeStdin(json.dumps(envelope).encode("utf-8"))
    out = io.StringIO()

    patches = [
        mock.patch.object(sys, "stdin", stdin),
        mock.patch.dict(os.environ, env, clear=True),
    ]
    if client_factory is not None:
        patches.append(mock.patch.object(worker_main, "ClaudeSDKClient", client_factory))

    with contextlib.ExitStack() as stack:
        for p in patches:
            stack.enter_context(p)
        with contextlib.redirect_stdout(out):
            code = worker_main.main()

    return code, out.getvalue()


def base_env(log_dir: str, **overrides: str) -> dict[str, str]:
    env = {
        "KAHYA_TASK_ID": VALID_ENVELOPE["task_id"],
        "KAHYA_TRACE_ID": VALID_ENVELOPE["trace_id"],
        "KAHYA_SOCKET": "/tmp/does-not-matter.sock",
        "KAHYA_LOG_DIR": log_dir,
        "ANTHROPIC_BASE_URL": "http://127.0.0.1:12345",
        "ANTHROPIC_API_KEY": "kahya-task-" + "a" * 32,
        "KAHYA_MCP_BRIDGE": "/repo/bin/kahya-mcp",
        "KAHYA_CREDENTIAL_MODE": "keychain",
        "PATH": os.environ.get("PATH", "/usr/bin:/bin"),
    }
    env.update(overrides)
    return env


def read_jsonl(path: str) -> list[dict]:
    if not os.path.exists(path):
        return []
    with open(path, encoding="utf-8") as f:
        return [json.loads(line) for line in f if line.strip()]


class TestEnvelopeValidation(unittest.TestCase):
    def test_invalid_envelope_exits_2_with_turkish_message(self) -> None:
        with tempfile.TemporaryDirectory() as log_dir:
            bad_envelope = dict(VALID_ENVELOPE)
            bad_envelope["model"] = "gpt-5"  # not in the HANDOFF §9 set
            code, out = run_main_with(bad_envelope, base_env(log_dir))

        self.assertEqual(code, 2)
        lines = [json.loads(l) for l in out.splitlines() if l.strip()]
        self.assertEqual(lines, [{"type": "error", "message": "Görev zarfı geçersiz."}])

    def test_invalid_envelope_logs_english_detail(self) -> None:
        with tempfile.TemporaryDirectory() as log_dir:
            bad_envelope = dict(VALID_ENVELOPE)
            del bad_envelope["prompt"]
            run_main_with(bad_envelope, base_env(log_dir))
            lines = read_jsonl(os.path.join(log_dir, "worker.jsonl"))

        self.assertTrue(any(l.get("event") == "envelope_invalid" for l in lines))
        invalid_line = next(l for l in lines if l.get("event") == "envelope_invalid")
        self.assertIn("prompt", invalid_line.get("detail", ""))
        self.assertEqual(invalid_line.get("trace_id"), VALID_ENVELOPE["trace_id"])


class TestStartupEnvAssertions(unittest.TestCase):
    def test_missing_anthropic_base_url_exits_2(self) -> None:
        with tempfile.TemporaryDirectory() as log_dir:
            env = base_env(log_dir)
            del env["ANTHROPIC_BASE_URL"]
            code, out = run_main_with(dict(VALID_ENVELOPE), env)
        self.assertEqual(code, 2)
        self.assertEqual(out, "")

    def test_keychain_mode_rejects_non_matching_api_key(self) -> None:
        with tempfile.TemporaryDirectory() as log_dir:
            env = base_env(log_dir, KAHYA_CREDENTIAL_MODE="keychain", ANTHROPIC_API_KEY="not-a-valid-token")
            code, out = run_main_with(dict(VALID_ENVELOPE), env)
        self.assertEqual(code, 2)
        self.assertEqual(out, "")

    def test_passthrough_mode_does_not_require_task_token(self) -> None:
        messages = [
            ResultMessage(
                subtype="success", duration_ms=1, duration_api_ms=1,
                is_error=False, num_turns=1, session_id="sess-1",
            ),
        ]
        with tempfile.TemporaryDirectory() as log_dir:
            env = base_env(log_dir, KAHYA_CREDENTIAL_MODE="passthrough", ANTHROPIC_API_KEY="whatever-the-sdk-uses")
            code, out = run_main_with(
                dict(VALID_ENVELOPE), env, client_factory=fake_client_factory(messages=messages)
            )
        # Env assertions must NOT block passthrough mode just because the
        # API key isn't shaped like the per-task token - the run should
        # reach the (mocked) SDK session and succeed.
        self.assertEqual(code, 0)

    def test_real_key_anywhere_in_env_refuses_to_start(self) -> None:
        with tempfile.TemporaryDirectory() as log_dir:
            env = base_env(log_dir, SOME_OTHER_TOOL_VAR="sk-ant-oldrealkey0000000000000000")
            code, out = run_main_with(dict(VALID_ENVELOPE), env)
            lines = read_jsonl(os.path.join(log_dir, "worker.jsonl"))

        self.assertEqual(code, 2)
        self.assertEqual(out, "")  # no dedicated stdout message for this case
        self.assertTrue(any(l.get("event") == "real_key_in_env" for l in lines))
        leak_line = next(l for l in lines if l.get("event") == "real_key_in_env")
        self.assertEqual(leak_line.get("var"), "SOME_OTHER_TOOL_VAR")

    def test_real_key_check_applies_even_in_passthrough_mode(self) -> None:
        """Belt-and-braces holds in BOTH credential modes (task spec)."""
        with tempfile.TemporaryDirectory() as log_dir:
            env = base_env(
                log_dir,
                KAHYA_CREDENTIAL_MODE="passthrough",
                LEAKED="sk-ant-shouldneverbehere00000000",
            )
            code, _out = run_main_with(dict(VALID_ENVELOPE), env)
        self.assertEqual(code, 2)


class TestStdoutProtocolAndSession(unittest.TestCase):
    def test_success_stream_emits_session_delta_result_one_json_per_line(self) -> None:
        messages = [
            AssistantMessage(content=[TextBlock(text="Merhaba!")], model="claude-sonnet-5", session_id="sess-abc"),
            ResultMessage(
                subtype="success", duration_ms=10, duration_api_ms=5,
                is_error=False, num_turns=1, session_id="sess-abc",
            ),
        ]
        with tempfile.TemporaryDirectory() as log_dir:
            code, out = run_main_with(
                dict(VALID_ENVELOPE), base_env(log_dir), client_factory=fake_client_factory(messages=messages)
            )

        self.assertEqual(code, 0)
        raw_lines = [l for l in out.split("\n") if l != ""]
        parsed = [json.loads(l) for l in raw_lines]  # every line must be valid, standalone JSON

        self.assertEqual(parsed[0], {"type": "session", "session_id": "sess-abc"})
        self.assertEqual(parsed[1], {"type": "delta", "text": "Merhaba!"})
        self.assertEqual(parsed[2], {"type": "result", "status": "ok"})
        self.assertEqual(len(parsed), 3)

    def test_result_message_is_error_maps_to_error_line_and_exit_1(self) -> None:
        messages = [
            ResultMessage(
                subtype="success", duration_ms=1, duration_api_ms=1, is_error=True,
                num_turns=1, session_id="sess-1", result="rate limited",
            ),
        ]
        with tempfile.TemporaryDirectory() as log_dir:
            code, out = run_main_with(
                dict(VALID_ENVELOPE), base_env(log_dir), client_factory=fake_client_factory(messages=messages)
            )

        self.assertEqual(code, 1)
        lines = [json.loads(l) for l in out.splitlines() if l.strip()]
        self.assertEqual(len(lines), 2)  # session (session_id present) + error
        self.assertEqual(
            lines[-1],
            {
                "type": "error",
                "message": f"Model çağrısı başarısız oldu. Ayrıntı: kahya log --trace {VALID_ENVELOPE['trace_id']}",
            },
        )

    def test_sdk_connect_exception_maps_to_error_line_and_exit_1(self) -> None:
        with tempfile.TemporaryDirectory() as log_dir:
            code, out = run_main_with(
                dict(VALID_ENVELOPE),
                base_env(log_dir),
                client_factory=fake_client_factory(raise_on_connect=RuntimeError("cli not found")),
            )

        self.assertEqual(code, 1)
        lines = [json.loads(l) for l in out.splitlines() if l.strip()]
        self.assertEqual(
            lines,
            [
                {
                    "type": "error",
                    "message": f"Model çağrısı başarısız oldu. Ayrıntı: kahya log --trace {VALID_ENVELOPE['trace_id']}",
                }
            ],
        )

    def test_no_stray_output_beyond_protocol_lines(self) -> None:
        """Every captured stdout line must parse as exactly one JSON
        object - no stray prints, no partial lines."""
        messages = [
            ResultMessage(
                subtype="success", duration_ms=1, duration_api_ms=1,
                is_error=False, num_turns=1, session_id="sess-1",
            ),
        ]
        with tempfile.TemporaryDirectory() as log_dir:
            _code, out = run_main_with(
                dict(VALID_ENVELOPE), base_env(log_dir), client_factory=fake_client_factory(messages=messages)
            )

        for line in out.splitlines():
            if not line.strip():
                continue
            obj = json.loads(line)  # raises if not valid standalone JSON
            self.assertIn("type", obj)


class TestBuildOptions(unittest.TestCase):
    def test_allowed_tools_left_empty_and_mcp_server_shape(self) -> None:
        """BLOCKER 2 fix: allowed_tools must NOT whole-tool-allow the 3
        memory tools (or anything else) - doing so auto-approves them
        before can_use_tool is ever consulted. See
        test_can_use_tool_is_not_shadowed_for_real_built_options below for
        the regression that proves this against the SDK's own shadowing
        rule, not just against our own expectation of the value."""
        from kahya_worker.envelope import parse_envelope

        env = parse_envelope(json.dumps(VALID_ENVELOPE).encode("utf-8"))
        options = worker_main._build_options(env, "/tmp/k.sock", "/repo/bin/kahya-mcp")

        self.assertEqual(options.allowed_tools, [])
        self.assertEqual(options.model, "claude-sonnet-5")
        self.assertEqual(options.permission_mode, "default")
        self.assertIsNotNone(options.can_use_tool)
        mcp_cfg = options.mcp_servers["kahya_memory"]
        self.assertEqual(mcp_cfg["type"], "stdio")
        self.assertEqual(mcp_cfg["command"], "/repo/bin/kahya-mcp")
        self.assertEqual(mcp_cfg["env"]["KAHYA_SOCKET"], "/tmp/k.sock")

    def test_can_use_tool_is_not_shadowed_for_real_built_options(self) -> None:
        """BLOCKER 2 regression: for the REAL ClaudeAgentOptions this
        worker builds, the pinned SDK's own
        `claude_agent_sdk.types._get_can_use_tool_shadowed_warning` helper
        (the exact function the SDK itself uses to decide whether
        can_use_tool would silently never fire) must report NO shadowing -
        i.e. can_use_tool truly is consulted for every tool call, not
        bypassed by a whole-tool allowed_tools entry or by
        permission_mode="bypassPermissions"."""
        from claude_agent_sdk.types import _get_can_use_tool_shadowed_warning
        from kahya_worker.envelope import parse_envelope

        env = parse_envelope(json.dumps(VALID_ENVELOPE).encode("utf-8"))
        options = worker_main._build_options(env, "/tmp/k.sock", "/repo/bin/kahya-mcp")

        warning = _get_can_use_tool_shadowed_warning(options.permission_mode, options.allowed_tools)
        self.assertFalse(warning, f"can_use_tool is shadowed and would never fire: {warning!r}")


class TestResumeOptionWiring(unittest.TestCase):
    """W4-02: _build_options must translate envelope.resume/session_id
    into ClaudeAgentOptions(resume=...) - streaming input mode (and
    therefore hooks/can_use_tool) stays in force regardless, per
    TestBuildOptions above."""

    def test_resume_true_passes_session_id_as_resume_option(self) -> None:
        from kahya_worker.envelope import parse_envelope

        d = dict(VALID_ENVELOPE)
        d["session_id"] = "sess-original-123"
        d["resume"] = True
        env = parse_envelope(json.dumps(d).encode("utf-8"))

        options = worker_main._build_options(env, "/tmp/k.sock", "/repo/bin/kahya-mcp")
        self.assertEqual(options.resume, "sess-original-123")

    def test_resume_false_leaves_resume_option_unset(self) -> None:
        from kahya_worker.envelope import parse_envelope

        env = parse_envelope(json.dumps(VALID_ENVELOPE).encode("utf-8"))
        options = worker_main._build_options(env, "/tmp/k.sock", "/repo/bin/kahya-mcp")
        self.assertIsNone(options.resume)

    def test_resumed_session_still_reaches_can_use_tool(self) -> None:
        """The BLOCKER-2-fixed can_use_tool shadowing check must ALSO hold
        for a resumed session's options - resume must never accidentally
        route through a bypass permission mode or a whole-tool allow."""
        from claude_agent_sdk.types import _get_can_use_tool_shadowed_warning
        from kahya_worker.envelope import parse_envelope

        d = dict(VALID_ENVELOPE)
        d["session_id"] = "sess-original-123"
        d["resume"] = True
        env = parse_envelope(json.dumps(d).encode("utf-8"))
        options = worker_main._build_options(env, "/tmp/k.sock", "/repo/bin/kahya-mcp")

        warning = _get_can_use_tool_shadowed_warning(options.permission_mode, options.allowed_tools)
        self.assertFalse(warning, f"can_use_tool is shadowed on resume and would never fire: {warning!r}")


class TestSessionIDCapture(unittest.TestCase):
    """MINOR 4 fix: the init SystemMessage carries session_id inside
    `.data`, not as a top-level attribute - _run_session must fall back to
    `message.data["session_id"]` so the very first message of the stream
    can already surface the session id."""

    def test_system_message_init_session_id_captured_from_data(self) -> None:
        from claude_agent_sdk import SystemMessage

        messages = [
            SystemMessage(subtype="init", data={"session_id": "sess_x"}),
            ResultMessage(
                subtype="success", duration_ms=1, duration_api_ms=1,
                is_error=False, num_turns=1, session_id="sess_x",
            ),
        ]
        with tempfile.TemporaryDirectory() as log_dir:
            code, out = run_main_with(
                dict(VALID_ENVELOPE), base_env(log_dir), client_factory=fake_client_factory(messages=messages)
            )

        self.assertEqual(code, 0)
        lines = [json.loads(l) for l in out.splitlines() if l.strip()]
        self.assertEqual(lines[0], {"type": "session", "session_id": "sess_x"})


class TestRealKeyCaseInsensitivity(unittest.TestCase):
    """BLOCKER 3 fix: the real-key scan must catch SK-ANT-.../Sk-Ant-...
    variants, not just the exact lowercase "sk-ant-" needle, in EITHER
    credential mode."""

    def test_uppercase_titlecase_mixedcase_variants_refuse_to_start(self) -> None:
        variants = [
            "SK-ANT-UPPERCASEKEY00000000000000",
            "Sk-Ant-Titlecasekey00000000000000",
            "sK-aNt-MiXeDcAsEkEy00000000000000",
        ]
        for credential_mode in ("keychain", "passthrough"):
            for variant in variants:
                with self.subTest(credential_mode=credential_mode, variant=variant):
                    with tempfile.TemporaryDirectory() as log_dir:
                        env = base_env(
                            log_dir,
                            KAHYA_CREDENTIAL_MODE=credential_mode,
                            SOME_OTHER_TOOL_VAR=variant,
                        )
                        code, out = run_main_with(dict(VALID_ENVELOPE), env)
                        lines = read_jsonl(os.path.join(log_dir, "worker.jsonl"))

                    self.assertEqual(code, 2)
                    self.assertEqual(out, "")
                    self.assertTrue(any(l.get("event") == "real_key_in_env" for l in lines))


class TestTraceIDLeakRedaction(unittest.TestCase):
    """MINOR 7 fix: a leaked real key sitting in KAHYA_TRACE_ID itself must
    never be adopted as the JSONL logger's own trace_id - every emitted
    line (including the leak report) must carry a safe placeholder
    instead."""

    def test_leaked_key_in_trace_id_itself_is_not_logged_verbatim(self) -> None:
        with tempfile.TemporaryDirectory() as log_dir:
            env = base_env(log_dir, KAHYA_TRACE_ID="sk-ant-shouldneverbeloggedverbatim")
            code, out = run_main_with(dict(VALID_ENVELOPE), env)
            lines = read_jsonl(os.path.join(log_dir, "worker.jsonl"))

        self.assertEqual(code, 2)
        self.assertEqual(out, "")
        self.assertTrue(len(lines) >= 1)
        for line in lines:
            self.assertNotEqual(line.get("trace_id"), "sk-ant-shouldneverbeloggedverbatim")
            self.assertNotIn("sk-ant-", line.get("trace_id", ""))
        leak_line = next(l for l in lines if l.get("event") == "real_key_in_env")
        self.assertEqual(leak_line.get("var"), "KAHYA_TRACE_ID")


class TestReaderMode(unittest.TestCase):
    """W4-03: mode="reader" builds a TOOLLESS ClaudeAgentOptions (no MCP
    servers, no built-in tools, a can_use_tool that denies everything
    regardless, empty system_prompt) and runs through
    _run_reader_session, never _run_session/_build_options."""

    READER_ENVELOPE = {
        **VALID_ENVELOPE,
        "mode": "reader",
        "schema": "mail_summary_v1",
        "model": "claude-haiku-4-5",
        "memory_injection": False,
    }

    def test_build_reader_options_is_toolless(self) -> None:
        from kahya_worker.envelope import parse_envelope

        env = parse_envelope(json.dumps(self.READER_ENVELOPE).encode("utf-8"))
        options = worker_main._build_reader_options(env)

        self.assertEqual(options.tools, [])
        self.assertEqual(options.mcp_servers, {})
        self.assertEqual(options.allowed_tools, [])
        self.assertEqual(options.system_prompt, "")
        self.assertEqual(options.permission_mode, "default")
        self.assertIs(options.can_use_tool, worker_main._reader_deny_all_tools)

    def test_reader_deny_all_tools_denies_everything(self) -> None:
        import asyncio

        from claude_agent_sdk import PermissionResultDeny

        result = asyncio.run(worker_main._reader_deny_all_tools("Bash", {"command": "ls"}, None))
        self.assertIsInstance(result, PermissionResultDeny)

    def test_run_main_with_mode_reader_streams_deltas_and_result(self) -> None:
        messages = [
            AssistantMessage(
                content=[TextBlock(text='{"from_display":"","subject":"",'), TextBlock(text='"summary":"s","dates":[],"amounts":[]}')],
                model="claude-haiku-4-5",
            ),
            ResultMessage(subtype="success", duration_ms=1, duration_api_ms=1, is_error=False, num_turns=1, session_id="sess-reader", total_cost_usd=0.0),
        ]
        factory = fake_client_factory(messages=messages)

        with tempfile.TemporaryDirectory() as log_dir:
            env = base_env(log_dir)
            code, out = run_main_with(dict(self.READER_ENVELOPE), env, client_factory=factory)

        lines = [json.loads(l) for l in out.splitlines() if l.strip()]
        self.assertEqual(code, 0)
        self.assertEqual(lines[-1], {"type": "result", "status": "ok"})
        delta_texts = [l["text"] for l in lines if l["type"] == "delta"]
        joined = "".join(delta_texts)
        self.assertEqual(
            json.loads(joined),
            {"from_display": "", "subject": "", "summary": "s", "dates": [], "amounts": []},
        )
        # No "session" protocol line is required for reader mode (the Go
        # side keys purely on the accumulated delta content, not a
        # session_id) - but emitting none must not be treated as an error
        # either, which the exit code / result line above already prove.

    def test_run_main_with_mode_reader_error_result_maps_to_error_line(self) -> None:
        messages = [
            ResultMessage(subtype="error", duration_ms=1, duration_api_ms=1, is_error=True, num_turns=1, session_id="sess-reader", total_cost_usd=0.0, result="boom"),
        ]
        factory = fake_client_factory(messages=messages)

        with tempfile.TemporaryDirectory() as log_dir:
            env = base_env(log_dir)
            code, out = run_main_with(dict(self.READER_ENVELOPE), env, client_factory=factory)

        lines = [json.loads(l) for l in out.splitlines() if l.strip()]
        self.assertEqual(code, 1)
        self.assertEqual(lines[-1]["type"], "error")


if __name__ == "__main__":
    unittest.main()
