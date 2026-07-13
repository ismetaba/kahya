# W78-06 — Dogfood readiness gate + tracking doc

**Status:** done — `kahya readiness [--phase=start|complete]` over `GET /readiness` (reads the recorded eval/redteam/restore evidence + W78-04 metrics, fail-closed on missing/stale/future-dated rows), `docs/mvp-readiness.md` (every §6 gate + §9 criterion → evidence), `docs/dogfood.md` (2-week tracker + the v1-blocked rule byte-exact), `make readiness`/`make readiness-complete`. Firing the real 2-week dogfood window (so `--phase=complete` can go green) is the user's real-usage period — user-assist runtime.
**Phase:** W7–8 — Hardening + eval
**Depends on:** W78-01, W78-02, W78-03, W78-04, W78-05, and every phase gate: W12-10, W12-11, W3-10, W4-07, W5-05, W6-04
**Flags:** none
**Handoff refs:** §2, §6 north star, §9

## Goal

A final readiness checklist verifying EVERY §6 weekly acceptance gate and the §9 "MVP done"
criteria are met, plus an opened 2-week dogfood tracking doc. This is the gate that says the
MVP may enter real daily use — and encodes the hard rule that **nothing from v1 starts until
the MVP survives 2 weeks of real daily use**.

## Context you need

Binding HANDOFF items (verbatim):

> Kural: **MVP 2 hafta gerçek günlük kullanımdan sağ çıkmadan v1'den hiçbir şey başlamaz.**

