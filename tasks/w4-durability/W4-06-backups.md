# W4-06 — Backups: nightly VACUUM INTO, retention, git push, Keychain-loss runbook

**Status:** done
**Phase:** W4 — Durability
**Depends on:** W4-01, W0-01
**Flags:** none
**Handoff refs:** §6 backup ⚑, §9 (Dosya düzeni + Yedekleme), §6 W7–8 (restore drill context)

## Goal

brain.db and the memory corpus stop being single-copy data: a nightly job produces a verified
`VACUUM INTO` snapshot of brain.db with 7-day retention and correct Time Machine exclusions,
a nightly job pushes `~/Kahya` to its private remote, and a Keychain-loss runbook exists. This
is the "sıfır veri-kaybı" prerequisite for MVP-done (§9) and for the W78-05 restore drill.

## Context you need

Binding text (HANDOFF §6, quote verbatim):

> ⚑ **Yedekleme (W4 iş kalemi — "sıfır veri-kaybı" kabul kriterinin gereği):** (1) `~/Kahya` → private git remote; W5 gecelik commit'in sonuna `git push`; (2) gecelik `VACUUM INTO ~/Kahya/backups/brain-YYYYMMDD.db` + `PRAGMA integrity_check` (son 7 kopya; canlı WAL db Time Machine'den dışlanır, VACUUM kopyası dahil edilir) — **defter/episodes markdown'dan türetilemez, tek kopyadır**; (3) Keychain sırları kayıpta API-key rotasyonuyla yeniden üretilir.

- Why this matters (tasks/README.md convention): the SQLite index can always be rebuilt from
  `~/Kahya/memory/*.md` — EXCEPT the ledger (`events`) and `episodes`, which exist only in
  brain.db. Losing brain.db without a snapshot is unrecoverable data loss.
- W0-01 created `~/Kahya/{memory,backups}` as a git repo with a private remote and the seed
  commit. W4-01 provides the job registry (`jobs:` config + `kahya-trigger`).
- kahyad is the single writer of brain.db, so running `VACUUM INTO` on kahyad's own connection
  is a consistent online backup even mid-WAL (SQLite guarantees a transactional snapshot).
- `VACUUM INTO` fails if the target file exists — handle same-day reruns.
- W5-02 will later chain a push after the consolidation commit; tonight's `memory-push` job
  simply pushes whatever HEAD is — the two do not conflict.

## Deliverables

- `kahyad/internal/backup/backup.go` + `backup_test.go` — snapshot, verify, prune
- `kahyad/internal/backup/gitpush.go` + test — `git -C ~/Kahya push` wrapper
- Job registrations in kahyad config: `backup-nightly` (03:30) and `memory-push` (03:45),
  handlers registered via the W4-01 scheduler
- Time Machine exclusion setup in kahyad startup (idempotent) using `tmutil addexclusion`
- `~/Kahya/.gitignore` entry: `backups/` (see step 5)
- `docs/runbooks/keychain-loss.md`

## Steps

1. Snapshot handler (`backup-nightly`):
   a. Target `~/Kahya/backups/brain-YYYYMMDD.db` (local date). If it exists (rerun), delete it
      first — a same-day rerun replaces, never appends.
   b. Run `VACUUM INTO '<target>'` on the live kahyad connection.
   c. Open the copy read-only; `PRAGMA integrity_check;` must return exactly `ok` — otherwise
      delete the corrupt copy, keep older copies untouched, append ledger event
      `backup.failed`, and alarm (Turkish, exact):
      `Gece yedeği BAŞARISIZ: <sebep>. brain.db'nin tek kopyası risk altında.`
   d. On success: ledger event `backup.completed` with `{path, bytes, sha256}`.
   e. Prune: keep the 7 newest `brain-*.db` in `~/Kahya/backups`, delete older. Prune runs
      ONLY after a successful verify (never reduce good copies on a failure night).
2. `memory-push` handler: `git -C ~/Kahya push origin HEAD`. Failure ⇒ ledger `backup.push_failed`
   + alarm (Turkish, exact): `Hafıza deposu push BAŞARISIZ: <sebep>. Uzak yedek güncel değil.`
   with `<sebep>` = git stderr first line (technical output stays English per §3). No commit is
   created here — committing is W0-01 (seed) / W5-02 (consolidation) territory.
3. Time Machine (idempotent at kahyad startup): `tmutil addexclusion` for
   `~/Library/Application Support/Kahya/brain.db`, `brain.db-wal`, `brain.db-shm`;
   assert `~/Kahya/backups` is NOT excluded (if `tmutil isexcluded` says excluded, remove the
   exclusion). Then check `tmutil destinationinfo`: because `backups/` is gitignored (step 5),
   Time Machine is the ONLY off-machine copy of brain.db snapshots — §9 says the ledger
   "tek diskte kalamaz". If no destination is configured, ledger `backup.no_offsite` + alarm
   (Turkish, exact): `Time Machine hedefi yok: brain.db yedekleri makine dışına kopyalanmıyor.`
   (alarm at most once per 24h; do not block startup). Log the result once per startup.
