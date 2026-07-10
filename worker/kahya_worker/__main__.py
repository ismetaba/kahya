"""kahya_worker entrypoint (``docs/ipc.md``; HANDOFF §4 ⚑ IPC contract).

Reads ONE JSON envelope from stdin (then EOF), validates it, configures
JSONL logging, asserts the credential-mode environment invariants (step
6 - see ``_check_credential_env``/``_check_real_key_leak`` below), then
runs exactly one ``ClaudeSDKClient`` streaming-input session with the
``UserPromptSubmit`` memory-injection hook and ``can_use_tool``
fail-closed policy gate wired in, and speaks the worker stdout protocol
(``docs/ipc.md`` §4). stdout carries ONLY those protocol lines - one JSON
object per line; every diagnostic goes to ``worker.jsonl``
(``kahya_worker.logging``) or stderr.

Deliberately NEVER uses ``claude_agent_sdk.query()`` (the one-shot
helper): HANDOFF §4 ⚑ is explicit that the ``UserPromptSubmit`` hook and
``can_use_tool`` callback this task wires do not run in that mode - only
``ClaudeSDKClient``'s streaming-input mode runs them.
"""

from __future__ import annotations

import asyncio
import json
import os
import re
import sys
from typing import Any

from claude_agent_sdk import (
    AssistantMessage,
    ClaudeAgentOptions,
    ClaudeSDKClient,
    HookMatcher,
    ResultMessage,
    TextBlock,
)

from . import logging as wlog
from .envelope import Envelope, EnvelopeError, parse_envelope
from .hooks import make_can_use_tool, make_user_prompt_submit_hook
from .system_prompt import SYSTEM_PROMPT

# The two fixed Turkish stdout error messages this file emits (HANDOFF §3
# language policy: user-facing strings in Turkish, byte-exact, never
# paraphrased - docs/ipc.md §4).
MSG_ENVELOPE_INVALID = "Görev zarfı geçersiz."
MSG_MODEL_CALL_FAILED_FMT = "Model çağrısı başarısız oldu. Ayrıntı: kahya log --trace {trace_id}"

# EXACTLY these three MCP tools - no SDK built-in file/exec tools
# (Read/Glob/Grep/Bash/...) in W1-2 (task spec step 2). Keep this list AND
# system_prompt.SYSTEM_PROMPT byte-identical across every task/change -
# together they are the §4 ⚑ prompt-cache discipline's frozen prefix.
ALLOWED_TOOLS = [
    "mcp__kahya_memory__memory_search",
    "mcp__kahya_memory__memory_write",
    "mcp__kahya_memory__memory_forget",
]

# --- OWNER AUTH DECISION (docs/ipc.md's W12-08 note; this task's spec) ---
#
# HANDOFF §4 assumes kahyad injects a real Anthropic API key read from the
# Keychain at its forward-proxy. The owner decided NOT to provision a
# separate Anthropic API key for this project: the worker
# (claude-agent-sdk) instead authenticates through its OWN, already
# logged-in Claude Code SDK session, forwarded unmodified by kahyad's
# proxy in "passthrough" mode (the current default - see
# kahyad/internal/anthproxy's package doc + config.CredentialMode).
#
# Concretely, this means the per-task local proxy token
# (ANTHROPIC_API_KEY="kahya-task-<hex32>") is:
#   - REQUIRED to match that exact shape in "keychain" mode (the original
#     HANDOFF design, still fully supported as a fallback) - a worker
#     misconfigured this way would otherwise silently fail every proxied
#     call with a 401 much later, so the assertion below catches it at
#     startup instead.
#   - NOT enforced as the worker's OWN auth in "passthrough" mode - the
#     SDK subprocess supplies its own upstream auth header independently,
#     and kahyad's proxy forwards it unchanged after validating the local
#     token itself (a proxy-side concern, not this worker's).
#
# Either way, the "API anahtarı worker'a verilmez" invariant (HANDOFF §4
# IPC ⚑ - a real Anthropic key must never reach the worker) is checked
# BELT-AND-BRACES IN BOTH MODES: no environment value may ever contain the
# "sk-ant-" prefix of a real Anthropic key (see _check_real_key_leak).
_TASK_TOKEN_RE = re.compile(r"^kahya-task-[0-9a-f]{32}$")
_REAL_KEY_NEEDLE = "sk-ant-"

