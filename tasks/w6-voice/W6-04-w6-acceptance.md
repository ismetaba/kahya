# W6-04 — W6 acceptance gate

**Status:** todo
**Phase:** W6 — Voice + shortcut
**Depends on:** W6-01, W6-02, W6-03
**Flags:** none
**Handoff refs:** §6 W6 acceptance

## Goal
The Phase W6 gate: automated end-to-end tests plus a one-time manual protocol proving all three §6 W6 acceptance clauses. W7–8 does not start until this gate is green.

## Context you need
- The gate, HANDOFF §6 W6, verbatim:
  > → **Kabul:** basılı-tut → konuş → transkript → görev döngüsü, %100 yerel; **uzun görev sırasında `⌥⎋` → daemon yeniden başlasa bile görev devam ETMİYOR ve retry edilmiyor**; palet-aç→ilk-token zaman damgaları events tablosuna loglanıyor.
- And the halt spec it tests against:
  > ⚑ **`⌥⎋` semantiği:** Hammerspoon'dan kahyad'a 'halt' IPC → worker process-group'u + ilgili Docker konteynerleri öldürülür, görev terminal `user_halted` durumuna yazılır (**session-resume ve outbox retry'dan kalıcı hariç**), bekleyen tüm onaylar geçersiz kılınır.
- Latency: §6 defines "*palet-aç→ilk-token* = palet açılış zaman damgası → ilk stream token'ı" and the north star targets "palet-aç→ilk-token p50 <1.5s". The gate requires the **timestamps to be logged**, not the p50 target — p50 is measured during dogfood via W78-04. Log the observed delta, do not gate on it.
- Building blocks you are testing, not re-implementing: W6-01 palette + `palette_open`/`first_token` events + local approval cards; W6-02 `input_audio_path` envelope + `stt.py` + fixture `worker/tests/fixtures/tr_toplanti.wav`; W6-03 `POST /halt` + `user_halted` terminal state. Model calls in tests go through the stub/record-replay harness patterns established by W12-08/W12-10 tests — never a live cloud call in `make test`.
- Per tasks/README.md, gate tests are **permanent** regression tests; W78-03 will collect them into the CI invariant map.
- BACKLOG.md gate rule binds this task: "Weekly gates are the `*-acceptance` tasks — do not start the next phase until the current gate is `[x]`". The only W6-era exception mechanism is the ↔ flag, and no W6 task carries it — so there is nothing slidable out of this gate.
- If a gate test exposes a bug, the fix belongs to the owning task (reopen W6-01/02/03 per README step 7 semantics), committed as that task's ID — never as a silent fix inside `[W6-04]`.

## Deliverables
- `kahyad/internal/gate/w6_gate_test.go` (or the integration-test location established by W4-07's gate — reuse its daemon start/stop harness): the three automated gate tests below.
- `worker/tests/test_w6_gate.py`: STT-side gate assertions (offline transcription within the task loop).
- Manual protocol results recorded as the checked boxes in this file (no separate report doc).
- BACKLOG.md: only the **W6-04** row flipped to `[x]`, by this task at completion (README step 6). W6-01..03 rows are flipped by their owning tasks and must already be `[x]` before this task starts — step 1 verifies, never pre-checks.

## Steps
1. Confirm W6-01, W6-02, W6-03 are `[x]` in BACKLOG.md with all their acceptance boxes checked. If any is not, stop — this task depends on them.
2. **Gate test 1 — voice loop, %100 yerel** (`test_w6_gate.py` + Go side): submit `kahya ask --audio worker/tests/fixtures/tr_toplanti.wav --palette-opened-at <now>` against a kahyad test instance whose forward proxy points at the local stub model. Assert, in order, from the JSONL logs and DB of one `trace_id`: (a) `stt.completed` emitted with `HF_HUB_OFFLINE=1` in the worker env; (b) zero forward-proxy/egress log entries timestamped before `stt.completed` (audio and transcription touched no network); (c) the task reached a completed state with the transcript as its prompt.
3. **Gate test 2 — halt survives a real daemon restart** (`w6_gate_test.go`): start kahyad (test harness), launch a long-running stub task that has called a W2 tool (so an outbox row and a pending approval exist), `POST /halt`, then **stop the kahyad process and start a fresh one on the same brain.db**. After startup resume/outbox passes: no worker spawned for the task, outbox row still `canceled`, task still `user_halted`, approval decision attempt → DENY. This is stronger than W6-03's in-process test — it proves "daemon yeniden başlasa bile".
4. **Gate test 3 — palette timestamps** (`w6_gate_test.go`): after gate test 1's run, `SELECT kind, ts FROM events WHERE trace_id=?` returns both `palette_open` and `first_token`; assert `first_token.ts >= palette_open.ts` and log the delta in ms (`t.Logf`, informational).
5. Wire both test files into `make test`; run `make test && make lint` on a clean checkout.
6. Run the manual protocol once, under launchd-started processes (per §7 ⚑ TCC rule — not from a terminal):
   1. Hold `⌥Space`, speak a Turkish command ("yarın dokuzda toplantım var"), release → transcript-driven answer notification.
   2. Start a long task, press `⌥⎋`, then `launchctl kickstart -k gui/$(id -u)/com.kahya.kahyad` → task neither continues nor retries; `kahya log --trace <id>` ends at `task.user_halted`.
   3. Note the observed palet-aç→ilk-token delta from `kahya log --trace <id>` for the dogfood baseline.
7. Check the boxes below, set this file's Status to `done`, mark `[x]` in BACKLOG.md, commit as `[W6-04] pass W6 acceptance gate`.

## Acceptance criteria
- [ ] `make test` green on a clean checkout, including the three new gate tests (gate test 2 requires the W3-04 Docker runtime running; that is part of the dev environment since W3).
- [ ] Gate test 1 proves: `stt.completed` before any proxy/egress event, offline STT (`HF_HUB_OFFLINE=1`), single `trace_id` end to end — "basılı-tut → konuş → transkript → görev döngüsü, %100 yerel" (automated half).
- [ ] Gate test 2 proves: after halt + real daemon restart, zero resume, zero retry, approvals dead — "daemon yeniden başlasa bile görev devam ETMİYOR ve retry edilmiyor".
- [ ] Gate test 3 proves: `palette_open` and `first_token` rows exist in the `events` table for the same `trace_id` — "palet-aç→ilk-token zaman damgaları events tablosuna loglanıyor".
- [ ] Manual protocol step 1 done live under launchd (hold→speak→answer) — date and observed transcript noted here when checking the box.
- [ ] Manual protocol step 2 done live (`⌥⎋` + `launchctl kickstart -k`) — trace id noted here when checking the box.
- [ ] Observed palet-aç→ilk-token delta recorded here (informational; the p50 <1.5s north-star is tracked by W78-04, not gated now).
- [ ] `make lint` green; no W6-01..03 acceptance test regressed.

## Out of scope
- Retrieval QA and the ~50-command eval set — W78-01.
- Red-team scenarios and the `KAHYA_ENV=dev` profile — W78-02.
- CI collection/coverage map of invariant tests — W78-03.
- `kahya metrics` (p50, commands/day, cache-hit) — W78-04; this gate only verifies the raw timestamps exist.
- Performance tuning toward the p50 <1.5s target — dogfood-phase work.
- Any new W6 feature work: if a gate test fails, fix it under the owning task (W6-01/02/03), not here.
