"""JSONL logging for the worker process (HANDOFF §4 IPC ⚑: "Tüm süreçler
her satırda trace_id içeren JSONL loglar"). Every line this module writes
goes to ``$KAHYA_LOG_DIR/worker.jsonl``, one JSON object per line,
carrying ``trace_id`` (configured from the ``KAHYA_TRACE_ID`` env var by
``__main__``) on EVERY line - this is the one place that invariant is
enforced for the worker process.

Diagnostics only: stdout is reserved exclusively for the protocol lines
``docs/ipc.md`` §4 defines (see ``kahya_worker.__main__``); this module
never writes to stdout. Log message text is English (HANDOFF §3 language
policy - code/logs in English, only user-facing strings in Turkish).
"""

from __future__ import annotations

import json
import os
import sys
import threading
from datetime import datetime, timezone
from typing import Any

_lock = threading.Lock()
_log_path: str | None = None
_trace_id: str = ""


def configure(log_dir: str, trace_id: str) -> None:
    """Points subsequent `log()` calls at `<log_dir>/worker.jsonl`,
    tagging every line with `trace_id`. Safe to call more than once (each
    test case reconfigures independently); creates `log_dir` if it does
    not exist yet."""
    global _log_path, _trace_id
    os.makedirs(log_dir, exist_ok=True)
    with _lock:
        _log_path = os.path.join(log_dir, "worker.jsonl")
        _trace_id = trace_id


def log(level: str, event: str, **fields: Any) -> None:
    """Appends one JSONL line: `{"ts","level","event","trace_id",
    ...fields}`. Never raises - a logging failure must never crash the
    worker or leak onto stdout (docs/ipc.md §4: stdout carries ONLY
    protocol lines); falls back to stderr if `configure()` was never
    called or the log file can't be opened/written."""
    line: dict[str, Any] = {
        "ts": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%fZ"),
        "level": level,
        "event": event,
        "trace_id": _trace_id,
    }
    line.update(fields)

    try:
        text = json.dumps(line, ensure_ascii=False)
    except (TypeError, ValueError):
        # A field failed to JSON-encode (e.g. a non-serializable object
        # passed by mistake) - degrade to str(...) on every extra field
        # rather than lose the log line entirely.
        safe = {k: (v if k in ("ts", "level", "event", "trace_id") else str(v)) for k, v in line.items()}
        text = json.dumps(safe, ensure_ascii=False)

    with _lock:
        path = _log_path

    try:
        if path:
            with open(path, "a", encoding="utf-8") as f:
                f.write(text + "\n")
        else:
            print(text, file=sys.stderr)
    except OSError:
        print(text, file=sys.stderr)
