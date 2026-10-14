"""``UserPromptSubmit`` hook + ``can_use_tool`` callback (HANDOFF §4 ⚑ IPC
/ §5 ⚑ safety #4). Both talk to kahyad exclusively over UDS via
``kahya_worker.udshttp`` - never any other transport, never directly to
any cloud endpoint.

The two callbacks have DELIBERATELY DIFFERENT failure postures:

- Memory search (``make_user_prompt_submit_hook``) is an *enrichment*, not
  a security gate: a search error/timeout logs a warning and the turn
  proceeds WITHOUT injection - it never fails the task.
- The policy check (``make_can_use_tool``) is a *security* decision:
  ``can_use_tool`` is only an early-reject/UX layer (the binding decision
  is kahyad's own tool-side gate on ``/v1/mcp`` - HANDOFF §5 ⚑), but this
  callback still fails CLOSED (deny) on any error, exactly mirroring
  kahyad's own posture rather than quietly allowing a tool call whenever
  the check itself couldn't be completed.

B-1: every ``/policy/check`` this worker sends is a PREFLIGHT
(``"preflight": true``): kahyad decides and ledgers but mints nothing.
``needs_approval`` for one of kahyad's own MCP tools
(``mcp__kahya_memory__*``) is DEFERRED (allowed through) to kahyad's
binding gate, which parks the call for the human decision and executes
exactly the approved bytes - the only pending approval a human ever sees
is that gate's. ``needs_approval`` for any other tool (an SDK built-in,
which has no kahyad-side executor) is a fail-closed deny.

UDS calls run in a worker thread (``asyncio.to_thread``) so the SDK's event
loop keeps draining stdout/control messages while kahyad answers
(FIX-I-16): ``udshttp.post_json`` is deliberately stdlib-only and
synchronous, and calling it directly inside these ``async`` callbacks froze
the whole loop for up to the 5 s budget (serializing parallel tool checks
and stalling the SDK's reader). Only ``post_json`` runs off-loop; every
decision, log line and the fail-closed handling stay on the loop, after the
``to_thread`` return - ``to_thread`` re-raises ``UDSHTTPError`` unchanged.
"""

from __future__ import annotations

import asyncio
import time
from typing import Any, Awaitable, Callable

from claude_agent_sdk import PermissionResultAllow, PermissionResultDeny

from . import logging as wlog
from .udshttp import UDSHTTPError, post_json

MEMORY_SEARCH_PATH = "/v1/memory/search"
POLICY_CHECK_PATH = "/policy/check"

# Both budgets are fixed at 5s by HANDOFF §4 IPC ⚑ ("timeout 5s").
MEMORY_SEARCH_TIMEOUT_S = 5.0
POLICY_CHECK_TIMEOUT_S = 5.0

# docs/ipc.md §3/W12-09 task spec: k=6 for injection search.
MEMORY_SEARCH_K = 6

# HANDOFF §4/§5 ⚑: ANY can_use_tool failure - exception, timeout, non-200,
# unparsable/garbage body - is a DENY with this exact Turkish message.
POLICY_FAIL_CLOSED_MESSAGE = "Politika kontrolü başarısız — reddedildi (fail-closed)."

# B-1: can_use_tool is an early-reject/UX layer (HANDOFF §5 ⚑). Every
# /policy/check it sends is a PREFLIGHT: kahyad decides and ledgers but
# mints nothing - the binding decision (and the only pending approval the
# human ever sees) is minted by kahyad's own tool-side gate on /v1/mcp.
POLICY_CHECK_PREFLIGHT = True
# Tools served by kahyad's own MCP bridge (__main__._build_options registers
# the stdio server as "kahya_memory"): a needs_approval for one of these is
# DEFERRED to that binding gate, which parks the call for the human
# decision and executes exactly the approved bytes.
KAHYA_MCP_TOOL_PREFIX = "mcp__kahya_memory__"
# A needs_approval for any OTHER tool (an SDK built-in) has no kahyad-side
# executor to defer to: fail closed, with this exact Turkish message.
POLICY_NEEDS_APPROVAL_NON_MCP_MESSAGE = "Bu araç için onay gerekiyor ancak bu yüzeyden onaylanamaz (reddedildi)."


