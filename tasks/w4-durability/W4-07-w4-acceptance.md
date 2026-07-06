# W4-07 — W4 acceptance gate: kill-resume, offline, tamper

**Status:** todo
**Phase:** W4 — Durability
**Depends on:** W4-01, W4-02, W4-03, W4-04, W4-05, W4-06
**Flags:** none
**Handoff refs:** §6 W4 acceptance, §6 W7–8 (dev profile), tasks/README.md gate rule

## Goal

The three W4 gate scenarios from HANDOFF §6 pass, as scripted, repeatable runs: (A) a long
W2-calling task SIGKILLed mid-tool-call resumes to completion without double execution,
(B) a command issued offline completes on reconnect or fails with an explicit notification,
(C) local ledger tampering is detected against the remote anchor and alarms. A CI-speed
variant runs in `make test`; one real-time run is executed and its evidence recorded in the
ledger. W5 must not start until this gate is `[x]`.

## Context you need

The gate (HANDOFF §6 W4, quote verbatim):

> → **Kabul:** ≥10 dk süren, en az bir W2 aracı çağıran görev bir araç çağrısı ortasında SIGKILL edilir; resume sonrası **çift araç-yürütmesi olmadan** (outbox/defter kayıtlarıyla doğrulanır) tamamlanıyor; ağ kapalıyken verilen komut ağ dönünce tamamlanıyor ya da açık hata bildirimiyle kapanıyor; defter yerelden değiştirilince uzak çapayla uyumsuzluk tespit edilip alarm veriyor.

- How scenario A can pass at all: W2 tools execute on the kahyad side (W3-02 approval tokens,
  W4-02 receipts). SIGKILLing the *worker* mid-call lets kahyad finish the tool and write the
  receipt; on resume the replayed call is answered from the receipt (`tool.replayed`), so the
  side effect happens exactly once. The receipt-less variant (tool truly interrupted) must
  BLOCK for W2, not silently retry — that path is covered by W4-02's tests and is NOT what
  this gate exercises.
- Scenario B rides W4-04 (`bekliyor-yeniden-deneme` + outbox redelivery). Scenario C rides
  W4-05 (`kahya ledger verify`).
- Run gate scenarios under the dev profile so the real brain.db/memory are never touched:
  `KAHYA_ENV=dev` (separate brain.db + `~/Kahya-dev/memory` + separate launchd label — §6
  W7–8 ⚑ defines the profile; implement the minimal env-switch here if W12-01's config does
  not already support it, and leave egress-deny-all/record-replay to W78-02).
- README rule: acceptance criteria are commands/tests/log-lines — every check below states its
  verification command.

## Deliverables

- `tests/acceptance/w4/w4_gate_test.go` — CI-speed versions of A/B/C (fake clock, stub tools,
  seconds not minutes), tagged `//go:build acceptance`. **`make test` must be extended to run
  `go test -tags acceptance ./tests/acceptance/...`** — a tagged file that plain `go test`
  silently skips would make this gate vacuously green; add a Makefile assertion (the target
  fails if the acceptance package reports `[no test files]`)
- `scripts/accept_w4.sh` — orchestrated real-daemon run of A/B/C under `KAHYA_ENV=dev`
- `make accept-w4` target (runs the script; used once for the real-time evidence run)
- Dev-profile stub W2 tool `w2_slow_stub` registered ONLY in the dev policy.yaml overlay
  (sleeps a configurable duration, then appends one line to a counter file — the "side effect")
- Minimal `KAHYA_ENV=dev` plumbing if not already present (paths: `~/Library/Application
  Support/Kahya-dev/brain.db`, `~/Kahya-dev/memory`, socket `kahyad-dev.sock`)

## Steps

1. Dev profile plumbing: `KAHYA_ENV=dev` switches DB path, memory root, socket path, launchd
   label suffix `-dev`. Refuse to start dev kahyad if it would open the prod brain.db path.
2. Stub tool: `w2_slow_stub(duration, counter_file)` — declared `class: W2`,
   `reversible: false` in the dev overlay; execution = sleep, then append one line to
   `counter_file`, then receipt. Approval satisfied via the normal W3-02 flow (pre-approve in
   the script through the CLI so the run is unattended).
