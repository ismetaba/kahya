"""kahya_worker — the per-task Python worker harness (W12-09, HANDOFF §4
IPC ⚑): reads one task envelope from stdin, runs a single
``claude_agent_sdk.ClaudeSDKClient`` streaming-input session with the
``UserPromptSubmit`` memory-injection hook and the ``can_use_tool``
early-reject callback wired in, and speaks the frozen worker stdout
protocol (``docs/ipc.md`` §4) back to kahyad.

This package is invoked as ``python -m kahya_worker`` (see
``kahyad/internal/config``'s ``defaultWorkerCmd``) - see
``kahya_worker.__main__`` for the actual entrypoint.
"""
