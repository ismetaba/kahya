# W78-05 — Backup restore drill + runbook

**Status:** todo
**Phase:** W7–8 — Hardening + eval
**Depends on:** W4-06, W12-10, W78-02 (reuses its `KAHYA_ENV` profile path-resolution)
**Flags:** none
**Handoff refs:** §6 W7–8 acceptance, §6 backup ⚑

## Goal

Rehearse a full restore on a clean environment and prove data integrity: a fresh clone of
`~/Kahya` plus the latest `brain-*.db` backup, brought up on a scratch profile, yields the
**same `<hafiza>` injection for the same query** as production. The procedure is captured
in `docs/restore-runbook.md`.

## Context you need

Binding HANDOFF items (verbatim):

> → **Kabul:** … bir kez yedekten geri-dönüş tatbik edildi (temiz makinede aynı sorguya aynı `<hafiza>` enjeksiyonu). Sonra 2 hafta gerçek günlük dogfood başlıyor.

> ⚑ **Yedekleme (W4 iş kalemi — "sıfır veri-kaybı" kabul kriterinin gereği):** (1) `~/Kahya` → private git remote; W5 gecelik commit'in sonuna `git push`; (2) gecelik `VACUUM INTO ~/Kahya/backups/brain-YYYYMMDD.db` + `PRAGMA integrity_check` (son 7 kopya; canlı WAL db Time Machine'den dışlanır, VACUUM kopyası dahil edilir) — **defter/episodes markdown'dan türetilemez, tek kopyadır**; (3) Keychain sırları kayıpta API-key rotasyonuyla yeniden üretilir.

Why brain.db backup is non-negotiable (README + §5): memory source of truth is Markdown +
git and can be rebuilt, **but the ledger (`events`) and `episodes` exist only in brain.db** —
they are not derivable from markdown. A restore drill must prove BOTH survive: markdown-derived
index rebuilds identically AND the ledger/episodes come back from the VACUUM copy.

The equivalence check must go through the real injection path: the W12-10 acceptance already
established that a query injects a `<hafiza>` block. The drill re-runs that exact query on the
restored scratch profile and byte-compares the injected block (modulo volatile fields like
timestamps/trace_id, which are normalized before comparison).

You build on: W4-06 (nightly `VACUUM INTO` + `integrity_check` + `git push`), W12-10
(end-to-end injection acceptance), W12-04 corpus indexer (rebuild from markdown), and the
W78-02 `KAHYA_ENV` profile resolution — the injection path lives **in kahyad**, so the drill
boots a **scratch kahyad** under `KAHYA_ENV=restore` (paths resolve to
`~/Library/Application Support/Kahya-restore/` + `~/Kahya-restore`) on its own socket. The
scratch daemon reuses the same local MLX embedding model files read-only, and the backup's
active `model_ver` must match the live one (the drill fails otherwise — mixed-version KNN is
forbidden per §4 ⚑).

Two gotchas a naive drill gets wrong:
1. **Markdown/backup sync** — if `~/Kahya` has commits newer than the backup, reindexing the
   clone legitimately changes the index and the injection diff fails for the wrong reason.
   The drill therefore first triggers a fresh W4-06 backup and records the production
   reference injection at that same point in time.
2. **The handoff says "temiz makinede"** — the automated drill uses the isolated scratch
   profile as the clean-machine stand-in (zero reads from production paths, enforced by the
   guard); the runbook additionally documents the true clean-machine procedure (model
   re-download, Keychain rotation, launchd install), which is not automatable here.

## Deliverables

- `docs/restore-runbook.md` — step-by-step restore procedure: clone `~/Kahya` from the private remote, pick the latest `brain-YYYYMMDD.db`, place it at the scratch profile's App Support path, run migrations/verify `user_version`, reindex from markdown, verify. Includes the Keychain-loss note (rotate `kahya.anthropic`/`kahya.telegram`/`kahya.anchor`).
- `scripts/restore-drill.sh` — automates the drill against the **scratch** profile `KAHYA_ENV=restore` (`~/Kahya-restore` + `~/Library/Application Support/Kahya-restore/`), never touching production; boots the scratch kahyad, runs the equivalence query through it, diffs. Drill artifacts containing memory content (the reference `<hafiza>` block) are stored under `~/Kahya/backups/drill/` — never committed to the code repo.
- `kahyad/internal/restore/drill_test.go` (or a `kahya restore-drill --check` subcommand) — programmatic equivalence assertion used in `make test` with a fixture backup.
- `Makefile`: `make restore-drill` target.

