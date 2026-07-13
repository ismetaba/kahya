# W6-03 — `⌥⎋` emergency halt semantics

**Status:** code-complete; live ⌥⎋ + real daemon-restart drill under launchd = user-assist (see the last acceptance item)
**Phase:** W6 — Voice + shortcut
**Depends on:** W6-01, W4-02
**Flags:** none
**Handoff refs:** §6 W6 ⚑

## Goal
After this task `⌥⎋` is a real emergency stop: every running worker (whole process group) and its Docker containers are killed, the task lands in a terminal `user_halted` state that survives daemon restarts, and every pending approval for it becomes unusable. A halted task can never silently resume, retry, or execute a stale approval.

## Context you need
- The binding spec, HANDOFF §6 W6, verbatim:
  > ⚑ **`⌥⎋` semantiği:** Hammerspoon'dan kahyad'a 'halt' IPC → worker process-group'u + ilgili Docker konteynerleri öldürülür, görev terminal `user_halted` durumuna yazılır (**session-resume ve outbox retry'dan kalıcı hariç**), bekleyen tüm onaylar geçersiz kılınır.
- The matching acceptance clause: "**uzun görev sırasında `⌥⎋` → daemon yeniden başlasa bile görev devam ETMİYOR ve retry edilmiyor**".
- This is an **emergency** stop: SIGKILL the process group immediately, no graceful drain. The durability story is W4-02's job — `intent → executing → receipt` with "makbuzsuz `executing`'de yalnız W1 oto-tekrar, W2/W3 asla" — `user_halted` must be a terminal state that both the W4-02 resume scanner and the outbox redelivery loop exclude unconditionally.
- Approval invalidation matters because of §5's one-time approval tokens: a diff approved before the halt must not authorize anything after it. Invalidate DB rows **and** revoke the tokens (W3-02).
- Prior outputs you build on: W12-07 per-task worker spawn (add `Setpgid` if it doesn't set it); W3-04 Docker shell tool (containers must carry `kahya.task_id`/`kahya.trace_id` labels — add the labels there if W3-04 didn't); W3-07 Telegram bot (edit pending approval messages, if configured); W6-01 `hammerspoon/kahya.lua` (the hotkey lives there; W6-01 deliberately did NOT bind `⌥⎋`).

## Deliverables
- kahyad: `POST /halt` on the UDS server (body `{"task_id": "<id>"}` or `{"all": true}`); halt executor; `user_halted` terminal state in the task state machine; exclusion clauses + regression tests in the W4-02 resume scanner and outbox redelivery query.
- Worker spawn (W12-07 code): `syscall.SysProcAttr{Setpgid: true}`; kahyad records the pgid per task in memory **and** persists it (`tasks.worker_pgid` column, new goose migration) so workers orphaned by a daemon crash/restart can still be halted.
- Docker shell tool (W3-04 code): containers launched with `--label kahya.task_id=<id> --label kahya.trace_id=<tid>` (only if not already done).
- `kahyad/cmd/kahya/`: `kahya halt [--task <id>]` (no flag = halt all active tasks).
- `hammerspoon/kahya.lua`: `⌥⎋` binding → `kahya halt` via `hs.task`, then `hs.notify` `Acil durdurma — tüm görevler durduruldu`.
- Ledger event kinds: `task.user_halted`, `approval.invalidated` (with `trace_id`).
- Tests (Go, in `make test`): process-group kill, terminal-state exclusion, container kill, token revocation.

## Steps
1. In the W12-07 spawner: set `Setpgid: true`; record the pgid in kahyad's in-memory task registry **and** persist it to a new `tasks.worker_pgid` column (goose migration). Do NOT assume a daemon restart killed the workers — macOS has no PDEATHSIG, so a worker spawned before a kahyad crash keeps running as an orphan. The halt executor kills whatever pgid is recorded for the task (in-memory entry if present, `worker_pgid` from the DB otherwise; a dead pgid makes `kill` fail with ESRCH, which is logged and ignored) — `⌥⎋` must be a real stop even immediately after a daemon restart.
2. Verify/extend W3-04 so every container the shell tool starts carries the two `kahya.*` labels.
3. Implement the halt executor in kahyad, per task, in this order (each step logged JSONL with `trace_id`; continue past individual failures — halt must be best-effort-complete, never partial-abort):
   1. `syscall.Kill(-pgid, syscall.SIGKILL)` on the worker process group.
   2. `docker kill $(docker ps -q --filter label=kahya.task_id=<id>)` (skip silently if Docker is not running).
   3. `UPDATE tasks SET status='user_halted' …` — terminal; also stamp `halted_at`.
   4. Cancel the task's undelivered outbox rows (`status='canceled'`).
   5. Invalidate all pending approvals for the task (`status='invalidated'`) and revoke their one-time tokens in the W3-02 token store. Token revocation is what makes the halt binding on **every** surface at once: a stale CLI `decide`, a Hammerspoon card button, or a Telegram W2 inline button pressed after the halt all hit the same token store and are DENIED (§5 one-time approval tokens).
   6. Ledger `task.user_halted` + one `approval.invalidated` per approval.
   7. If W3-07 sent a Telegram message for an invalidated approval, edit it to `⛔ Onay geçersiz — görev kullanıcı tarafından durduruldu.` (skip if the bot is not configured).