4. Register both jobs in the `jobs:` config section (W4-01 syncs the launchd plists):
   `backup-nightly` `{Hour: 3, Minute: 30}`, `memory-push` `{Hour: 3, Minute: 45}`.
5. Add `backups/` to `~/Kahya/.gitignore` (commit in `~/Kahya`, author=user is fine — this is
   a user-owned repo config change). Rationale: the ⚑ routes DB snapshots through Time Machine
   ("VACUUM kopyası dahil edilir") and markdown through git; pushing daily DB binaries would
   bloat the private remote. Note this in the commit message.
6. `docs/runbooks/keychain-loss.md` — numbered recovery steps per ⚑ item (3):
   rotate the Anthropic API key in the console; regenerate the Telegram token via BotFather;
   create a new anchor deploy key + update the remote repo; re-add all three items
   (`kahya.anthropic`, `kahya.telegram`, `kahya.anchor`) with
   `security add-generic-password -U -s <service> -a kahya -T "$(which kahyad)" -w`;
   verify with `security find-generic-password -s kahya.anthropic -a kahya` and a kahyad
   health-check round trip. State explicitly: no memory/ledger data is lost — secrets are
   re-creatable, brain.db is not.
7. Tests (temp dirs + in-memory-ish SQLite, all in `make test`):
   - Snapshot + verify happy path; `sha256` in the event matches the file.
   - Concurrent-writer test: a goroutine inserts events during `VACUUM INTO`; the copy passes
     `integrity_check` = `ok`.
   - Rerun same day replaces the file (mtime/sha change, count unchanged).
   - Prune: create 9 dated fakes → 7 newest remain.
   - Corrupt-copy path (inject a failing verifier): older copies untouched, `backup.failed`
     event with the exact Turkish alarm string, non-`ok` handled.
   - `memory-push` against a `file://` bare remote: remote HEAD advances; failure path ledgers
     `backup.push_failed` with the exact push-failure Turkish string.
   - No-TM-destination path (inject `tmutil destinationinfo` output): `backup.no_offsite`
     event with the exact Turkish string; alarm rate-limited to once per 24h (fake clock).

## Acceptance criteria

- [x] `make test` green including all step-7 tests.
- [ ] `launchctl print gui/$(id -u)/com.kahya.job.backup-nightly` and `...memory-push` both
      loaded after `kahyad -sync-jobs`. **Not run in this session** (build-sandbox
      constraint: no real launchd calls) — `kahyad/internal/scheduler`'s existing
      Sync/plist tests cover the rendering logic; user must verify this specific
      command on the real machine after `make install-agent`.
- [ ] Manual: `kahya-trigger backup-nightly` then
      `sqlite3 ~/Kahya/backups/brain-$(date +%Y%m%d).db "PRAGMA integrity_check;"` prints `ok`,
      and `sqlite3 brain.db "SELECT kind FROM events ORDER BY id DESC LIMIT 1;"` shows
      `backup.completed`. **Not run in this session** (would touch the real ~/Kahya and
      real brain.db) — equivalent behavior is proven by backup_test.go's
      TestRunSnapshotVerifyHappyPath against a real temp-dir SQLite store.
- [ ] `tmutil isexcluded ~/Library/Application\ Support/Kahya/brain.db` reports `[Excluded]`;
      `tmutil isexcluded ~/Kahya/backups` reports `[Included]`. **Not run in this session**
      (real tmutil call, explicitly out of scope for the build sandbox) — the
      exclusion logic itself is unit-tested against a fake TMRunner in tm_test.go.
- [ ] `kahya-trigger memory-push` advances the private remote:
      `git -C ~/Kahya ls-remote origin HEAD` equals local `git -C ~/Kahya rev-parse HEAD`.
      **Not run against the real ~/Kahya/remote in this session** — the equivalent
      real-git behavior is proven by gitpush_test.go's TestPusherRunAdvancesRemoteHEAD
      against a temp-dir file:// bare remote.
- [x] `docs/runbooks/keychain-loss.md` exists and contains all three `security
      add-generic-password` recovery commands.

## Out of scope

- The restore drill on a clean machine + `docs/restore-runbook.md` — W78-05.
- The nightly consolidation commit and its chained push — W5-02 (only tonight's plain push
  job lives here).
- SQLCipher or any at-rest encryption change — §8 deferred (FileVault + Keychain is the locked
  §4 decision).
- Off-site replication of brain.db snapshots beyond Time Machine (the ⚑ names Time Machine;
  do not add S3/rsync targets).
- Anchor repo backups (W4-05 owns the anchor remote).