## Steps

1. Read §6 W7–8 acceptance + §6 backup ⚑ and the W4-06 backup outputs (backup filenames, integrity_check policy, git push).
2. Write `docs/restore-runbook.md` with the full manual procedure, including exact paths: source `~/Kahya` (private remote), backup `~/Kahya/backups/brain-YYYYMMDD.db`, target scratch App Support `~/Library/Application Support/Kahya-restore/brain.db`. Document that the live WAL db is Time-Machine-excluded and the VACUUM copy is the restorable artifact.
3. Implement `scripts/restore-drill.sh`: (a) trigger a fresh W4-06 backup now (`VACUUM INTO` + `integrity_check`) and record the production reference at the same point: run the equivalence query through production kahyad, save the injected `<hafiza>` block + `events`/`episodes` row counts to `~/Kahya/backups/drill/reference.json`; (b) fresh `git clone` of `~/Kahya` into `~/Kahya-restore`; (c) copy that backup to `~/Library/Application Support/Kahya-restore/brain.db`; (d) `PRAGMA integrity_check` (must be `ok`); (e) boot the scratch kahyad (`KAHYA_ENV=restore`, own socket) — goose migrations run at startup; confirm `PRAGMA user_version` matches the binary's expected version; (f) reindex from `~/Kahya-restore/memory` — this must be an incremental **no-op** since clone and backup were taken at the same point (a non-empty reindex diff fails the drill: it proves markdown↔index drift); (g) run the same equivalence query through the scratch daemon's injection path; (h) normalize ONLY `trace_id` and timestamp fields (documented regex), then byte-compare the `<hafiza>` block against the reference — any other difference fails.
4. Prove the ledger/episodes survive: assert `SELECT count(*) FROM events` and `FROM episodes` on the restored db are ≥ the recorded reference counts (these cannot come from markdown, so this proves the VACUUM copy is intact).
5. Add the programmatic `drill_test.go` (fixture `~/Kahya` + fixture backup db shipped in `testdata/`) so `make test` runs the equivalence assertion hermetically.
6. Guardrails: the script refuses to run if the target path resolves to the production App Support dir or `~/Kahya` (fail-closed against clobbering prod).
7. Run `make test`, `make lint`, then `make restore-drill` for real; on success the script reports the result to **production kahyad over its UDS**, and kahyad records `type=restore.drill.result {ok, ref_query_sha, backup_file, trace_id}` — the script never writes SQL to production brain.db itself (kahyad is the sole writer). This row is the evidence W78-06 readiness reads.

## Acceptance criteria

- [ ] `make restore-drill` completes: fresh backup + synced scratch clone ⇒ `PRAGMA integrity_check` = `ok`, `user_version` matches, reindex is an incremental no-op, and the equivalence query through the **scratch kahyad** yields a `<hafiza>` block byte-identical (after trace_id/timestamp normalization only) to the production reference.
- [ ] Restored db `events` and `episodes` row counts ≥ reference counts (proves the ledger/episodes — not derivable from markdown — were restored from the VACUUM copy).
- [ ] `docs/restore-runbook.md` exists with exact paths and the Keychain-rotation note.
- [ ] Drill script refuses to target the production App Support path or `~/Kahya` (tested).
- [ ] `restore.drill.result` event recorded by **production kahyad via UDS** after a successful drill; the drill script itself never opens production brain.db for writing (tested).
- [ ] `drill_test.go` runs in `make test` with `testdata/` fixtures (no network, no production paths); `make test` and `make lint` green.

## Out of scope

- The backup mechanism itself (W4-06 owns `VACUUM INTO`, integrity_check, git push, Time Machine exclusion).
- External ledger anchor verification (W4-05 owns anchor mismatch detection); this drill restores brain.db, it does not re-anchor.
- Automated periodic restore drills — one rehearsed drill satisfies the MVP gate; scheduling is post-MVP.
- Keychain secret restoration beyond documenting the rotate-keys runbook (secrets are not in backups by design).