4. Wire `POST /halt` to the executor; `{"all": true}` iterates every task in a non-terminal running state. Response: `{"halted": <n>}`.
5. Add exclusion guards where W4-02 selects work: resume scan and outbox redelivery queries must filter `status != 'user_halted'` explicitly (defense in depth even though it is terminal), with a code comment quoting the §6 ⚑ line.
6. Add `kahya halt [--task <id>]`. Turkish output: `⛔ <n> görev durduruldu (user_halted).` / `Durdurulacak görev yok.` (exit 0 both ways — pressing `⌥⎋` with nothing running is not an error).
7. Bind `⌥⎋` in `hammerspoon/kahya.lua`: `hs.hotkey.bind({"alt"}, "escape", …)` → `hs.task` running `kahyaBin halt` (reuse W6-01's absolute-path constant — `hs.task` cannot PATH-resolve) → notify. The binding must work while the palette or an approval dialog is open.
8. Make halt idempotent: halting an already-`user_halted` (or otherwise terminal) task is a no-op that logs and returns success — a panicked double-press of `⌥⎋` must never error or corrupt state.
9. Scope check: halt kills **task-scoped** processes only (worker pgids, labeled containers). It does NOT touch kahyad itself, launchd jobs, or the kahyad-supervised MLX helper processes (`mlx_lm.server` from W3-08) — those are shared infrastructure, and the §4 idle-TTL unload owns their lifecycle.
10. Write the tests below; `make test && make lint`.

## Acceptance criteria
- [x] Go test: kahyad spawns a stub worker script that forks a child (`sleep 300 &`); `POST /halt` → both the worker PID and its child are gone (`kill(pid, 0)` errors for both) — proves process-*group* kill. Variant: clear the in-memory registry entry first (simulating a daemon restart that orphaned the worker) → halt via the persisted `tasks.worker_pgid` still kills the group. (`kahyad/internal/halt/executor_test.go`'s `TestHaltTaskKillsProcessGroupViaInMemoryPGID` + `TestHaltTaskKillsProcessGroupViaPersistedPGIDAfterSimulatedRestart`.)
- [x] Go test: a task in `executing` with one pending outbox delivery and one pending W2 approval is halted → task `status='user_halted'`, outbox row `canceled`, approval `invalidated`; a subsequent `POST /approvals/{id}/decision --approve` → DENY (token revoked). Then run the W4-02 resume scan and an outbox tick exactly as daemon startup does → zero worker spawns, zero redeliveries for that task. (`kahyad/internal/halt/executor_test.go`'s `TestHaltTaskExcludesFromResumeAndOutboxAfterHalt`; the HTTP route itself re-exercised in `kahyad/internal/server/halt_test.go`.)
- [x] Go test (requires the W3-04 Docker runtime, e.g. colima, running): `docker run -d --label kahya.task_id=<test-id> alpine sleep 300` registered to a fake task → halt → `docker ps -q --filter label=kahya.task_id=<test-id>` is empty. (`kahyad/internal/halt/container_test.go`'s `TestHaltTaskKillsLabeledDockerContainer`, gated on `KAHYA_DOCKER_TESTS=1` exactly like `mcp/shell`'s own live-Docker tests — ran for real against this session's colima daemon, not skipped.)
- [x] Ledger check: `sqlite3 ~/Library/Application\ Support/Kahya/brain.db "SELECT kind FROM events WHERE trace_id='<id>'"` includes `task.user_halted` and `approval.invalidated`. (Automated equivalent: `executor_test.go`'s `countEventsByTraceAndKind` helper, asserted in `TestHaltTaskExcludesFromResumeAndOutboxAfterHalt` and the double-halt test.)
- [!] Manual: start a deliberately long task (e.g. a multi-minute Docker shell job); press `⌥⎋` → notification `Acil durdurma — tüm görevler durduruldu` appears and the container dies; then `launchctl kickstart -k gui/$(id -u)/com.kahya.kahyad` → the task does not continue and is not retried (`kahya log --trace <id>` shows nothing after `task.user_halted`). This is the §6 kabul clause verbatim. — user-assist (needs a real Hammerspoon under launchd + a live macOS keypress + an actual daemon restart); every mechanism this drill exercises is hermetically proven above (process-group kill, container kill, terminal-state exclusion surviving a *simulated* restart via the persisted `worker_pgid`, zero resume/outbox activity after halt) — this item is the real end-to-end physical drill, deferred to the user exactly like W6-01/W6-02's own TCC/Hammerspoon manual items.
- [x] `kahya halt` with no running tasks prints `Durdurulacak görev yok.` and exits 0. (`kahyad/cmd/kahya/halt_test.go`'s `TestHaltZeroHaltedPrintsNoneMessage`.)

## Out of scope
- Graceful drain, pause/resume, or partial checkpointing — halt is terminal SIGKILL by design; anything softer is a different feature no backlog row owns.
- Compensating already-completed side effects — the saga compensation executor is deferred by HANDOFF §8 ("saga telafi-yürütücüsü … §6 W4 — yeterli"); the W1 undo window belongs to W3-02.
- Endpoint Security extension / VM isolation — §8 deferred.
- The W6 gate tests that span daemon restart end-to-end — W6-04 (this task's tests may simulate restart by re-running the scan; W6-04 restarts the real daemon).
