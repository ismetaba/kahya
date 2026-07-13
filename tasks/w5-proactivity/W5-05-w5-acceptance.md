# W5-05 — W5 acceptance gate

**Status:** blocked-user (code-complete: all four gate tests + `kahya eval mini` + `scripts/w5_gate.sh` implemented, `make test`/`make lint` green; the REAL end-to-end drill run and drill item 4's Telegram ritual answer are user-assist runtime — see "Gate run" note below)
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

- [x] `make test` green, including the four new gate tests (`TestW5GateSingleNotificationTraceIDThenDuplicateSkipped`, `TestW5GateConsolidationProducesDiffThenApproveCommitsAsKahyaAndReindexes`, `TestSameToolSameTargetDeniedTaintedAllowedClean`, `TestRunnerRunDetectsInjectedRegression`) — plus a dedicated Makefile guard (mirroring W4-07's) that fails the build if any of the four is missing from the test run or its package reports `[no test files]`.
- [ ] `scripts/w5_gate.sh` run once for real prints PASS for all four gate items — **user-assist**: this environment has no Calendar Automation TCC grant (the morning-briefing job's `collect_calendar.go` osascript call hangs — a real, live-drill-discovered W5-01 bug, flagged separately, not fixed here per this task's own scope rule) and no live Anthropic credential (HANDOFF W0-04, pre-existing blocker) or Telegram token, so the drill's model/Telegram-dependent halves come back ERTELENDİ (deferred) rather than GEÇTİ (pass) on THIS machine. See "Gate run" note below for the actual run's full output. The script itself is code-complete and was validated live against a real running kahyad (health check, job dispatch, `/policy/check`, `kahya eval mini`, ledger/sqlite reads all exercised for real; two real bugs in the script's own bash/python plumbing were found and fixed this way before freezing it — see the note).
- [x] `kahya eval mini` exits 0 on a non-regressing run and `sqlite3 ... "SELECT count(*) FROM events WHERE kind='eval.mini.run'"` grows by 1 per run (hermetically proven in `kahyad/internal/eval/mini_test.go`'s `TestRunnerRunFirstRunNeverRegressesAndLedgersEvent`/`TestRunnerRunDetectsInjectedRegression`, AND reproduced live in this environment: two consecutive `kahya eval mini` runs against a real daemon took the event count from 0→1→2 with exit 0 both times, no regression on an unchanged corpus). Note: the acceptance criterion's own text says `events.type`; the actual column is `events.kind` (`type` does not exist in the schema) — the query above uses the real column name. The full "post a REAL cloud-backed consolidation" loop is part of the live drill (still user-assist, see above).
- [x] `eval/mini-baseline.jsonl` contains ≥20 lines (22) and line 1 is the `evlerimizden`→`ev` morphology probe, byte-exact (enforced by `TestMiniBaselineFileShape`). Authored and verified against the actually-seeded `~/Kahya/memory` corpus using the real `search.Searcher`/`indexer.Indexer` code (FTS-only, no embedder) before freezing.
- [x] Taint gate test proves the **same** tool+target pair is DENIED from the briefing session and ALLOWED (to approval) from a clean session — not two different actions (`kahyad/internal/policy/taint_gate_test.go`, both tests construct the ToolInput bytes ONCE and pass that identical value to both `Check` calls).
- [ ] `kahya log --trace <briefing-trace-id>` output saved by the drill script shows kahyad and worker JSONL lines sharing the id — **user-assist** (needs a real briefing delivery, see above); the identical invariant is hermetically proven by `TestRunEmitsCollectorWorkerAndDeliveryJSONLLinesUnderOneTraceID` (jsonl_test.go) and composed into `TestW5GateSingleNotificationTraceIDThenDuplicateSkipped` (gate_test.go).

## Gate run

**2026-07-13, this session (blocked-user environment: no Calendar TCC grant, no live Anthropic credential, no Telegram token).**

`make test` (full suite, including the W4-07 and W5-05 Makefile guards) and `make lint`: **green**.

`scripts/w5_gate.sh` run once for real against a throwaway `KAHYA_ENV=dev` kahyad (fresh temp HOME/DB, real binaries, real seeded corpus, no mocks):

```
GEÇTİ      sağlık
KALDI      brifing-tek-bildirim -- 60s içinde iş tamamlanmadı (collect_calendar.go'nun
           osascript çağrısı askıda kaldı - Takvim Otomasyonu izni hiç karar verilmemiş)
ERTELENDİ  brifing-trace_id / brifing-tekrar-atlandı (yukarıdaki hataya bağlı)
ERTELENDİ  taint-tainted-deny (brifing işi tamamlanmadığı için taint satırı commit
           edilmedi) / taint-clean-allow (gerçek bir 'clean' oturum yok)
GEÇTİ      mini-baseline-önce (eval.mini.run olayı yazıldı, 1/22 soru geçti - bu
           ortamdaki 2 dosyalık W12-10 fixture korpusu üzerinde, gerçek ~/Kahya
           içeriği değil)
ERTELENDİ  konsolidasyon-diff / -onay-commit / -reindex / mini-baseline-sonra
           (nightly-consolidation'ın cloud-lane oturumu canlı kimlik bilgisi
           olmadan başarısız oldu)
ERTELENDİ  ritüel-yanıt (90s içinde Telegram yanıtı gelmedi - Telegram bu ortamda
           yapılandırılmamış)
=== ÖZET: 1 KALDI, 9 ERTELENDİ ===
```

The one real FAIL (`brifing-tek-bildirim`) is a genuine, previously-unknown W5-01 bug found by this drill, not a W5-05 defect: `collect_calendar.go`'s `ExecCalendarRunner.Run` invokes `osascript` with the CALLER's context (no deadline), so on a machine where Calendar Automation permission was never decided, the call can hang indefinitely, wedging the whole morning-briefing job before it ever reaches `Taint.InsertUntrusted`/`TaskStore.InsertTask`. Flagged as a follow-up (spawn_task `task_d175a382`, "Bound collect_calendar.go's osascript call with its own timeout") — per this task's own scope rule, fixed under W5-01, not here.

Two real bugs in `scripts/w5_gate.sh` itself were found and fixed during this same live run, before freezing the script:
1. `python3 -` reads its own program text from stdin, so `cmd | python3 - <<'EOF' ... EOF` silently starves the inner script's `sys.stdin` of the piped data (the interpreter already consumed fd 0 reading the heredoc-supplied program). Two call sites (`wait_for_job_done`'s event filter, and the briefing task_id extraction) used exactly this broken pattern; fixed by having each python3 heredoc read the JSONL log FILE directly by path (passed as `sys.argv`) instead of piping another process's output into it.
2. The script originally polled kahyad.jsonl for package-specific event names (`consolidation.pending`, `briefing_skipped_duplicate`, etc.) that are ONLY ever ledgered (brain.db `events` table), never echoed to JSONL — so the wait would never find them. Fixed by polling the scheduler's own universal `job_completed`/`job_failed` JSONL line (kahyad/internal/scheduler.Scheduler.Trigger logs this for every job, always) as the "is it done" signal, then reading the specific ledger event kind from `events` via `sqlite3` for the "what happened" verdict.

Drill item 4 (truth-ritual Telegram answer) needs the user; per protocol this stays `blocked-user` rather than a fabricated answer. Re-run `scripts/w5_gate.sh` once a Calendar Automation grant, a live Anthropic credential, and a Telegram bot token are all wired (`make install-agent` / `make run-daemon`) to turn every ERTELENDİ line above into a real GEÇTİ/KALDI.

## Out of scope

- Full ~50-command Turkish eval + precision ≥80% gate — W78-01. Red-team set + `KAHYA_ENV=dev` profile — W78-02. CI invariant collection — W78-03.
- Enabling consolidation auto-commit — the W5-02 guard requires an `eval.mini.pass` event, which ONLY the W7 mini-eval (W78-01) writes; this task's runner emits `eval.mini.run` and must never write `eval.mini.pass`.
- Any new feature work: this task only verifies and encodes W5-01..04. If a gate item fails, fix it under its owning task id, not here.
- W6 voice/palette work — starts only after this gate is `[x]`.