_CREDENTIAL_MODE_KEYCHAIN = "keychain"
_CREDENTIAL_MODE_PASSTHROUGH = "passthrough"


def _print_protocol_line(obj: dict[str, Any]) -> None:
    """Writes exactly one JSON object as one stdout line, then flushes -
    the ONLY thing this process ever writes to stdout (docs/ipc.md §4)."""
    sys.stdout.write(json.dumps(obj, ensure_ascii=False) + "\n")
    sys.stdout.flush()


def _check_real_key_leak() -> str | None:
    """Returns the name of the FIRST environment variable whose value
    looks like a real Anthropic API key (contains "sk-ant-"), or None if
    none do. Checked regardless of credential_mode (see module doc
    comment) - "API anahtarı worker'a verilmez" holds in both modes."""
    for name, value in os.environ.items():
        if _REAL_KEY_NEEDLE in value:
            return name
    return None


def _check_credential_env(credential_mode: str) -> str | None:
    """Returns a description of the first violated startup env
    invariant (step 6), or None if they all hold. ANTHROPIC_BASE_URL must
    always be set - the worker only ever talks to kahyad's own per-task
    forward-proxy listener, never a real upstream directly, in EITHER
    mode. In "keychain" mode, ANTHROPIC_API_KEY must additionally match
    the per-task token shape exactly (see the module's OWNER AUTH
    DECISION comment for why "passthrough" mode does not enforce this)."""
    if not os.environ.get("ANTHROPIC_BASE_URL", "").strip():
        return "ANTHROPIC_BASE_URL is not set"
    if credential_mode == _CREDENTIAL_MODE_KEYCHAIN:
        api_key = os.environ.get("ANTHROPIC_API_KEY", "")
        if not _TASK_TOKEN_RE.match(api_key):
            return "ANTHROPIC_API_KEY does not match the per-task token shape ^kahya-task-[0-9a-f]{32}$"
    return None


def _fail_envelope_invalid(detail: str) -> int:
    """Common failure path for step 1: log the English detail, emit the
    fixed Turkish stdout error line, return exit code 2."""
    wlog.log("error", "envelope_invalid", detail=detail)
    _print_protocol_line({"type": "error", "message": MSG_ENVELOPE_INVALID})
    return 2


def main(argv: list[str] | None = None) -> int:
    raw = sys.stdin.buffer.read()

    try:
        envelope = parse_envelope(raw)
    except EnvelopeError as e:
        # KAHYA_LOG_DIR/KAHYA_TRACE_ID are plain env vars spawn always
        # sets regardless of the envelope's own (possibly invalid)
        # content (docs/ipc.md §3), so logging can be configured even
        # when the envelope itself failed to parse.
        wlog.configure(os.environ.get("KAHYA_LOG_DIR", "."), os.environ.get("KAHYA_TRACE_ID", ""))
        return _fail_envelope_invalid(str(e))

    wlog.configure(os.environ.get("KAHYA_LOG_DIR", "."), os.environ.get("KAHYA_TRACE_ID", envelope.trace_id))

    leaked_var = _check_real_key_leak()
    if leaked_var is not None:
        # Belt-and-braces security invariant violation - should never
        # happen in production. No dedicated Turkish stdout message is
        # defined for this case (unlike step 1/step 5's messages); exit 2
        # with no terminal stdout line falls through to kahyad's own
        # generic "unexpected exit" Turkish message (docs/ipc.md's
        # "Unexpected termination" rule), which is the correct posture
        # for a should-never-happen internal invariant failure.
        wlog.log("error", "real_key_in_env", var=leaked_var)
        return 2

    credential_mode = os.environ.get("KAHYA_CREDENTIAL_MODE") or _CREDENTIAL_MODE_KEYCHAIN
    env_err = _check_credential_env(credential_mode)
    if env_err is not None:
        wlog.log("error", "worker_env_invalid", detail=env_err, credential_mode=credential_mode)
        return 2

    socket_path = os.environ.get("KAHYA_SOCKET", "")
    mcp_bridge = os.environ.get("KAHYA_MCP_BRIDGE", "")

    return asyncio.run(_run_session(envelope, socket_path, mcp_bridge))


