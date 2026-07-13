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
import time
from typing import Any

from claude_agent_sdk import (
    AssistantMessage,
    ClaudeAgentOptions,
    ClaudeSDKClient,
    CLINotFoundError,
    HookMatcher,
    ResultMessage,
    TextBlock,
)

from . import logging as wlog
from . import stt
from .envelope import Envelope, EnvelopeError, parse_envelope
from .hooks import make_can_use_tool, make_user_prompt_submit_hook
from .system_prompt import SYSTEM_PROMPT

# The two fixed Turkish stdout error messages this file emits (HANDOFF §3
# language policy: user-facing strings in Turkish, byte-exact, never
# paraphrased - docs/ipc.md §4).
MSG_ENVELOPE_INVALID = "Görev zarfı geçersiz."
MSG_MODEL_CALL_FAILED_FMT = "Model çağrısı başarısız oldu. Ayrıntı: kahya log --trace {trace_id}"

# W6-02: the byte-exact Turkish error for an empty/whitespace-only
# transcript (tasks/w6-voice/W6-02-ptt-whisper.md step 3) - an empty
# transcript is NEVER submitted to the task loop as a prompt; this is the
# terminal stdout protocol message the CLI/hammerspoon surface instead.
MSG_EMPTY_TRANSCRIPT = "Ses anlaşılamadı — lütfen tekrar deneyin"

# W6-02: a generic fallback for an mode="stt" failure that is neither
# stt.SttModelMissingError nor an empty transcript (e.g. the wav path does
# not exist, or mlx_whisper itself raises something else entirely) - kept
# distinct from MSG_MODEL_CALL_FAILED_FMT's wording (that message is about
# a MODEL call, this is about local audio transcription) but follows the
# same "point at kahya log --trace" convention.
MSG_STT_FAILED_FMT = "Ses işlenemedi. Ayrıntı: kahya log --trace {trace_id}"

# W4-04: exit code this process uses for the cloud_unreachable protocol
# line below - distinct from 1 (ordinary model-call failure) and 2
# (envelope invalid) so kahyad/internal/spawn can tell the three apart
# from the exit code alone, in addition to the stdout protocol line
# itself (spawn.go's own stdoutLine.Event field is the primary signal;
# the exit code is belt-and-braces).
_EXIT_CLOUD_UNREACHABLE = 3

# kahyad/internal/anthproxy.MsgCloudUnreachableMarker's exact value
# (kahyad/internal/anthproxy/proxy.go) - embedded in the Anthropic-shaped
# error body kahyad's own forward-proxy returns once its W4-04 inline
# retry budget is exhausted. This worker has no other way to distinguish
# "kahyad's own retry budget ran out" from an ordinary model-call failure
# (it never sees the proxy's internal retry bookkeeping directly - only
# whatever claude_agent_sdk/the underlying CLI subprocess ends up raising
# after ITS OWN attempt to reach ANTHROPIC_BASE_URL finally fails), so
# this is necessarily a string-content check over the raised exception,
# not a typed one. Kept in English (CLAUDE.md §3: technical/internal
# identifiers stay English) - never shown to the user directly.
_CLOUD_UNREACHABLE_MARKER = "kahya_cloud_unreachable"

# Additional best-effort substrings that plausibly indicate a genuine
# transport/connectivity failure reaching kahyad's own localhost proxy
# (as opposed to CLINotFoundError - a local install problem - or an
# ordinary API-level failure the SDK already surfaces as ResultMessage.
# is_error, handled separately below) - belt-and-braces in case the
# underlying CLI subprocess never echoes the marker string above
# verbatim into whatever claude_agent_sdk raises.
_CLOUD_UNREACHABLE_FALLBACK_MARKERS = (
    "econnrefused",
    "econnreset",
    "connection refused",
    "connection reset",
    "socket hang up",
    "fetch failed",
)

# Safe placeholder trace_id used ONLY when the real-key leak check
# (_check_real_key_leak) fires - see MINOR 7 fix in main() below. Never the
# tainted KAHYA_TRACE_ID value itself.
_SAFE_TRACE_ID_PLACEHOLDER = "redacted"

