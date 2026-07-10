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
  is kahyad's own ``/policy/check``, which is itself fail-closed by
  construction - HANDOFF §5 ⚑), but this callback still fails CLOSED
  (deny) on any error, exactly mirroring kahyad's own posture rather than
  quietly allowing a tool call whenever the check itself couldn't be
  completed.
"""

from __future__ import annotations

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
            resp = post_json(
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
    ``{trace_id,task_id,session_id,tool_name,tool_input}`` over UDS (5s
    budget), passing ``tool_name`` VERBATIM as the SDK delivers it
    (SDK-prefixed for MCP tools, e.g.
    ``"mcp__kahya_memory__memory_search"`` - kahyad canonicalizes this
    worker never rewrites it). ANY failure - exception, timeout, non-200,
    or an unparsable/garbage response body - is a DENY with
    ``POLICY_FAIL_CLOSED_MESSAGE``. Every decision (allow, explicit deny,
    or fail-closed deny) is logged as ``event=tool_gate`` with the tool
    name, decision, and ``duration_ms``.
    """

    async def can_use_tool(tool_name: str, tool_input: dict[str, Any], ctx: Any) -> Any:
        start = time.monotonic()
        try:
            resp = post_json(
                socket_path,
                POLICY_CHECK_PATH,
                {
                    "trace_id": trace_id,
                    "task_id": task_id,
                    "session_id": session_id,
                    "tool_name": tool_name,
                    "tool_input": tool_input,
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
            wlog.log("info", "tool_gate", tool=tool_name, decision="allow", duration_ms=duration_ms)
            return PermissionResultAllow()

        if decision == "deny":
            reason = resp.get("reason") or POLICY_FAIL_CLOSED_MESSAGE
            wlog.log(
                "info", "tool_gate", tool=tool_name, decision="deny",
                duration_ms=duration_ms, reason=reason,
            )
            return PermissionResultDeny(message=reason)

        # Any other/garbage "decision" value (missing, misspelled, wrong
        # type): fail-closed, exactly like a transport-level failure.
        wlog.log(
            "info", "tool_gate", tool=tool_name, decision="deny",
            duration_ms=duration_ms, reason=POLICY_FAIL_CLOSED_MESSAGE,
            garbage_response=True,
        )
        return PermissionResultDeny(message=POLICY_FAIL_CLOSED_MESSAGE)

    return can_use_tool