def make_user_prompt_submit_hook(
    socket_path: str,
    task_id: str,
    trace_id: str,
    memory_injection: bool,
) -> Callable[[dict[str, Any], str | None, Any], Awaitable[dict[str, Any]]]:
    """Returns a ``UserPromptSubmit`` hook callback (the
    ``claude_agent_sdk.HookCallback`` shape). When ``memory_injection`` is
    true, POSTs ``/v1/memory/search``
    ``{"query","k":6,"for_injection":true,"task_id","trace_id"}`` over UDS
    (5s budget). A non-empty ``hafiza_block`` in the response is returned
    as ``additionalContext`` BYTE-EXACT AND UNMODIFIED (HANDOFF §5 safety
    #4: kahyad already ledgered exactly these bytes as ``hafiza_injected``
    - any local mutation here would break that forensic equality). A
    search error/timeout is logged at ``warn``
    (``event=memory_search_failed``) and the hook returns no
    ``additionalContext`` - memory search is enrichment, not a security
    gate, so the turn proceeds without injection rather than failing the
    task.
    """

    async def user_prompt_submit(
        input_data: dict[str, Any], tool_use_id: str | None, context: Any
    ) -> dict[str, Any]:
        if not memory_injection:
            return {}

        prompt = input_data.get("prompt", "")
        try:
            # FIX-I-16: off the event loop (see the module docstring).
            resp = await asyncio.to_thread(
                post_json,
                socket_path,
                MEMORY_SEARCH_PATH,
                {
                    "query": prompt,
                    "k": MEMORY_SEARCH_K,
                    "for_injection": True,
                    "task_id": task_id,
                    "trace_id": trace_id,
                },
                timeout=MEMORY_SEARCH_TIMEOUT_S,
            )
        except UDSHTTPError as e:
            wlog.log("warn", "memory_search_failed", task_id=task_id, error=str(e))
            return {}

        block = resp.get("hafiza_block", "")
        if not isinstance(block, str) or block == "":
            return {}

        return {
            "hookSpecificOutput": {
                "hookEventName": "UserPromptSubmit",
                # BYTE-EXACT, UNMODIFIED - do not strip/reformat/translate.
                "additionalContext": block,
            }
        }

    return user_prompt_submit


