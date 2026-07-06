# CLAUDE.md — Kâhya build repo

You are working on **Kâhya**, a local-first personal AI assistant for macOS. The design is complete and locked; your job is to execute the task backlog.

## Protocol (mandatory)

1. Read `tasks/README.md` (conventions + hard rules), then `tasks/BACKLOG.md`.
2. Unless the user told you which task to do: take the FIRST `[ ]` task in BACKLOG.md whose dependencies are all `[x]`, mark it `[~]`, and execute its task file under `tasks/<phase>/`.
3. Before writing code, read the HANDOFF sections cited in the task file's **Handoff refs** line. `docs/HANDOFF.md` is the source of truth and overrides anything else, including this file.
4. A task is done only when every acceptance criterion in its file passes and `make test` is green. Then mark `[x]` in BACKLOG.md and commit as `[<TASK-ID>] <short title>`.
5. If blocked on the user (secrets, TCC dialogs, manual review), mark `[!]` with a one-line note in BACKLOG.md, tell the user exactly what you need, and move to the next unblocked task.

## Hard rules (from HANDOFF §4–§5 — never violate, never renegotiate)

- **Fail-closed everywhere.** Policy check error/timeout = DENY. Memory pressure blocking the local model = secret-lane work refuses, it NEVER falls back to cloud.
- **No byte reaches a cloud model before secret-lane classification has completed locally.** (finans/sağlık/kimlik → local-only, enforced in Go code, not prompts.)
- **kahyad is the ONLY writer of brain.db.** Workers get memory access via the memory MCP tools only.
- **W3-class actions (money · prod · identity · messages as the user) always require written local approval** — at every autonomy level, forever. Telegram may notify but never approve W3.
- `can_use_tool` is an early-reject/UX layer, **not** a security boundary — binding policy decisions live in kahyad; side-effect MCP tools verify a one-time approval token from kahyad.
- The API key is never given to the worker — worker talks through kahyad's localhost forward-proxy (`ANTHROPIC_BASE_URL`).
- All processes log JSONL with a `trace_id` on every line.
- Code, logs, identifiers: **English**. User-facing strings (CLI/bot/notifications): **Turkish**. Preserve Turkish test strings byte-exact (e.g. the `'evlerimizden'` retrieval test).
- Directory names ASCII (`~/Kahya`, never `~/Kâhya`).

## Build/test

`make build` / `make test` / `make lint` (created in W0-02). Go for kahyad + CLI + memory MCP (sqlc + goose); Python (pinned `claude-agent-sdk`) for the worker only.
