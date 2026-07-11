"""Envelope v1 parsing/validation (``docs/ipc.md`` §2): the single JSON
object kahyad writes to the worker's stdin, then closes stdin. That file
is the frozen deliverable ("IPC sözleşmesi") - this module must match it
exactly, not the other way around.

Field rules mirror ``kahyad/internal/spawn.Envelope.Validate`` (Go) one
for one: an unknown ``schema_version``, a missing/wrong-typed field, an
unsupported ``kind``, a blank ``task_id``/``trace_id``/``prompt``, or a
``model`` outside the HANDOFF §9 cloud set are all rejected the same way
Go's side rejects them - the worker never accepts an envelope Go itself
would refuse to construct.
"""

from __future__ import annotations

import json
from dataclasses import dataclass

# Envelope v1's fixed schema_version (docs/ipc.md §2). Bump only alongside
# a documented, backward-compatible migration plan - never silently.
SCHEMA_VERSION = 1

# HANDOFF §9's cloud model set. The worker NEVER chooses or changes the
# model - "Model yönlendirme (karar Go kodunda, istemde değil)" - it only
# validates that the envelope's model is one Go could legitimately have
# routed to, exactly mirroring kahyad/internal/spawn.AllowedModels.
ALLOWED_MODELS = frozenset(
    {
        "claude-opus-4-8",
        "claude-sonnet-5",
        "claude-haiku-4-5",
        "claude-fable-5",
    }
)

# Every field docs/ipc.md §2's envelope table fixes. session_id is
# included here even though it may be JSON null - "missing field" and
# "field present but null" are different failures (Validate below reports
# each with a distinct, specific message).
_REQUIRED_FIELDS = (
    "schema_version",
    "task_id",
    "trace_id",
    "session_id",
    "kind",
    "prompt",
    "model",
    "memory_injection",
    "created_at",
)


class EnvelopeError(Exception):
    """Raised for ANY envelope validation failure. The caller
    (``kahya_worker.__main__``) turns every ``EnvelopeError`` into the one
    fixed Turkish stdout line (``"Görev zarfı geçersiz."``) + exit 2 -
    this exception's own message is English-only diagnostic detail that
    goes to ``worker.jsonl``, never to the user (HANDOFF §3 language
    policy)."""


@dataclass(frozen=True)
class Envelope:
    """A validated envelope v1 object - by the time one of these exists,
    every field has already passed ``parse_envelope``'s checks."""

    schema_version: int
    task_id: str
    trace_id: str
    session_id: str | None
    kind: str
    prompt: str
    model: str
    memory_injection: bool
    created_at: str
    # resume (W4-02, docs/ipc.md's own W4-02 note): true iff kahyad/
    # internal/outbox.Dispatcher is re-spawning this worker for a task
    # that already has a persisted session_id - kahya_worker.__main__.
    # _build_options then constructs ClaudeAgentOptions with
    # resume=session_id instead of starting a fresh conversation. Optional
    # on the wire (omitted, via Go's `omitempty`, whenever false - see
    # parse_envelope below); defaults to False so every pre-W4-02
    # envelope/test still parses unchanged.
    resume: bool = False


def parse_envelope(raw: bytes) -> Envelope:
    """Parses and validates raw stdin bytes into an `Envelope`, raising
    `EnvelopeError` on the first problem found: invalid JSON, a
    non-object body, a missing field, a wrong-typed field, an unknown
    `schema_version`, `kind != "chat"`, a blank `task_id`/`trace_id`/
    `prompt`, or a `model` outside `ALLOWED_MODELS`."""
    try:
        obj = json.loads(raw)
    except json.JSONDecodeError as e:
        raise EnvelopeError(f"invalid JSON: {e}") from e

    if not isinstance(obj, dict):
        raise EnvelopeError(f"envelope must be a JSON object, got {type(obj).__name__}")

    missing = [f for f in _REQUIRED_FIELDS if f not in obj]
    if missing:
        raise EnvelopeError(f"missing field(s): {', '.join(missing)}")

    schema_version = obj["schema_version"]
    if schema_version != SCHEMA_VERSION:
        raise EnvelopeError(
            f"schema_version = {schema_version!r}, want {SCHEMA_VERSION}"
        )

    task_id = obj["task_id"]
    if not isinstance(task_id, str) or not task_id.strip():
        raise EnvelopeError("task_id must be a non-blank string")

    trace_id = obj["trace_id"]
    if not isinstance(trace_id, str) or not trace_id.strip():
        raise EnvelopeError("trace_id must be a non-blank string")

    session_id = obj["session_id"]
    if session_id is not None and not isinstance(session_id, str):
        raise EnvelopeError("session_id must be a string or null")

    kind = obj["kind"]
    if kind != "chat":
        raise EnvelopeError(f'kind = {kind!r}, want "chat"')

    prompt = obj["prompt"]
    if not isinstance(prompt, str) or not prompt.strip():
        raise EnvelopeError("prompt must be a non-blank string")

    model = obj["model"]
    if not isinstance(model, str) or model not in ALLOWED_MODELS:
        raise EnvelopeError(
            f"model = {model!r} not in the HANDOFF §9 cloud model set"
        )

    memory_injection = obj["memory_injection"]
    if not isinstance(memory_injection, bool):
        raise EnvelopeError("memory_injection must be a boolean")

    created_at = obj["created_at"]
    if not isinstance(created_at, str) or not created_at.strip():
        raise EnvelopeError("created_at must be a non-blank string")

    # resume (W4-02): optional on the wire, like lane/category - absent
    # means False (a fresh, non-resumed spawn), mirroring Go's
    # `omitempty`/zero-value convention exactly.
    resume = obj.get("resume", False)
    if not isinstance(resume, bool):
        raise EnvelopeError("resume must be a boolean")
    if resume and (session_id is None or not session_id.strip()):
        raise EnvelopeError("resume = true requires a non-empty session_id")

    return Envelope(
        schema_version=schema_version,
        task_id=task_id,
        trace_id=trace_id,
        session_id=session_id,
        kind=kind,
        prompt=prompt,
        model=model,
        memory_injection=memory_injection,
        created_at=created_at,
        resume=resume,
    )