# The exact 3 MCP tool names this worker's "kahya_memory" MCP server
# exposes (kahyad's own /policy/check policy table keys off these same
# SDK-prefixed names - see hooks.make_can_use_tool). Kept as documentation/
# cross-check reference only as of the BLOCKER 2 fix in _build_options
# below: this list is DELIBERATELY NOT passed to
# ClaudeAgentOptions.allowed_tools anymore. See _build_options's own
# comment for why - the short version is that the pinned SDK
# (claude-agent-sdk==0.2.111) whole-tool-allows any bare tool name listed
# in allowed_tools, auto-approving it before can_use_tool is ever
# consulted, which would make the mandated fail-closed policy gate
# (HANDOFF §5 ⚑) dead code for exactly these 3 tools.
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
    looks like a real Anthropic API key (contains "sk-ant-", checked
    CASE-INSENSITIVELY so SK-ANT-.../Sk-Ant-... variants are caught too -
    BLOCKER 3 fix), or None if none do. Checked regardless of
    credential_mode (see module doc comment) - "API anahtarı worker'a
    verilmez" holds in both modes."""
    for name, value in os.environ.items():
        if _REAL_KEY_NEEDLE in value.lower():
            return name
    return None


def _check_credential_env(credential_mode: str, mode: str) -> str | None:
    """Returns a description of the first violated startup env
    invariant (step 6), or None if they all hold. ANTHROPIC_BASE_URL must
    always be set - the worker only ever talks to kahyad's own per-task
    forward-proxy listener, never a real upstream directly, in EITHER
    mode. In "keychain" mode, ANTHROPIC_API_KEY must additionally match
    the per-task token shape exactly (see the module's OWNER AUTH
    DECISION comment for why "passthrough" mode does not enforce this).

    W6-02: mode=="stt" sessions (envelope.Mode == spawn.ModeSTT, Go side)
    never construct a ClaudeAgentOptions/ClaudeSDKClient at all - they only
    call kahya_worker.stt.transcribe() as a local library function
    (_run_stt_only below) - so neither check applies to them; kahyad's own
    stt-phase caller (kahyad/internal/server) deliberately does not even
    open a per-task Anthropic forward-proxy listener for this mode, and
    passes ANTHROPIC_BASE_URL/ANTHROPIC_API_KEY empty. This is what makes
    "100% local" structural rather than incidental for the voice-capture
    phase: there is no reachable network endpoint for this process to hit
    even if something tried."""
    if mode == "stt":
        return None
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


def _is_cloud_unreachable(exc: Exception) -> bool:
    """W4-04: true when exc looks like kahyad's own forward-proxy telling
    this worker its inline retry budget is exhausted (or, best-effort, any
    other genuine transport/connectivity failure reaching it) - as opposed
    to CLINotFoundError (a local install problem, never retried) or an
    ordinary API-level failure the SDK already surfaces as ResultMessage.
    is_error (handled by the caller BEFORE this function is ever
    consulted - see _run_session's ResultMessage.is_error branch, which
    returns before reaching the except block below).

    See _CLOUD_UNREACHABLE_MARKER's own doc comment for why this is a
    string-content heuristic, not a typed check: this worker has no
    direct visibility into kahyad/internal/anthproxy's own retry
    bookkeeping.
    """
    if isinstance(exc, CLINotFoundError):
        return False
    text = str(exc).lower()
    if _CLOUD_UNREACHABLE_MARKER in text:
        return True
    return any(marker in text for marker in _CLOUD_UNREACHABLE_FALLBACK_MARKERS)


