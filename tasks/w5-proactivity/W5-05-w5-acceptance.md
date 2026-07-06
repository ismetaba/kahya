# W5-05 — W5 acceptance gate

**Status:** todo
**Phase:** W5 — Proactivity + consolidation
**Depends on:** W5-01, W5-02, W5-03, W5-04
**Flags:** user-assist 🧍 (the gate drill is human-run; drill item 4 needs the user to answer at least one ritual question on Telegram)
**Handoff refs:** §6 W5 acceptance

## Goal

The W5 weekly gate is verified end-to-end and encoded as permanent tests: single-notification briefing with `trace_id`, nightly consolidation diff-commit, taint enforcement on the briefing session, and a ~20-question retrieval mini-baseline that shows no regression after consolidation. The next phase (W6) must not start until this gate is green.

## Context you need

- HANDOFF §6 W5 acceptance (verbatim — this file exists to prove exactly this):
  > → **Kabul:** 08:30 brifingi tek bildirim + `trace_id`; gece külliyat konsolide olup diff commit ediliyor; **tainted brifing oturumundan doğrudan W-araç çağrısı reddediliyor** (aynı eylem temiz oturumdan geçiyor); ~20 soruluk retrieval mini-baseline konsolidasyon sonrası gerileme göstermiyor.
- Why the mini-baseline exists, HANDOFF §5 memory #5 (verbatim):
  > **Gerçek-temelli değerlendirme:** değerlendirme kümesi mağazanın kendi inançlarından değil, **haftalık doğru/yanlış ritüelinin insan etiketlerinden** beslenir. 1/6/24-ay ayrıntı-yoklamaları + precision + çekimserlik. Her konsolidasyon/gömülü/füzyon değişikliğinden **önce** kapı.
  The full labeled eval is W78-01; the W5 mini-baseline is a small fixed retrieval set run before/after consolidation to catch regressions early. Seed it from the seeded corpus (W0-01) plus any `eval_labels` already collected by W5-03.
- Prior outputs under test: W5-01 briefing (tainted-by-design session, once-per-day dedupe, Telegram delivery), W5-02 consolidation (suggestion-mode diff → `author=kahyad` commit, reindex trigger, nightly push), W5-03 ritual labels, W5-04 injection-eligibility predicate, W4-03 taint store (persisted by `session_id`, only rises, fail-closed), W12-10 e2e harness (reuse its patterns for spawning kahyad+worker in tests).
- Per tasks/README.md, gate criteria become real automated tests in `make test` wherever feasible; the truly end-to-end pieces run from a drill script that a human executes once and that stays in the repo.
- Mini-baseline format (canonical): `eval/mini-baseline.jsonl`, one JSON object per line: `{"q": "<Turkish question>", "expect_substring": "<must appear in top-k retrieved chunks>", "k": 5}`. ~20 questions, Turkish/mixed-language, over the seeded corpus. Include the W1–2 morphology probe as question 1: `{"q": "evlerimizden", "expect_substring": "ev", "k": 5}` (byte-exact, trigram index — no manual stemming). Scoring: a question passes if the substring appears in the top-k `memory_search` results; **regression** = any previously-passing question failing after consolidation, or pass-count dropping. Abstention (empty result) counts as a fail for questions with an expectation.

## Deliverables

