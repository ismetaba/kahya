# W4-02 — Task state machine, outbox redelivery, session resume

**Status:** done
**Phase:** W4 — Durability
**Depends on:** W12-07, W12-02
**Flags:** none
**Handoff refs:** §6 W4 ⚑, §4 IPC ⚑

## Goal

Tasks survive crashes. kahyad gets a persisted task state machine with idempotency/receipt
semantics for every side-effectful tool call, an outbox dispatcher that redelivers interrupted
work, and worker session resume via `session_id` — so a SIGKILLed task can be resumed to
completion **without double tool-execution** (the W4-07 gate).

## Context you need

Binding decisions (HANDOFF, quote verbatim):

§6 W4:
> **idempotency/makbuz semantiği** (`intent → executing → receipt`; makbuzsuz `executing`'de yalnız W1 oto-tekrar, W2/W3 asla)

§4 IPC:
> kahyad worker'ı **görev-başına** spawn eder: görev zarfı JSON stdin'den; `trace_id` env/arg ile geçer; W4 oturum devamı `session_id` ile.

- W12-07 delivered per-task worker spawn (envelope JSON on stdin, `trace_id` env/arg, a
  reserved `session_id` field) and the fail-closed `POST /policy/check` UDS endpoint.
  W12-02 delivered the `tasks` and `events/outbox` tables (goose migrations, sqlc queries).
- Action classes (§4): R read-only · W1 undoable · W2 hard-to-undo · W3 irreversible. Class is
  static metadata on tool registrations (policy.yaml, W3-01), checked in Go (W3-02).
- Side-effectful MCP tools are kahyad-owned and verify one-time approval tokens (W3-02). Tool
  execution therefore happens on the kahyad side; a dead worker does not necessarily mean a
  dead tool execution. That is what makes idempotent replay possible.
- kahyad is the single writer of brain.db; the worker reports via its stdout JSONL event stream
  (established in W12-07/W12-09), never by touching the DB.
- Saga compensation executors are explicitly deferred: §8 — "saga telafi-yürütücüsü (kademeli
  yürütme + idempotency/makbuz — §6 W4 — yeterli)". Do not build compensation logic.

## Deliverables

- `kahyad/migrations/<next-goose-seq>_task_durability.sql` — see step 1
- `kahyad/internal/task/machine.go` + `machine_test.go` — state machine + legal transitions
- `kahyad/internal/task/receipts.go` + `receipts_test.go` — tool-call intent/receipt lifecycle
- `kahyad/internal/outbox/dispatcher.go` + `dispatcher_test.go` — lease-based redelivery loop
- `worker/kahya_worker/resume.py` (or equivalent module in the W12-09 harness) — SDK resume
- `kahya task resolve <id> --retry|--abort` and `kahya task show <id>` subcommands in
  `kahyad/cmd/kahya/` (CLI exists since W12-06; W1–2 gate is closed before W4 starts).
  `show` prints status, `session_id`, live worker PID (if any), `attempts`, and the task's
  `tool_calls` rows — the W4-07 gate script kills the worker via this PID.
- Config key `task.retry.w1_max_auto` (default 3) — cap for receipt-less W1 auto-retries
- New sqlc queries for all of the above

## Steps

1. Migration. `tasks` gains: `status TEXT NOT NULL CHECK(status IN
   ('intent','executing','bekliyor-yeniden-deneme','blocked_user','user_halted','done','failed'))`,
   `session_id TEXT`, `next_retry_at`, `attempts INTEGER NOT NULL DEFAULT 0`.
   (`bekliyor-yeniden-deneme` is populated by W4-04; `user_halted` by W6-03 — the enum values
   MUST exist now so those tasks are pure logic.) New table `tool_calls`:
   `id, task_id, seq, tool_name, class TEXT CHECK(class IN ('R','W1','W2','W3')),
   args_hash TEXT, approval_token_id, status TEXT CHECK(status IN
   ('intent','executing','receipt','failed')), receipt_json, started_at, finished_at`,
   unique index on `(task_id, tool_name, args_hash, seq)`. `outbox` gains
   `available_at, lease_until, attempts` if W12-02 did not already create them.
2. State machine (`machine.go`): allowed transitions only —
   `intent→executing`, `executing→{done,failed,blocked_user,bekliyor-yeniden-deneme,user_halted}`,
   `bekliyor-yeniden-deneme→{executing,user_halted}`, `blocked_user→{executing,failed,user_halted}`.
   (The `…→user_halted` edges from parked/blocked states exist so W6-03's `⌥⎋` can halt a task
   that is waiting on retry or on the user — otherwise "permanently excluded from outbox retry"
   is unenforceable and W6-03 stops being pure logic.) Illegal transition ⇒
   error + ledger event `task.illegal_transition`. Every transition appends a ledger event with
   `trace_id`.
3. Receipt lifecycle around every side-effectful (W1/W2/W3) kahyad-owned tool execution:
   insert `tool_calls` row `status=intent` at policy-check time → `executing` when the tool
   starts → `receipt` with `receipt_json` (result + result hash) in the same transaction that
   commits the tool's DB effects, immediately when the side effect completes. R-class calls get
   no rows (no side effects to protect).
