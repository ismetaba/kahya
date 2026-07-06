# Kâhya — Task Backlog Protocol

This directory is the **complete build plan** for the Kâhya MVP (the 8-week critical path in `docs/HANDOFF.md` §6). It is written so that any competent agent — human or LLM, with no access to any prior conversation — can pick up the next task and execute it.

## Sources of truth, in order

1. **`docs/HANDOFF.md`** — the locked design document. §4 decisions are closed for debate. §5 invariants must never be violated by any task. If a task file ever contradicts the handoff, **the handoff wins** — fix the task file in the same commit.
2. **`tasks/BACKLOG.md`** — the ordered index: every task, its dependencies, flags, and status.
3. **The task file** — one markdown file per task under `tasks/<phase>/`.

## How to pick and run a task

1. Open `tasks/BACKLOG.md`. Take the **first** task marked `[ ]` whose dependencies are all `[x]`.
2. Mark it `[~]` (in progress) in BACKLOG.md.
3. Open its task file. Read the HANDOFF sections listed under **Handoff refs** *before* writing any code.
4. Do the work following **Steps**. Stay inside **Deliverables**; respect **Out of scope**.
5. Verify **every** acceptance criterion. Run `make test` (must be green) and `make lint`.
6. Set the task file's `Status:` to `done`, mark `[x]` in BACKLOG.md, and commit everything as:
   `[<TASK-ID>] <short imperative title>` (e.g. `[W12-03] add FTS5 dual index with BM25 fusion`).
7. If you cannot proceed without the user (a secret, a TCC permission dialog, a manual review, a remote URL), set `Status: blocked-user`, mark `[!]` in BACKLOG.md with a one-line reason, tell the user **exactly** what you need from them, and move on to the next unblocked task.

Never work two tasks in one commit. Never skip acceptance criteria "because it obviously works".

## Global conventions (apply to every task)

- **Fail-closed is the default posture.** Any policy/permission/classification error or timeout results in DENY / refuse, never in a permissive fallback. Specifically: secret-lane (finans/sağlık/kimlik) work NEVER falls back to a cloud model — not on memory pressure, not on model-load failure, not on timeout.
- **kahyad is the single writer of `brain.db`.** Everything else reads/writes memory through the memory MCP tools.
- **Memory source of truth is Markdown + git** (`~/Kahya/memory/*.md`); SQLite is a derived index that can always be rebuilt from it. The ledger (`events`) and `episodes` are the exception — they exist only in brain.db, which is why backups (W4-06) are non-negotiable.
- **Every log line is JSONL and carries `trace_id`.** A `trace_id` is minted when a task/command enters the system and is propagated to worker, MLX helpers, tools, and ledger events. Acceptance criteria that say "single trace_id" are verified by grepping the JSONL logs.
- **Language:** code, identifiers, logs, commit messages in English. User-facing strings (CLI output, Telegram messages, notifications) in Turkish. Turkish test fixtures are byte-exact — do not "translate" or ASCII-fold them (`'evlerimizden'` must match a note containing `'ev'` via the trigram index, not via manual stemming).
- **Paths:** code = this repo (`~/code/kahya`); memory = `~/Kahya` (separate git repo); index/ledger = `~/Library/Application Support/Kahya/brain.db`; secrets = macOS Keychain only. Directory names ASCII.
- **Versions are pinned:** `claude-agent-sdk` (exact version + lock file), `sqlite-vec` ≥0.1.9 pinned, Go deps via go.mod. Model IDs come from HANDOFF §9 — never invent new ones.
- **Autonomy/action classes:** R (read-only), W1 (undoable write), W2 (hard-to-undo write), W3 (irreversible: money · prod · identity · messaging as the user). W3 always requires written local approval. These are static metadata on tool registrations, enforced in Go before the tool runs.
- When a task says "test", write a real automated test that lives in the repo and runs in `make test` — not a one-off manual check. §5 invariants get permanent regression tests (collected in CI by W78-03).

## Task file template

Every task file follows this structure:

```markdown
# <ID> — <Title>

**Status:** todo | in-progress | blocked-user | done
**Phase:** <phase name>
**Depends on:** <task IDs or "none">
**Flags:** <user-assist / long-running / slidable / none>
**Handoff refs:** <§ sections to read first>

## Goal
One short paragraph: what exists after this task that didn't before.

## Context you need
Self-contained background: the relevant handoff constraints (quote ⚑ items verbatim
where they bind this task), prior tasks' outputs you build on, and gotchas.

## Deliverables
Exact files/artifacts created or modified.

## Steps
Numbered, concrete, in execution order. A basic agent must be able to follow them
without inventing design decisions.

## Acceptance criteria
Checkbox list. Each item is objectively checkable (a command to run, a log line to
find, a test that must pass). These mirror/refine the §6 weekly acceptance gates.

## Out of scope
What NOT to build here (usually: things deferred by HANDOFF §8 or owned by a later task).
```

## Flags legend

- **user-assist** 🧍 — needs the user for a secret, account, review, or macOS permission dialog. Do everything you can, then block cleanly.
- **long-running** ⏳ — involves large downloads or long jobs; run in background where possible and verify afterward.
- **slidable** ↔ — HANDOFF §6 explicitly allows deferring this task to a later week if the schedule is tight. It still must be done before W7-8 acceptance.