- `eval/mini-baseline.jsonl` — ~20 questions per the format above (write them against the actually-seeded corpus; questions must pass on the pre-consolidation index at authoring time).
- `kahyad/internal/eval/mini.go` + `kahya eval mini` CLI: runs the baseline against `memory_search`, prints pass/fail per question and a summary line, writes an `eval.mini.run` event (with pass count + `trace_id`) to `events`. Exit code non-zero on any regression vs. the stored previous run. The CLI talks to kahyad over the UDS; the event row is written by kahyad — the CLI process never opens brain.db (kahyad is the sole writer). The runner emits `eval.mini.run` only, NEVER `eval.mini.pass` (that event belongs to W78-01 and is what unlocks consolidation auto-commit).
- Gate tests (in `make test`):
  - `kahyad/internal/briefing/gate_test.go` — single-notification + trace_id assertions (may extend W5-01's tests).
  - `kahyad/internal/consolidation/gate_test.go` — consolidation on a fixture corpus produces a diff and, after approval, an `author=kahyad` commit + reindex event.
  - `kahyad/internal/policy/taint_gate_test.go` — briefing-session W-tool DENY vs. clean-session ALLOW-path (approval flow reached), same tool, same target.
  - `kahyad/internal/eval/mini_test.go` — baseline runner on a fixture index; injected regression is detected (exit non-zero).
- `scripts/w5_gate.sh` — the human-run end-to-end drill (steps 4–7 below), idempotent, prints PASS/FAIL per gate item in Turkish.
- BACKLOG.md status updates for W5 tasks on completion (per protocol; done at execution time, not by this spec).

## Steps

1. Read HANDOFF §6 W5 acceptance and §5 memory #5. Confirm W5-01..04 are `[x]` in BACKLOG.md (this gate must not be gamed by stubbing their features).
2. Author `eval/mini-baseline.jsonl` against the live seeded corpus; run each question manually via `kahya` / `memory_search` to confirm it passes before freezing the file.
3. Implement `kahya eval mini` + the runner, storing run results in `events` and comparing to the previous `eval.mini.run`.
4. Drill item 1 — briefing: `kahya job run morning-briefing`; verify exactly one Telegram notification arrives; capture its `trace_id`; `kahya log --trace <id>` shows collector→worker→delivery under one id; re-run and verify `briefing.skipped_duplicate`.
5. Drill item 2 — taint: from the *same* briefing session id, drive a direct W1 tool call (test hook in the drill script) and verify DENY + ledger event; then run the identical action from a fresh clean session and verify it proceeds to the normal approval flow.
6. Drill item 3 — consolidation: run `kahya eval mini` (record baseline) → `kahya job run nightly-consolidation` → `kahya consolidation show` → `approve` → verify `git -C ~/Kahya log -1 --format='%an'` = `kahyad` and a `reindex.completed` event → run `kahya eval mini` again → zero regressions.
7. Drill item 4 — labels flowing: trigger `kahya job run truth-ritual`, ask the user to answer at least one question on Telegram, then verify `sqlite3 ... "SELECT count(*) FROM eval_labels WHERE answered_at IS NOT NULL"` > 0 (if the user is unavailable, set `Status: blocked-user` per protocol rather than faking an answer).
8. Wire all gate tests into `make test`; run `make test` and `make lint` green.
9. Record the drill output (date + PASS lines) in this task file under a "Gate run" note, set Status: done, mark `[x]` in BACKLOG.md.

## Acceptance criteria

- [ ] `make test` green, including the four new gate tests.
- [ ] `scripts/w5_gate.sh` run once for real prints PASS for all four gate items (mirrors §6 W5 acceptance byte-for-byte in intent: tek bildirim + `trace_id`; diff commit; tainted-DENY/clean-ALLOW; mini-baseline no regression).
- [ ] `kahya eval mini` exits 0 post-consolidation and `sqlite3 ... "SELECT count(*) FROM events WHERE type='eval.mini.run'"` ≥ 2 (before + after runs recorded).
- [ ] `eval/mini-baseline.jsonl` contains ≥20 lines and line 1 is the `evlerimizden`→`ev` morphology probe, byte-exact.
- [ ] Taint gate test proves the **same** tool+target pair is DENIED from the briefing session and ALLOWED (to approval) from a clean session — not two different actions.
- [ ] `kahya log --trace <briefing-trace-id>` output saved by the drill script shows kahyad and worker JSONL lines sharing the id.

## Out of scope

- Full ~50-command Turkish eval + precision ≥80% gate — W78-01. Red-team set + `KAHYA_ENV=dev` profile — W78-02. CI invariant collection — W78-03.
- Enabling consolidation auto-commit — the W5-02 guard requires an `eval.mini.pass` event, which ONLY the W7 mini-eval (W78-01) writes; this task's runner emits `eval.mini.run` and must never write `eval.mini.pass`.
- Any new feature work: this task only verifies and encodes W5-01..04. If a gate item fails, fix it under its owning task id, not here.
- W6 voice/palette work — starts only after this gate is `[x]`.