> **MVP tamamlandı sayılır:** 2 hafta kesintisiz günlük kullanım · sıfır veri-kaybı olayı (yedekten geri-dönüş bir kez tatbik edildi) · ≥10 komut/gün · haftada ≥5 "hatırladı" anı · egress/gizli-şerit/W3 değişmezleri kod-testli (CI'da yeşil).

> **Kuzey-yıldızı (MVP):** komut/gün — *yararlı mı?* (hedef: hafta 2'de ≥10/gün; komutların ≥%60'ı açıklama-turu olmadan tamamlanır; palet-aç→ilk-token p50 <1.5s).

The §6 weekly gates to confirm green (each is a `*-acceptance` task): W12-10 (core injection +
`'evlerimizden'`→`'ev'` + single trace_id), W3-10 (Telegram W2 diff / W3 CLI-only / egress /
secret-lane), W4-07 (SIGKILL-resume no double-exec / offline / anchor mismatch), W5-05 (briefing
/ consolidation diff / tainted-session reject / retrieval no-regression), W6-04 (voice loop /
`⌥⎋` no-continue / palette timestamps), and the W7–8 tasks W78-01/02/03/05.

This task builds nothing new in the product; it aggregates verification and opens the tracking
doc. The readiness check is a real command (not a manual eyeball) so it can be re-run.

## Deliverables

- `docs/dogfood.md` — 2-week tracking template: per-day rows for commands issued, "remembered" moments, incidents (structured column with a machine-parseable `type:` prefix — `data-loss` / `safety` / `crash` — because `--phase=complete` parses it), and a running north-star tally (commands/day, clarification-turn rate, palette→first-token p50). Includes the explicit rule that v1 work is blocked until 2 clean weeks pass.
- `docs/mvp-readiness.md` — the checklist mapping each §6 gate + each §9 MVP-done criterion → the evidence (task id, test name, or `kahya`/`make` command that proves it).
- `kahya readiness` subcommand (+ `kahyad/internal/readiness/`) — **thin UDS client** of a kahyad `GET /readiness` endpoint that reads **recorded evidence rows** (read-only, same `query_only` discipline as W78-04): latest `eval.retrieval.result` (green ≥0.80 — W78-01), latest `eval.redteam.result` (`bypasses=0` — W78-02), latest `restore.drill.result` (`ok` — W78-05), plus the W78-04 metrics aggregates; prints a Turkish pass/fail summary; exits non-zero if any gate is red or its evidence row is missing. Live test/lint greenness is NOT re-derived from events — the Makefile orchestration runs it (below).
- `Makefile`: `make readiness` = `make test lint invariants` (full local run — includes all `*-acceptance` gate tests and the `local-integration`-tagged invariant tests from W78-03) **then** `kahya readiness --phase=start`; `make readiness-complete` = same, then `kahya readiness --phase=complete`.

## Steps

1. Read §2, §6 (all weekly acceptance gates + north star + metric definitions), and §9 MVP-done criteria.
2. Write `docs/mvp-readiness.md`: one row per §6 gate and per §9 criterion, each with the concrete evidence command/test. §9 criteria: 2 weeks continuous daily use, zero data-loss incident (restore drilled once — W78-05), ≥10 commands/day (W78-04), ≥5 remembered-moments/week (W78-04), egress/secret-lane/W3 invariants code-tested green in CI (W78-03).
3. Implement the kahyad `GET /readiness` endpoint + the `kahya readiness` client: read the latest recorded `eval.retrieval.result`, `eval.redteam.result`, `restore.drill.result` events and the W78-04 metrics aggregates. Any missing/red evidence row ⇒ that line fails. Live test greenness comes from the `make readiness` orchestration, not from events.
4. Distinguish two classes in the output: **build gates** (tests/evals that must be green NOW to start dogfood) vs **usage gates** (the §9 MVP-done set: ≥10 commands/day, ≥5 remembered-moments/week, a 2-week continuous window, zero data-loss incidents — only satisfiable DURING dogfood). `kahya readiness --phase=start` requires all build gates; `--phase=complete` additionally requires the usage gates over the dogfood window: metrics thresholds from W78-04, and zero data-loss incidents verified by parsing the structured incident column of `docs/dogfood.md` (any `type: data-loss` row ⇒ red). North-star targets that are goals rather than §9 gates (clarification-turn rate ≤40%, palette→first-token p50 <1.5s) are REPORTED with pass/fail marks but do not drive the exit code — §9 is the contract.
5. Write `docs/dogfood.md` with the daily template and the v1-blocked rule stated verbatim.
6. Run `make readiness` (which runs `make test`, `make lint`, `make invariants`, then `kahya readiness --phase=start`); confirm all build gates green before opening the dogfood window.

## Acceptance criteria

- [ ] `make readiness` exits 0 only when: the full local `make test` + `make lint` + `make invariants` run is green (includes all `*-acceptance` gate tests and `local-integration` invariant tests) AND the recorded evidence rows are green — `eval.retrieval.result` ≥0.80 (W78-01), `eval.redteam.result` bypasses=0 (W78-02), `restore.drill.result` ok (W78-05). Any red/missing item ⇒ non-zero with the failing lines in Turkish.
- [ ] `docs/mvp-readiness.md` maps every §6 weekly gate and every §9 MVP-done criterion to concrete evidence (task id + command/test).
- [ ] `docs/dogfood.md` exists with a 2-week daily template and states the rule that no v1 work starts until the MVP survives 2 weeks of real daily use.
- [ ] `kahya readiness --phase=complete` correctly evaluates the §9 usage gates (≥10 commands/day, ≥5 remembered-moments/week, 2-week window, zero `type: data-loss` incidents parsed from `docs/dogfood.md`) using W78-04 metrics, and reports (without gating on) the north-star targets clarification-rate ≤40% and p50 <1.5s; it is expected RED at task time and green only after a real 2-week window — a fixture test proves both the red and the green path.
- [ ] The `kahya readiness` evidence check runs over the kahyad UDS `GET /readiness` endpoint, read-only over brain.db (`query_only` discipline), and is re-runnable; a test with fixtures asserts a red gate produces a non-zero exit.
- [ ] `make test` and `make lint` green.

## Out of scope

- Building any product feature — all functionality is owned by earlier tasks; this task only aggregates and tracks.
- Anything from HANDOFF §8 (v2 / "şimdi inşa etme"): NATS, SQLCipher, GPT-OSS-120B, screen-observation firehose, SwiftUI browser, bitemporal graph, wake-word, XTTS/Piper, iPhone app, etc. The dogfood doc explicitly gates these behind the 2-week rule.
- Firing the actual 2-week dogfood — this task opens the tracking doc and the readiness gate; running the 2 weeks is the user's real-usage period.
