# W5-02 — Nightly consolidation

**Status:** todo
**Phase:** W5 — Proactivity + consolidation
**Depends on:** W4-01, W12-04, W12-09
**Flags:** none
**Handoff refs:** §6 W5 ⚑, §5 memory #4

## Goal

A nightly (03:00) consolidation job exists: an Agent SDK session merges/organizes `~/Kahya/memory/*.md` and the result is git-committed under strict commit discipline — but for the first 2 weeks only as a **suggested diff** the user approves from the CLI. Consolidation writes only markdown+git; kahyad triggers the SQLite reindex afterwards. Hot-window/detail-atom rules from §5 memory #4 are enforced.

## Context you need

- HANDOFF §6 W5 (verbatim, all three ⚑ bind this task):
  > gecelik konsolidasyon (bir Agent SDK oturumu markdown'ı birleştirip git-commit eder — **yalnız markdown+git'e yazar, SQLite reindex'ini kahyad tetikler**)
  > ⚑ **Konsolidasyon ilk 2 hafta öneri-modunda:** diff üretir, kullanıcı onayıyla commit eder; otomatik commit W7 mini-eval yeşiliyle açılır.
  > ⚑ **Commit disiplini:** konsolidasyondan önce kirli working tree `author=user` commit'i olur (user_edit tier git author'dan türetilir); daemon değişiklikleri daima `author=kahyad` ayrı commit; çelişkide user_edit kazanır (daemon o gün kullanıcının dokunduğu satırları atlar).
- HANDOFF §5 memory #4 (verbatim):
  > **90+ gün sıcak pencere + ayrıntı-atomu:** 48 saat değil ≥90 gün. Soğutmadan önce sayı/tarih/alıntı/karar/söz'ler yapılandırılmış olgulara terfi. Her özet **ham kanıttan** üretilir, asla alt-özetten.
- Backup tie-in, HANDOFF §6 backup ⚑: "(1) `~/Kahya` → private git remote; W5 gecelik commit'in sonuna `git push`". W4-06 owns the push machinery; this task must invoke it at the end of a successful nightly run.
- Secret-lane routing, HANDOFF §4 ⚑ (binds which model sees which file):
  > ⚑ **Sıralama değişmezi:** *Hiçbir bayt, gizli-şerit sınıflandırması yerel/deterministik olarak tamamlanmadan bulut modele gitmez.* policy.yaml globları **yalnız dosya yolları** için
  Memory files are path-addressed, so classification is deterministic via the policy.yaml secret-lane globs (W3-01). Files matching those globs may only be consolidated by the local `Qwen3-30B-A3B` lane (W3-08); if the local model cannot load (memory pressure), those files are **skipped fail-closed with a Turkish notice — never sent to cloud**. Non-secret-lane consolidation uses `claude-haiku-4-5` (§4 routing row "Çıkarım · geri-yazım"); routing decided in Go, worker obeys the envelope.
- Prior outputs: W4-01 scheduler; W12-04 corpus indexer (`kahya reindex`, kahyad sole brain.db writer); W12-09 worker harness; W12-05 memory MCP. Scheduling per §4 ⚑: launchd `StartCalendarInterval` (missed-during-sleep runs once on wake), never in-daemon cron for wall-clock.
- Mechanics chosen here (canonical, do not redesign): consolidation runs in a temporary **git worktree** of `~/Kahya` on branch `kahya/consolidation-YYYYMMDD`; the suggested diff is `git diff main...<branch>`. Approval merges the branch to main (`--ff-only` after rebase) with `--author="kahyad <kahyad@kahya.local>"`. User pre-commit uses `--author="user <user@kahya.local>"`; the W12-04 indexer derives `source_tier=user_edit` from that author.
- Pending-diff collision (canonical): if the nightly run starts while a previous suggestion is still pending (neither approved nor rejected), the old branch/worktree is deleted, a `consolidation.superseded` event is ledgered with both trace_ids, and the run regenerates against current `main`. A stale pending diff is never auto-approved and never merged.

## Deliverables

- `kahyad/internal/consolidation/consolidation.go` — orchestrator: pre-commit dirty tree as user, spawn session(s), collect edits in worktree, enforce user-line skip rule, produce diff, suggestion/auto mode switch, trigger reindex, invoke nightly `git push`.
- `kahyad/internal/consolidation/hotwindow.go` — ≥90-day hot-window selection + detail-atom promotion: numbers/dates/quotes/decisions/promises extracted to candidate facts written through kahyad's fact-write path as `source_tier=agent_derived` (quarantined; hardened by W5-04). Summaries generated only from raw episode/chunk evidence, never from prior summaries.
- Worker: consolidation session profile in `worker/` (prompt: merge duplicates, fix headings, fold new notes into topic files; output = file edits, no direct git access; session tool surface limited to a scratch-dir fs write provided by kahyad).
- Scheduler registration: `nightly-consolidation` at 03:00 via W4-01.
- CLI: `kahya consolidation show` (render pending diff), `kahya consolidation approve` (Turkish confirm prompt: `onayla`), `kahya consolidation reject`. Pending state stored in `tasks`/`events`.
- Config: `consolidation.auto_commit` (default `false`); kahyad **refuses** `true` unless an `eval.mini.pass` event (written by the W7 mini-eval, W78-01) exists in the last 30 days.
- Tests: `kahyad/internal/consolidation/*_test.go` — commit discipline, user-line skip, secret-lane file never in cloud envelope, markdown+git-only writes, hot-window promotion on synthetic >90-day fixtures.