def _build_options(envelope: Envelope, socket_path: str, mcp_bridge: str) -> ClaudeAgentOptions:
    """Builds the one ClaudeAgentOptions this task's session uses (step
    2): model/system_prompt/mcp_servers/allowed_tools/hooks/can_use_tool
    exactly as the task spec fixes. Factored out of _run_session so tests
    can inspect the constructed options directly without opening a real
    SDK session."""
    return ClaudeAgentOptions(
        model=envelope.model,
        system_prompt=SYSTEM_PROMPT,
        mcp_servers={
            "kahya_memory": {
                "type": "stdio",
                "command": mcp_bridge,
                "env": {
                    "KAHYA_SOCKET": socket_path,
                    "KAHYA_TRACE_ID": envelope.trace_id,
                },
            },
        },
        allowed_tools=list(ALLOWED_TOOLS),
        # Default permission_mode so can_use_tool actually fires (task
        # spec step 2) - never "bypassPermissions"/"acceptEdits", which
        # would shadow it.
        permission_mode="default",
        can_use_tool=make_can_use_tool(socket_path, envelope.task_id, envelope.trace_id, envelope.session_id),
        hooks={
            "UserPromptSubmit": [
                HookMatcher(
                    hooks=[
                        make_user_prompt_submit_hook(
                            socket_path, envelope.task_id, envelope.trace_id, envelope.memory_injection
                        )
                    ]
                ),
            ],
        },
    )


async def _run_session(envelope: Envelope, socket_path: str, mcp_bridge: str) -> int:
    """Runs the one streaming-input ClaudeSDKClient session for this task
    (steps 2/5): sends envelope.prompt, relays each assistant text delta
    and the first session_id seen as protocol stdout lines, and maps the
    outcome onto exactly one terminal protocol line + exit code."""
    options = _build_options(envelope, socket_path, mcp_bridge)

    try:
        async with ClaudeSDKClient(options=options) as client:
            await client.query(envelope.prompt)

            session_emitted = False
            async for message in client.receive_response():
                session_id = getattr(message, "session_id", None)
                if not session_emitted and session_id:
                    _print_protocol_line({"type": "session", "session_id": session_id})
                    session_emitted = True

                if isinstance(message, AssistantMessage):
                    for block in message.content:
                        if isinstance(block, TextBlock):
                            _print_protocol_line({"type": "delta", "text": block.text})
                elif isinstance(message, ResultMessage):
                    if message.is_error:
                        wlog.log("error", "model_call_failed", result=str(message.result))
                        _print_protocol_line(
                            {"type": "error", "message": MSG_MODEL_CALL_FAILED_FMT.format(trace_id=envelope.trace_id)}
                        )
                        return 1
    except Exception as e:  # noqa: BLE001 - any SDK/transport failure surfaces as one user-facing error line.
        wlog.log("error", "sdk_error", error=str(e))
        _print_protocol_line(
            {"type": "error", "message": MSG_MODEL_CALL_FAILED_FMT.format(trace_id=envelope.trace_id)}
        )
        return 1

    _print_protocol_line({"type": "result", "status": "ok"})
    return 0


if __name__ == "__main__":
    sys.exit(main())