def main(argv: list[str] | None = None) -> int:
    raw = sys.stdin.buffer.read()

    # MINOR 7 fix: scan for a leaked real key BEFORE anything below ever
    # configures the JSONL logger with a trace_id sourced from the
    # environment (KAHYA_TRACE_ID, adopted a few lines down and in the
    # EnvelopeError branch below) - otherwise a leaked sk-ant-* value
    # sitting in KAHYA_TRACE_ID itself would be written into EVERY
    # subsequent worker.jsonl line, INCLUDING the very leak report meant to
    # flag it. Runs even before envelope parsing, so both the success path
    # and the EnvelopeError path below are covered by the same guard.
    leaked_var = _check_real_key_leak()
    if leaked_var is not None:
        # Configure with a SAFE placeholder trace_id, never the (possibly
        # tainted) KAHYA_TRACE_ID env value.
        wlog.configure(os.environ.get("KAHYA_LOG_DIR", "."), _SAFE_TRACE_ID_PLACEHOLDER)
        # Belt-and-braces security invariant violation - should never
        # happen in production. No dedicated Turkish stdout message is
        # defined for this case (unlike step 1/step 5's messages); exit 2
        # with no terminal stdout line falls through to kahyad's own
        # generic "unexpected exit" Turkish message (docs/ipc.md's
        # "Unexpected termination" rule), which is the correct posture
        # for a should-never-happen internal invariant failure.
        wlog.log("error", "real_key_in_env", var=leaked_var)
        return 2

    try:
        envelope = parse_envelope(raw)
    except EnvelopeError as e:
        # KAHYA_LOG_DIR/KAHYA_TRACE_ID are plain env vars spawn always
        # sets regardless of the envelope's own (possibly invalid)
        # content (docs/ipc.md §3), so logging can be configured even
        # when the envelope itself failed to parse. The real-key leak
        # check above already ran, so adopting KAHYA_TRACE_ID here is safe.
        wlog.configure(os.environ.get("KAHYA_LOG_DIR", "."), os.environ.get("KAHYA_TRACE_ID", ""))
        return _fail_envelope_invalid(str(e))

    wlog.configure(os.environ.get("KAHYA_LOG_DIR", "."), os.environ.get("KAHYA_TRACE_ID", envelope.trace_id))

    credential_mode = os.environ.get("KAHYA_CREDENTIAL_MODE") or _CREDENTIAL_MODE_KEYCHAIN
    env_err = _check_credential_env(credential_mode, envelope.mode)
    if env_err is not None:
        wlog.log("error", "worker_env_invalid", detail=env_err, credential_mode=credential_mode)
        return 2

    socket_path = os.environ.get("KAHYA_SOCKET", "")
    mcp_bridge = os.environ.get("KAHYA_MCP_BRIDGE", "")

    # HANDOFF §4 IPC ⚑ ("Tüm süreçler her satırda trace_id içeren JSONL
    # loglar") / the W1-2 gate's "single trace_id" acceptance criterion
    # (docs/ipc.md, tasks/w1-2-core/W12-10) requires worker.jsonl to carry
    # this task's trace_id on at least one line - unconditionally, not only
    # on an error/tool-call path. Without this line, a fully successful
    # task that never hits a memory-search failure and never has the model
    # attempt a tool call (both of the only two other call sites that log
    # anything - hooks.make_user_prompt_submit_hook's warn-on-failure and
    # make_can_use_tool's per-decision info line) would leave worker.jsonl
    # empty, silently violating that invariant on exactly the happy path.
    wlog.log("info", "task_started", task_id=envelope.task_id, model=envelope.model)

    # W4-03: mode="reader" runs the TOOLLESS Reader session instead of the
    # ordinary Actor/chat one - no MCP servers, no memory-injection hook,
    # no can_use_tool consultation of kahyad at all (a can_use_tool IS
    # still wired, but it unconditionally denies - see
    # _reader_deny_all_tools - belt-and-braces against any SDK built-in
    # tool the model might otherwise attempt, on top of tools=[] disabling
    # the built-in set outright).
    if envelope.mode == "reader":
        return asyncio.run(_run_reader_session(envelope))

    # W6-02: mode="stt" runs the toolless, CLOUD-LESS local transcription
    # path instead of any agent session at all - no asyncio.run needed,
    # mlx_whisper.transcribe (kahya_worker.stt.transcribe) is a plain
    # blocking library call (see _run_stt_only's own doc comment).
    if envelope.mode == "stt":
        return _run_stt_only(envelope)

    return asyncio.run(_run_session(envelope, socket_path, mcp_bridge))