## Steps

1. Read HANDOFF §6 W5 + backup ⚑, §5 memory #4, §4 routing/ordering invariant. Inspect W12-04 indexer and W4-01 scheduler APIs as built.
2. Implement the pre-run guard: if `~/Kahya` working tree is dirty, commit everything as `--author="user <user@kahya.local>"` with message `user edits before consolidation`.
3. Compute the user-touched line set for the day (diff of all `author=user` commits since 00:00 local). Partition memory files by policy.yaml secret-lane globs into cloud-lane and local-lane sets.
4. If a pending suggestion exists, supersede it per the collision rule in Context (delete branch/worktree, ledger `consolidation.superseded`). Create the worktree + dated branch. Spawn the consolidation session(s) per lane (envelope model: `claude-haiku-4-5` cloud lane / `Qwen3-30B-A3B` local lane; local unavailable ⇒ skip lane, notify `"yerel model için bellek yok — gizli-şerit dosyaları bu gece atlandı"`). The session receives file contents and returns whole-file rewrites; kahyad writes them into the worktree only.
5. Enforce the skip rule Go-side: any hunk overlapping a user-touched line from step 3 is dropped before staging (user_edit wins).
6. Run hot-window pass: episodes older than 90 days and not yet cooled → promote detail atoms (sayı/tarih/alıntı/karar/söz) to `agent_derived` candidate facts via kahyad's fact-write path; only then mark cooled. Summaries must cite episode/chunk ids as raw evidence.
7. Commit on the branch as `author=kahyad`. Suggestion mode: store pending-diff state, notify (Telegram, redacted per W5-01 rules): `"Konsolidasyon önerisi hazır — kahya consolidation show"`. Auto mode (guarded): merge to main directly.
8. On `kahya consolidation approve`: merge to main, delete worktree/branch, trigger `kahya reindex` (kahyad-internal call — the session never touches brain.db), then run the W4-06 nightly `git push`. On reject: delete branch, ledger the rejection.
9. Register the 03:00 job; add `kahya job run nightly-consolidation` manual trigger.
10. Write the tests in Deliverables; wire into `make test`.

## Acceptance criteria

- [ ] `kahya job run nightly-consolidation` on a seeded corpus produces a pending diff; `kahya consolidation show` renders it; `kahya consolidation approve` results in `git log ~/Kahya` showing a separate `author=kahyad <kahyad@kahya.local>` commit (and, if the tree was dirty, a preceding `author=user` commit) — mirrors §6 W5 gate "gece külliyat konsolide olup diff commit ediliyor".
- [ ] Test in `make test`: a line edited by the user the same day is byte-identical after consolidation even when the model proposed changing it (user_edit wins).
- [ ] Test in `make test`: a file matching a secret-lane glob never appears in a cloud-lane envelope (assert against the forward-proxy/request log); with local model unavailable the file is skipped and the Turkish notice is emitted — no cloud fallback.
- [ ] Test in `make test`: consolidation performs zero writes outside the `~/Kahya` worktree and never opens brain.db; reindex is observed only as a kahyad-triggered event after approval.
- [ ] Test in `make test`: synthetic 91-day-old episode fixture gets its detail atoms promoted to `source_tier=agent_derived` facts before cooling; a summary referencing a sub-summary as evidence is rejected.
- [ ] With no `eval.mini.pass` event present, setting `consolidation.auto_commit: true` makes kahyad log an error and stay in suggestion mode (test).
- [ ] Test in `make test`: a second nightly run while a suggestion is pending deletes the stale branch, ledgers `consolidation.superseded`, and produces a fresh pending diff — the stale diff is never merged.
- [ ] After approve, `git -C ~/Kahya log origin/main..main` is empty (nightly push ran).

## Out of scope

- Auto-commit **enablement** — stays off until the W7 mini-eval is green (W78-01 writes `eval.mini.pass`).
- Fact lattice/merge/log-odds correctness logic — W5-04 (this task only emits quarantined `agent_derived` candidates).
- Retrieval mini-baseline regression check — W5-05. Full eval set — W78-01.
- Re-embedding / `model_ver` migration (W12-11), SwiftUI memory browser, two-temporal graph queries, embedded NATS (HANDOFF §8).
- brain.db `VACUUM INTO` backups — W4-06 owns them.
