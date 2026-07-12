# W4-05 — Ledger external anchor push + tamper detection

**Status:** code-complete (real-remote verification blocked-user)
**Phase:** W4 — Durability
**Depends on:** W12-02, W0-04, W4-01 (step 3 uses its `RegisterTick` API)
**Flags:** user-assist
**Handoff refs:** §5 safety #4 ⚑, §7 secrets ⚑, §6 W4

## Goal

The append-only ledger (`events` in brain.db) becomes tamper-evident: kahyad keeps a running
digest over ledger events, periodically anchors the latest digest to an append-only,
separately-credentialed remote, and `kahya ledger verify` detects any local rewrite against the
anchors and alarms. This closes the "defter yerelden değiştirilince uzak çapayla uyumsuzluk
tespit edilip alarm veriyor" leg of the W4-07 gate.

## Context you need

Binding invariant (HANDOFF §5 safety #4, quote verbatim):

> **Dış-çapalı defter.** Ayrıcalıklı taraf tek denetim otoritesi; zincir başı daemon'ın yeniden-yazamayacağı ayrı-yetkili bir depoya periyodik yazılır. Her model çağrısındaki enjekte `<hafiza>` bloğu kaydedilir (zehirlenme adli izlenebilirliği).

> ⚑ **Somut mekanizma (W4):** kahyad her N saatte defterin son event hash'ini **yalnız-append yetkili ayrı uzak hedefe** yazar (force-push kapalı, append-only deploy-key'li ayrı git repo'su, ya da S3 Object Lock; offline'da farklı-uid'li yerel ekleme-yalnız dosya). Bu kimlik Keychain'de **ayrı öğedir**, yalnız çapa-yazma kod yolunda okunur.

- HANDOFF §8 defers "hash-zincir tiyatrosu (dış-çapa yeter)": do NOT build Merkle trees or a
  transparency log. The minimal mechanism that makes an anchored "last event hash" meaningful
  is a running digest — implement exactly that and nothing fancier.
- W0-04 created the Keychain item `kahya.anchor`
  (`security add-generic-password -s kahya.anchor -a kahya -T "$(which kahyad)" -w`) — the user
  supplies the actual deploy key. This identity is a SEPARATE Keychain item read only by the
  anchor code path (verbatim rule above).
- The primary remote choice here is the append-only deploy-key **git repo** (S3 Object Lock is
  the documented alternative, not built). The user must create it — user-assist.
- W4-01 tick API exists for the in-daemon cadence (anchor cadence is not wall-clock-critical:
  if the daemon is asleep, no new events were written either).

## Deliverables

- `kahyad/migrations/<next-goose-seq>_ledger_anchor.sql` — `ledger_digest_state(id INTEGER
  PRIMARY KEY CHECK(id=1), last_event_id INTEGER NOT NULL, digest BLOB NOT NULL)` and
  `anchor_log(id INTEGER PRIMARY KEY, event_id INTEGER NOT NULL, digest_hex TEXT NOT NULL,
  anchored_at TEXT NOT NULL, remote_ref TEXT, status TEXT NOT NULL CHECK(status IN
  ('pending','pushed')))`
- `kahyad/internal/anchor/digest.go`, `push.go`, `verify.go` + tests
- `kahyad/internal/anchor/anchor_import_guard_test.go` — Keychain-isolation guard
- `kahya ledger verify` subcommand in `kahyad/cmd/kahya/`
- Config keys: `anchor.remote` (git URL), `anchor.interval_hours` (default 6),
  `anchor.local_fallback_path` (optional)
- `docs/runbooks/anchor-setup.md` — the user-facing setup steps (step 7)

## Steps

1. Running digest: on every ledger append, inside the SAME SQLite transaction, update
   `ledger_digest_state`: `digest = SHA256(prev_digest || uint64_be(event_id) ||
   event_payload_bytes)` where `event_payload_bytes` are the exact stored JSON bytes of the
   event row. Genesis `prev_digest` = 32 zero bytes. Deterministic by construction — golden
   test with 3 fixed events.
2. Anchor record format (one line, append-only file `anchors.log` in the anchor repo):
   `<event_id> <digest_hex> <RFC3339 timestamp> <hostname>\n`.
3. Push path (`push.go`): every `interval_hours` (W4-01 `RegisterTick`), plus once at startup
   and once at graceful shutdown: read `ledger_digest_state`; skip if `last_event_id` already
   anchored; insert `anchor_log` row `pending`; clone-or-pull the anchor repo to
   `~/Library/Application Support/Kahya/anchor-repo`; append the line; commit
   (`author=kahyad-anchor`); push. On success mark `pushed` + ledger event `anchor.pushed`.
   Push is plain `git` exec with `GIT_SSH_COMMAND="ssh -i <tmpkey> -o IdentitiesOnly=yes"`,
   where `<tmpkey>` is the `kahya.anchor` Keychain value written to a 0600 temp file under
   App Support and removed in a defer.
4. Keychain isolation: the accessor (e.g. `keychain.AnchorDeployKey()`) may be referenced ONLY
   from `kahyad/internal/anchor`. `anchor_import_guard_test.go` walks the repo's Go sources and
   fails if the symbol name appears outside that package (cheap, permanent §5-#4 guard).
5. Offline behavior: if the remote is unreachable, leave rows `pending` and retry next tick;
   if the oldest `pending` is older than `2 × interval_hours`, alarm (Turkish):
   `DEFTER UYARISI: çapa uzak hedefe <saat> saattir yazılamıyor.` If
   `anchor.local_fallback_path` is set, ALSO append every anchor line there with `O_APPEND`.
6. Verify (`verify.go`, exposed as `kahya ledger verify`): recompute the digest from event 1
   upward; at each `anchor_log`/remote anchor line, compare. Also pull the remote and compare
   its last line against local `anchor_log`. Any mismatch ⇒ exit non-zero, ledger event
   `anchor.mismatch`, and alarm (Turkish, exact):
   `DEFTER UYARISI: yerel defter uzak çapayla uyuşmuyor (event <id>). Olası kurcalama — hemen incele.`
   (Alarm delivery = existing notification channel; Telegram gets this title only — it carries
   no ledger content, so §5 #5 secret-lane redaction is satisfied.) Full recompute is fine at
   MVP scale (≤~100k events); do not optimize.
7. Write `docs/runbooks/anchor-setup.md` telling the user exactly what to do, then set
   `Status: blocked-user` if the remote/key is missing when you reach integration testing:
   - Create a private git repo used ONLY for anchors; add a write deploy key; on GitHub enable
     branch protection on the default branch: block force pushes + block deletions.
   - Put the private key into Keychain: `security add-generic-password -U -s kahya.anchor -a kahya -T "$(which kahyad)" -w`
   - Set `anchor.remote` in kahyad config.
   - Optional offline fallback (different-uid local append-only file, per the ⚑; run once):
     ```bash
     sudo mkdir -p /usr/local/var/kahya
     sudo touch /usr/local/var/kahya/anchor.log
     sudo chown root:wheel /usr/local/var/kahya/anchor.log
     sudo chmod 0622 /usr/local/var/kahya/anchor.log
     sudo chflags sappnd /usr/local/var/kahya/anchor.log   # kernel-enforced append-only
     ```
     then set `anchor.local_fallback_path: /usr/local/var/kahya/anchor.log`.
8. Tests: digest golden test; push against a local `file://` bare repo (no SSH in CI) — one
   commit per anchor, `git rev-list --count` increments, earlier lines byte-identical after a
   second push; pending-buffer + stale-pending alarm with fake clock; tamper test — after 2
   anchors, `UPDATE events SET payload=... WHERE id=1` via a raw connection, then verify ⇒
   non-zero + `anchor.mismatch` event; import-guard test.

## Acceptance criteria

- [x] `make test` green including digest golden, file://-remote push, pending alarm, tamper,
      and import-guard tests.
- [ ] Manual (real remote, after user supplies it): two anchor ticks produce two lines in the
      remote repo's `anchors.log`; `git log` on the remote shows append-only history.
      **blocked-user: user must create the anchor repo + add kahya.anchor to Keychain (see
      docs/runbooks/anchor-setup.md) before this can be exercised against a real remote.**
- [x] Tamper drill: `sqlite3 brain.db "UPDATE events SET payload=json_set(payload,'$.k','x') WHERE id=(SELECT MIN(id) FROM events);"`
      then `kahya ledger verify` exits non-zero and prints the exact mismatch string above;
      an `anchor.mismatch` event exists. (This is the W4-07 third leg — must pass here first.)
      Verified hermetically in `kahyad/internal/anchor/verify_test.go`
      (`TestVerifyDetectsLocalTamperAfterTwoAnchors`) against a real `file://` bare remote —
      the literal `sqlite3 ... UPDATE events` command only succeeds once the append-only
      `events_no_update` trigger is dropped first (a raw connection can always do this before
      rewriting history), which the test does explicitly and documents as the realistic threat
      model this external anchor exists for.
- [x] `grep -rn "AnchorDeployKey" kahyad/ --include='*.go' | grep -v internal/anchor` returns
      nothing (mirrors the guard test).
- [x] JSONL log lines for push/verify carry a `trace_id` (per-tick minted, README convention).

## Out of scope

- Merkle trees / transparency logs / per-event hash-chain APIs — §8 "hash-zincir tiyatrosu"
  deferred; the running digest is the floor, not the start of a ladder.
- S3 Object Lock backend (document as alternative in the runbook; do not implement).
- Ledgering of injected `<hafiza>` blocks — W12-10 owns that (it feeds this digest for free).
- Backups of brain.db (W4-06) and the full W4-07 gate run.
- Any change to ledger write paths beyond the same-transaction digest update.