def _build_options(envelope: Envelope, socket_path: str, mcp_bridge: str) -> ClaudeAgentOptions:
    """Builds the one ClaudeAgentOptions this task's session uses (step
    2): model/system_prompt/mcp_servers/allowed_tools/hooks/can_use_tool
    exactly as the task spec fixes. Factored out of _run_session so tests
    can inspect the constructed options directly without opening a real
    SDK session.

    W4-02: when envelope.resume is true, resume=envelope.session_id is
    passed so the SDK subprocess resumes the stored conversation instead
    of starting a fresh one (docs/ipc.md's own W4-02 note) - streaming
    input mode stays in force either way (HANDOFF §4 ⚑: hooks/
    can_use_tool do not run under one-shot query()), so nothing else in
    this function's shape changes for a resumed task."""
    return ClaudeAgentOptions(
        model=envelope.model,
        resume=envelope.session_id if envelope.resume else None,
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
        # --- BLOCKER 2 FIX (deviation from this task's own step 2 note,
        # recorded in tasks/w1-2-core/W12-09-python-worker-harness.md) ---
        # The task file's step 2 literally says allowed_tools = exactly
        # ALLOWED_TOOLS (the 3 memory tools). Verified against the pinned
        # SDK (claude_agent_sdk==0.2.111)'s own
        # types._whole_tool_allowed/_get_can_use_tool_shadowed_warning: a
        # bare tool-name entry in allowed_tools (no "(...)" specifier)
        # whole-tool-allows it, auto-approving every call BEFORE
        # can_use_tool is ever consulted - so passing ALLOWED_TOOLS here
        # would make can_use_tool (and the fail-closed /policy/check gate +
        # event=tool_gate logging it drives - HANDOFF §5 ⚑) dead code for
        # exactly the 3 tools this worker actually calls. Deliberately left
        # EMPTY (the SDK default) instead: with permission_mode="default"
        # below, every tool call - the 3 memory tools AND any SDK built-in
        # file/exec tool the model might attempt - is routed through
        # can_use_tool, which denies anything that is not memory_search per
        # kahyad's interim /policy/check table. Built-in tools are
        # therefore still effectively unavailable, just enforced by the
        # policy gate rather than by omission from allowed_tools. See
        # worker/tests/test_main.py::TestBuildOptions for the regression
        # that proves types._get_can_use_tool_shadowed_warning returns
        # falsy for these exact options (i.e. can_use_tool truly fires).
        allowed_tools=[],
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
                # MINOR 4 fix: the init SystemMessage (subtype="init") has
                # no top-level session_id attribute - the pinned SDK's
                # message_parser puts it inside message.data instead (see
                # claude_agent_sdk._internal.message_parser's generic
                # SystemMessage branch: `SystemMessage(subtype=subtype,
                # data=data)`). Fall back to message.data["session_id"] so
                # the FIRST message of the stream (the init message) can
                # already surface the session id, not just a later one that
                # happens to carry it as a real attribute.
                session_id = getattr(message, "session_id", None)
                if session_id is None:
                    data = getattr(message, "data", None)
                    if isinstance(data, dict):
                        session_id = data.get("session_id")
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
        # W4-04: kahyad's own forward-proxy exhausted its inline retry
        # budget (kahyad/internal/anthproxy) - this is NOT an ordinary
        # model-call failure, so it gets its own protocol line/exit code
        # instead of the generic Turkish error line: kahyad's task-
        # durability layer (kahyad/internal/task.CloudRetry) has ALREADY
        # parked this task in bekliyor-yeniden-deneme synchronously, mid-
        # call, via the proxy's own OnCloudUnreachable callback - this
        # line is diagnostic/UX only (the offline-command-finishes-later
        # acceptance criterion), never the mechanism that actually retries
        # the task.
        if _is_cloud_unreachable(e):
            wlog.log("error", "cloud_unreachable", error=str(e))
            _print_protocol_line({"event": "cloud_unreachable"})
            return _EXIT_CLOUD_UNREACHABLE
        wlog.log("error", "sdk_error", error=str(e))
        _print_protocol_line(
            {"type": "error", "message": MSG_MODEL_CALL_FAILED_FMT.format(trace_id=envelope.trace_id)}
        )
        return 1

    wlog.log("info", "task_done", task_id=envelope.task_id)
    _print_protocol_line({"type": "result", "status": "ok"})
    return 0


# --- W4-03: Reader mode (toolless, cloud-Haiku half of the Reader/Actor
# split - kahyad/internal/reader's own doc comment). The secret-lane half
# never reaches this process at all: kahyad talks to the local Qwen server
# directly over HTTP (kahyad/internal/reader.NewLocalModel), never through
# ClaudeSDKClient - see that package's doc comment for why. ---


async def _reader_deny_all_tools(tool_name: str, tool_input: dict[str, Any], ctx: Any) -> Any:
    """The Reader session's can_use_tool: unconditionally denies every
    call. Belt-and-braces on top of ClaudeAgentOptions(tools=[]) (which
    already disables every SDK built-in tool outright) and mcp_servers={}
    (no MCP server is even wired) - a Reader session is toolless by design
    (HANDOFF §5 safety #2: "araçsız 'Okuyucu'"), and this is the third,
    independent layer that guarantees it."""
    from claude_agent_sdk import PermissionResultDeny

    return PermissionResultDeny(message="Reader oturumu araçsızdır (toolless).")


def _build_reader_options(envelope: Envelope) -> ClaudeAgentOptions:
    """Builds the toolless Reader session's ClaudeAgentOptions: no MCP
    servers, no built-in tools (tools=[]), no memory-injection hook, a
    can_use_tool that denies everything regardless, and system_prompt=""
    (the Reader's actual extraction instructions are already part of
    envelope.prompt itself - kahyad/internal/reader constructs that single
    combined string, since the envelope wire schema carries one prompt
    field, not a separate system/user split - an empty system_prompt here
    suppresses the SDK's own default Claude-Code-persona system prompt
    entirely, so nothing but envelope.prompt's own instructions reaches
    the model)."""
    return ClaudeAgentOptions(
        model=envelope.model,
        system_prompt="",
        tools=[],
        mcp_servers={},
        allowed_tools=[],
        permission_mode="default",
        can_use_tool=_reader_deny_all_tools,
    )


async def _run_reader_session(envelope: Envelope) -> int:
    """Runs one toolless Reader session: sends envelope.prompt, relays
    every assistant text delta as an ordinary "delta" protocol line
    (kahyad accumulates these into the single JSON object it then parses/
    validates - kahyad/internal/reader.WorkerCloudModel's own doc
    comment), and maps the outcome onto exactly one terminal protocol
    line + exit code - the SAME protocol/exit-code contract
    _run_session uses, so kahyad/internal/spawn.Run needs no Reader-
    specific parsing of its own."""
    options = _build_reader_options(envelope)

    try:
        async with ClaudeSDKClient(options=options) as client:
            await client.query(envelope.prompt)

            async for message in client.receive_response():
                if isinstance(message, AssistantMessage):
                    for block in message.content:
                        if isinstance(block, TextBlock):
                            _print_protocol_line({"type": "delta", "text": block.text})
                elif isinstance(message, ResultMessage):
                    if message.is_error:
                        wlog.log("error", "reader_model_call_failed", result=str(message.result))
                        _print_protocol_line(
                            {"type": "error", "message": MSG_MODEL_CALL_FAILED_FMT.format(trace_id=envelope.trace_id)}
                        )
                        return 1
    except Exception as e:  # noqa: BLE001 - any SDK/transport failure surfaces as one user-facing error line.
        # W4-04: same cloud_unreachable split as _run_session - see that
        # function's own comment.
        if _is_cloud_unreachable(e):
            wlog.log("error", "reader_cloud_unreachable", error=str(e))
            _print_protocol_line({"event": "cloud_unreachable"})
            return _EXIT_CLOUD_UNREACHABLE
        wlog.log("error", "reader_sdk_error", error=str(e))
        _print_protocol_line(
            {"type": "error", "message": MSG_MODEL_CALL_FAILED_FMT.format(trace_id=envelope.trace_id)}
        )
        return 1

    wlog.log("info", "reader_task_done", task_id=envelope.task_id, schema=envelope.schema)
    _print_protocol_line({"type": "result", "status": "ok"})
    return 0


# --- W6-02: STT mode (toolless, CLOUD-LESS - envelope.mode == "stt").
# Transcribes envelope.input_audio_path with kahya_worker.stt.transcribe
# (a plain, local library call - HANDOFF §4 ⚑ "mlx-whisper ... worker
# içinde kütüphane") and reports the transcript back over the SAME
# stdout protocol every other mode already uses ("delta" + terminal
# "result"/"error") - no ClaudeAgentOptions/ClaudeSDKClient/MCP server is
# ever constructed here, so this function never reaches the network,
# structurally, regardless of what ANTHROPIC_BASE_URL happens to be set
# to (kahyad's own caller deliberately leaves it blank for this mode -
# see kahyad/internal/server's stt.go doc comment). ---


def _tmp_dir() -> str:
    """Returns KAHYA_TMP_DIR (kahyad/internal/spawn.BuildEnv sets this
    from cfg.TmpDir - kahyad's own ``~/Library/Application Support/Kahya/
    tmp/``, created 0700 at kahyad startup), or "" if unset.
    ``_maybe_delete_audio`` treats an unset/blank value as "never delete
    anything" - fail CLOSED towards NOT deleting, never towards deleting
    something it should not."""
    return os.environ.get("KAHYA_TMP_DIR", "")


def _maybe_delete_audio(path: str) -> None:
    """Deletes `path` IFF it resolves to a file directly inside kahyad's
    own KAHYA_TMP_DIR (tasks/w6-voice/W6-02-ptt-whisper.md step 3: "delete
    the file iff it is under ~/Library/Application Support/Kahya/tmp/ -
    never delete a repo fixture or any path outside that tmp dir").
    Resolves BOTH paths (os.path.realpath) before comparing, so a symlink
    cannot be used to point outside the tmp dir while looking like it is
    inside it; the check is "PARENT directory equals the resolved tmp
    dir" (os.path.dirname), never a prefix/startswith string comparison -
    a bare startswith(tmp_dir) would also match an unrelated sibling
    directory that merely shares that prefix (e.g. ".../tmp-evil").
    Best-effort and never raises: a failed delete is logged (warn), never
    allowed to crash an otherwise-successful transcription or change this
    function's own return value."""
    tmp_dir = _tmp_dir()
    if not path or not tmp_dir:
        return
    try:
        real_path = os.path.realpath(path)
        real_tmp_dir = os.path.realpath(tmp_dir)
        if os.path.dirname(real_path) != real_tmp_dir:
            wlog.log("warn", "stt_delete_skipped_outside_tmp", path=path, tmp_dir=tmp_dir)
            return
        os.remove(real_path)
        wlog.log("info", "stt_temp_deleted", path=path)
    except OSError as e:
        wlog.log("warn", "stt_delete_failed", path=path, error=str(e))


def _run_stt_only(envelope: Envelope) -> int:
    """Runs mode="stt" (W6-02, task spec step 3): transcribes
    envelope.input_audio_path entirely locally, emits the ``stt.completed``/
    ``stt.empty`` JSONL event, deletes the temp wav (``_maybe_delete_audio``,
    unconditionally - the recording is consumed whether or not usable text
    came out of it), and reports the outcome via the ordinary stdout
    protocol: a successful non-blank transcript as one "delta" line
    followed by a terminal "result" line (kahyad/internal/server's own
    caller joins "delta" text exactly like kahyad/internal/reader.
    WorkerCloudModel already does for reader mode - no protocol change was
    needed for this task at all); a missing model, an empty/whitespace-only
    transcript, or any other transcription failure as a terminal "error"
    line whose message is kahyad's caller uses VERBATIM as the user-facing
    Turkish text - never wrapped, translated, or paraphrased between here
    and the CLI.
    """
    path = envelope.input_audio_path or ""
    started = time.monotonic()
    try:
        transcript = stt.transcribe(path)
    except stt.SttModelMissingError:
        # Fail-closed (HANDOFF §4/§5): never attempt a network download in
        # response - stt.resolve_model's own doc comment is the actual
        # enforcement; this is just the reporting side of it.
        wlog.log("error", "stt_model_missing", path=path)
        _maybe_delete_audio(path)
        _print_protocol_line({"type": "error", "message": stt.MSG_MODEL_MISSING})
        return 1
    except Exception as e:  # noqa: BLE001 - any other transcription failure (bad/missing wav, mlx_whisper internal error, ...) still surfaces as ONE clean Turkish line, never a raw traceback.
        wlog.log("error", "stt_failed", path=path, error=str(e))
        _maybe_delete_audio(path)
        _print_protocol_line({"type": "error", "message": MSG_STT_FAILED_FMT.format(trace_id=envelope.trace_id)})
        return 1

    duration_ms = int((time.monotonic() - started) * 1000)

    if not transcript.strip():
        # Never submit an empty prompt to the task loop (task spec step 3).
        wlog.log("info", "stt.empty", task_id=envelope.task_id, duration_ms=duration_ms)
        _maybe_delete_audio(path)
        _print_protocol_line({"type": "error", "message": MSG_EMPTY_TRANSCRIPT})
        return 1

    wlog.log(
        "info", "stt.completed", task_id=envelope.task_id,
        chars=len(transcript), duration_ms=duration_ms,
    )
    _maybe_delete_audio(path)
    # The transcript, used verbatim as the eventual user prompt - relayed
    # as an ordinary "delta" line (kahyad/internal/server's caller joins
    # every "delta" it sees into the final transcript string, exactly like
    # kahyad/internal/reader.WorkerCloudModel already does for its own
    # "reader" mode - no bespoke protocol field needed for this task).
    _print_protocol_line({"type": "delta", "text": transcript})
    _print_protocol_line({"type": "result", "status": "ok"})
    return 0


if __name__ == "__main__":
    sys.exit(main())