4. Idempotent replay: before executing, look up `(task_id, tool_name, args_hash)` with
   `status=receipt`; on hit, return the stored `receipt_json` WITHOUT re-executing and append
   ledger event `tool.replayed`. This is the mechanism that makes resume double-execution-safe.
5. Session capture: worker emits `{"event":"session_started","session_id":...}` on stdout at
   SDK init (W12-09 harness); kahyad persists it to `tasks.session_id`.
6. Resume: on kahyad startup and on a periodic scan (plain `time.Ticker`; switch to the W4-01
   tick API if it has landed), find `tasks.status='executing'` with no live worker PID. For each
   receipt-less `tool_calls` row in `executing`: class `W1` → mark that row `failed`,
   increment `attempts`, requeue via outbox (auto-retry allowed: undoable) — but at most
   `task.retry.w1_max_auto` (default 3) auto-retries per `(task_id, tool_name, args_hash)`;
   past the cap, set task `blocked_user` and notify with exactly this Turkish string:
   `"Görev <id>: '<tool>' aracı <n> kez yarıda kesildi (W1). Otomatik tekrar limiti doldu — 'kahya task resolve <id>' ile karar ver."`
   Class `W2`/`W3` → set task `blocked_user`, notify with exactly this Turkish string:
   `"Görev <id>: '<tool>' aracı yarıda kesildi ve makbuzu yok. W2/W3 sınıfı olduğu için otomatik tekrarlanmadı — 'kahya task resolve <id>' ile karar ver."`
   If no receipt-less side-effectful call exists → requeue for resume directly.
   **Approval tokens are one-time (W3-02) and are NEVER reused on retry:** a replay hit
   (step 4) needs no token because nothing re-executes; any genuine re-execution re-enters
   `/policy/check` and the normal approval flow from scratch.
7. Outbox dispatcher: loop claims due rows (`available_at <= now`, lease with `lease_until`,
   crash-safe re-claim after lease expiry), re-spawns the worker with the W12-07 envelope plus
   `"session_id": <stored>, "resume": true`, marks delivered on worker exit 0. Worker non-zero
   exit that did not report a terminal task state: leave the row unacknowledged — lease expiry
   drives the re-claim, `attempts` increments on each claim. Never redeliver
   tasks in `user_halted` or `blocked_user` (guard checked at claim time, not enqueue time).
8. Worker resume: when envelope has `resume: true`, construct `ClaudeSDKClient` options with
   `resume=<session_id>` (claude-agent-sdk session resume; keep streaming input mode — §4 ⚑:
   hooks and `can_use_tool` do not work with one-shot `query()`).
9. `kahya task resolve <id> --retry` → re-run the interrupted call (fresh approval token via
   the normal W3-02 flow) and requeue; `--abort` → task `failed` + ledger event. Implement
   `kahya task show <id>` here too (status, `session_id`, worker PID, `attempts`, `tool_calls`
   rows). Turkish CLI output.
10. Tests (all in `make test`): legal/illegal transitions (including
    `bekliyor-yeniden-deneme→user_halted` legal, `user_halted→executing` illegal); replay
    returns stored receipt and executes zero times (stub tool with an invocation counter);
    W1 receipt-less auto-retries exactly once more (`attempts=2`); W1 killed past
    `w1_max_auto` ⇒ `blocked_user` + the exact W1-cap Turkish string; W2 receipt-less →
    `blocked_user` + notification event with the exact W2/W3 Turkish string;
    dispatcher lease prevents double-claim under two concurrent dispatchers; envelope for a
    resumed task carries the original `session_id` and `trace_id`.

## Acceptance criteria

- [x] `make test` green including all step-10 tests.
- [x] Integration test: task runs a stub W2 tool whose kahyad-side execution completes while the
      (stub) worker is SIGKILLed mid-call; after dispatcher resume, `SELECT COUNT(*) FROM
      tool_calls WHERE task_id=? AND status='receipt'` = 1, stub side-effect counter = 1, and a
      `tool.replayed` event exists. (This is the CI-speed precursor of the W4-07 gate.)
- [x] Integration test: same scenario but the tool never wrote a receipt → task row is
      `blocked_user`, notification event payload contains the exact Turkish string from step 6,
      and `kahya task resolve <id> --abort` moves it to `failed`.
- [x] Grep test: every task/tool state transition ledger event carries the task's `trace_id`
      (JSONL log + events rows agree).
- [x] `sqlite3 brain.db "PRAGMA foreign_key_check;"` clean after migration; goose up/down/up
      passes on an empty DB.

## Out of scope

- Cloud-call retry taxonomy and `bekliyor-yeniden-deneme` population — W4-04 (enum value only
  is created here).
- `user_halted` semantics (kill process-group, invalidate approvals) — W6-03; only the enum
  value and the "never redeliver" dispatcher guard exist here.
- Saga compensation executor and embedded NATS — HANDOFF §8 deferred.
- Approval-token issuance/verification itself — W3-02 (consumed, not modified).
- Taint checks on resume — W4-03 (it plugs into `/policy/check`, not into this machine).