3. Scenario A (script): start dev kahyad; submit a task whose plan calls `w2_slow_stub`
   (duration 90s in script / ≥10 min in the real run via `W4_REAL=1` env switching duration
   and adding filler steps); wait until the tool call row is `status='executing'`; `kill -9`
   the worker PID (kahyad exposes it via `kahya task show <id>`); wait for outbox resume;
   assert: task `done`; `wc -l counter_file` == 1;
   `sqlite3 dev-brain.db "SELECT COUNT(*) FROM tool_calls WHERE task_id=? AND tool_name='w2_slow_stub' AND status='receipt';"` == 1;
   a `tool.replayed` event exists; all events share the task's `trace_id`.
4. Scenario B (script): reconfigure dev proxy upstream to `127.0.0.1:1` (blackhole); submit a
   task needing a cloud call; assert task reaches `bekliyor-yeniden-deneme` and the parked
   Turkish notification event exists (exact string from W4-04); flip upstream to the fake
   healthy responder from W4-04's test fixtures; assert task reaches `done`. Then the failure
   leg: blackhole + fake clock past `give_up_after` (CI test only) ⇒ `failed` + give-up string.
5. Scenario C (script): with ≥2 anchors pushed to a local `file://` bare anchor remote,
   run `sqlite3 dev-brain.db "UPDATE events SET payload=json_set(payload,'$.k','tampered') WHERE id=(SELECT MIN(id) FROM events);"`;
   run `kahya ledger verify` ⇒ non-zero exit; assert `anchor.mismatch` event + alarm string
   `DEFTER UYARISI: yerel defter uzak çapayla uyuşmuyor (event <id>). Olası kurcalama — hemen incele.`
6. CI-speed variants in `w4_gate_test.go` mirror 3–5 with stub worker + fake clock so the
   whole file runs in <60s inside `make test`.
7. Real-time evidence run: `W4_REAL=1 make accept-w4` once — the stub W2 task genuinely runs
   ≥10 minutes before the mid-call SIGKILL. The script appends a ledger event
   `accept.w4_real_run` with `{duration_s, scenario_results}`; keep the run log under
   `~/Library/Logs/Kahya/accept-w4-<date>.log`.
8. On green: set this file's Status to done, mark `[x]` in BACKLOG.md, commit
   `[W4-07] pass W4 durability acceptance gate`. Do not start any W5 task before this point
   (BACKLOG gate rule; ↔ exceptions do not exist in W4).

## Acceptance criteria

- [ ] `make test` green, including `tests/acceptance/w4` (CI-speed A/B/C).
- [ ] `make accept-w4` (script, dev profile) exits 0 with all three scenarios reported PASS.
- [ ] Scenario A evidence: `wc -l <counter_file>` prints `1`; the receipt-count SQL above
      prints `1`; `kahya log --trace <id>` shows spawn → SIGKILL gap → resume → `tool.replayed`
      → `done` under one `trace_id`.
- [ ] Scenario B evidence: events contain `task.waiting_retry` with the exact parked string,
      then `task.done` after upstream restore — verified by
      `sqlite3 dev-brain.db "SELECT kind FROM events WHERE trace_id=? ORDER BY id;"`.
- [ ] Scenario C evidence: `kahya ledger verify; echo $?` prints a non-zero code and the exact
      `DEFTER UYARISI...` line; `anchor.mismatch` event row exists.
- [ ] One `W4_REAL=1` run recorded: `sqlite3 dev-brain.db "SELECT json_extract(payload,'$.duration_s') FROM events WHERE kind='accept.w4_real_run';"` ≥ 600.
- [ ] Prod brain.db untouched: the script records
      `sqlite3 ~/Library/Application\ Support/Kahya/brain.db "SELECT COALESCE(MAX(id),0) FROM events;"`
      before and after the full gate run and fails if the two values differ.

## Out of scope

- Red-team scenarios (poisoned mail, homoglyph bypass, tainted-after-restart eval) and the
  full dev-profile hardening (egress deny-all, record-replay SDK fixtures) — W78-02.
- Backup restore drill on a clean machine — W78-05.
- `⌥⎋` halt semantics (`user_halted`) — W6-03/W6-04 gate.
- Fixing defects found by the gate beyond minimal patches to W4-01..06 deliverables — if a fix
  is substantial, reopen the owning task (`Status: in-progress`) rather than growing this one.