def make_can_use_tool(
    socket_path: str,
    task_id: str,
    trace_id: str,
    session_id: str | None,
) -> Callable[[str, dict[str, Any], Any], Awaitable[Any]]:
    """Returns a ``can_use_tool`` callback (the
    ``claude_agent_sdk.CanUseTool`` shape). POSTs ``/policy/check``
    ``{trace_id,task_id,session_id,tool_name,tool_input,preflight}`` over
    UDS (5s budget; ``preflight`` is always ``POLICY_CHECK_PREFLIGHT`` -
    B-1), passing ``tool_name`` VERBATIM as the SDK delivers it
    (SDK-prefixed for MCP tools, e.g.
    ``"mcp__kahya_memory__memory_search"`` - kahyad canonicalizes this
    worker never rewrites it). ANY failure - exception, timeout, non-200,
    an unparsable/garbage response body, or a self-contradictory body
    (``{"decision":"allow","error":...}`` - MINOR 5 fix) - is a DENY with
    ``POLICY_FAIL_CLOSED_MESSAGE``. ``needs_approval`` (B-1) is deferred
    (allowed, logged ``decision=defer``) for a ``mcp__kahya_memory__*`` tool
    - kahyad's binding gate then waits for the human - and denied with
    ``POLICY_NEEDS_APPROVAL_NON_MCP_MESSAGE`` for any other tool. Every
    decision (allow, defer, explicit deny, or fail-closed deny) is logged as
    ``event=tool_gate`` with the tool name, decision, and ``duration_ms``.
    """

    async def can_use_tool(tool_name: str, tool_input: dict[str, Any], ctx: Any) -> Any:
        start = time.monotonic()
        try:
            # FIX-I-16: off the event loop; the needs_approval/allow/deny
            # handling below runs back on the loop (G00 C-6).
            resp = await asyncio.to_thread(
                post_json,
                socket_path,
                POLICY_CHECK_PATH,
                {
                    "trace_id": trace_id,
                    "task_id": task_id,
                    "session_id": session_id,
                    "tool_name": tool_name,
                    "tool_input": tool_input,
                    "preflight": POLICY_CHECK_PREFLIGHT,
                },
                timeout=POLICY_CHECK_TIMEOUT_S,
            )
        except UDSHTTPError as e:
            duration_ms = int((time.monotonic() - start) * 1000)
            wlog.log(
                "info",
                "tool_gate",
                tool=tool_name,
                decision="deny",
                duration_ms=duration_ms,
                reason=POLICY_FAIL_CLOSED_MESSAGE,
                error=str(e),
            )
            return PermissionResultDeny(message=POLICY_FAIL_CLOSED_MESSAGE)

        duration_ms = int((time.monotonic() - start) * 1000)
        decision = resp.get("decision")

        if decision == "allow":
            # MINOR 5 fix: a 200 body of {"decision":"allow","error":"..."}
            # is self-contradictory - an "error" field alongside "allow"
            # means kahyad itself hit a problem forming its response, not
            # that it deliberately allowed the call. Treat this the same as
            # any other malformed/garbage response: fail-closed DENY,
            # never a trusting ALLOW.
            if "error" in resp:
                wlog.log(
                    "info", "tool_gate", tool=tool_name, decision="deny",
                    duration_ms=duration_ms, reason=POLICY_FAIL_CLOSED_MESSAGE,
                    garbage_response=True, server_error=str(resp.get("error")),
                )
                return PermissionResultDeny(message=POLICY_FAIL_CLOSED_MESSAGE)
            wlog.log("info", "tool_gate", tool=tool_name, decision="allow", duration_ms=duration_ms)
            return PermissionResultAllow()

        if decision == "deny":
            reason = resp.get("reason") or POLICY_FAIL_CLOSED_MESSAGE
            wlog.log(
                "info", "tool_gate", tool=tool_name, decision="deny",
                duration_ms=duration_ms, reason=reason,
            )
            return PermissionResultDeny(message=reason)

        if decision == "needs_approval":
            # Self-contradictory body (an "error" alongside the decision):
            # same fail-closed posture as the allow branch above.
            if "error" in resp:
                wlog.log(
                    "info", "tool_gate", tool=tool_name, decision="deny",
                    duration_ms=duration_ms, reason=POLICY_FAIL_CLOSED_MESSAGE,
                    garbage_response=True, server_error=str(resp.get("error")),
                )
                return PermissionResultDeny(message=POLICY_FAIL_CLOSED_MESSAGE)
            if tool_name.startswith(KAHYA_MCP_TOOL_PREFIX):
                # Deferred to kahyad's binding gate on /v1/mcp: it mints the
                # pending approval, parks this very call until the human
                # decides, and executes exactly the approved bytes (or
                # returns the Turkish refusal as the tool result).
                wlog.log(
                    "info", "tool_gate", tool=tool_name, decision="defer",
                    duration_ms=duration_ms, reason=resp.get("reason") or "",
                )
                return PermissionResultAllow()
            wlog.log(
                "info", "tool_gate", tool=tool_name, decision="deny",
                duration_ms=duration_ms, reason=POLICY_NEEDS_APPROVAL_NON_MCP_MESSAGE,
            )
            return PermissionResultDeny(message=POLICY_NEEDS_APPROVAL_NON_MCP_MESSAGE)

        # Any other/garbage "decision" value (missing, misspelled, wrong
        # type): fail-closed, exactly like a transport-level failure.
        wlog.log(
            "info", "tool_gate", tool=tool_name, decision="deny",
            duration_ms=duration_ms, reason=POLICY_FAIL_CLOSED_MESSAGE,
            garbage_response=True,
        )
        return PermissionResultDeny(message=POLICY_FAIL_CLOSED_MESSAGE)

    return can_use_tool
