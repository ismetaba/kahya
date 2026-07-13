# Backup restore runbook (W78-05)

How to restore Kâhya's `brain.db` from a nightly backup and **prove** the
restore is faithful: the same query must yield the same `<hafiza>` injection
block it did in production, and the ledger (`events`) and `episodes` — which
are **not** derivable from the markdown corpus — must come back intact.

**The one fact that matters most.** The memory *source of truth* is Markdown +
git (`~/Kahya/memory/*.md`), and the SQLite index over it is fully
rebuildable. The exception is the **ledger (`events`) and `episodes`**: they
live only in `brain.db` and cannot be regenerated from markdown. That is why
the W4-06 nightly `VACUUM INTO ~/Kahya/backups/brain-YYYYMMDD.db` copy is the
single restorable artifact — the live WAL `brain.db` is **excluded from Time
Machine**, and only the VACUUM copy is included (HANDOFF §6 backup ⚑).

**This procedure never touches production.** The restore is brought up on an
isolated scratch profile (`KAHYA_ENV=restore`): a separate `brain.db`, a
separate socket, and a separate memory clone. Nothing here opens, migrates, or
reindexes the production `brain.db`. The automated drill
(`scripts/restore-drill.sh`, `make restore-drill`) enforces this with a
fail-closed guard; this document is the manual clean-machine procedure.

Paths referenced (defaults; a `config.yaml` override moves them):

| Role | Production | Restore scratch (`KAHYA_ENV=restore`) |
| --- | --- | --- |
| memory repo | `~/Kahya` | `~/Kahya-restore` |
| App Support | `~/Library/Application Support/Kahya` | `~/Library/Application Support/Kahya-restore` |
| brain.db | `.../Kahya/brain.db` | `.../Kahya-restore/brain.db` |
| control socket | `.../Kahya/kahyad.sock` | `.../Kahya-restore/kahyad-restore.sock` |
| backups | `~/Kahya/backups/brain-YYYYMMDD.db` | (source of the restore) |

---

## 1 · Clone the memory repo from the private remote

The markdown corpus is a git repo pushed nightly to a private remote (W4-06's
`git push`). On a clean machine, clone it into the scratch memory path:

```bash
git clone <your-private-Kahya-remote> ~/Kahya-restore
```

On the same machine where production still exists, a local clone is equivalent
and is what the automated drill does:

```bash
git clone ~/Kahya ~/Kahya-restore
```

> **Markdown/backup sync gotcha.** The clone and the backup you restore must be
> taken at the **same point in time**. If `~/Kahya` has commits newer than the
> backup, reindexing the clone legitimately changes the index and the
> equivalence diff fails *for the wrong reason*. The automated drill avoids
> this by triggering a fresh W4-06 backup and recording the production
> reference at that same instant; a manual run should clone right after the
> backup you intend to restore was produced.

## 2 · Pick the latest backup

```bash
ls -1 ~/Kahya/backups/brain-*.db | sort | tail -1
```

Backups are named `brain-YYYYMMDD.db` (W4-06 keeps the newest 7). Pick the
newest whose `PRAGMA integrity_check` is `ok` (step 4 verifies this).

## 3 · Place the backup at the scratch brain.db path

```bash
mkdir -p ~/Library/Application\ Support/Kahya-restore
cp ~/Kahya/backups/brain-YYYYMMDD.db \
   ~/Library/Application\ Support/Kahya-restore/brain.db
```

Never copy over the production `brain.db`. The scratch App Support directory is
`Kahya-restore`, a sibling of `Kahya`.

## 4 · Verify integrity

```bash
sqlite3 ~/Library/Application\ Support/Kahya-restore/brain.db 'PRAGMA integrity_check;'
# must print exactly: ok
```

## 5 · Boot the scratch kahyad (migrations run, user_version verified)

Bring up kahyad under the restore profile. It binds its **own** socket
(`kahyad-restore.sock`), runs every pending goose migration at open, and sets
`PRAGMA user_version` to the resulting schema version:

```bash
KAHYA_ENV=restore ./bin/kahyad &
# in another shell, confirm the schema version matches the binary's expectation:
sqlite3 ~/Library/Application\ Support/Kahya-restore/brain.db 'PRAGMA user_version;'
```

`config.refuseNonProdProfileOpeningProdDB` fails startup closed if a non-prod
profile ever resolves to the production `brain.db` path — a second, independent
guard against clobbering prod.

## 6 · Reindex from the cloned markdown — must be a no-op

```bash
curl -sS --unix-socket ~/Library/Application\ Support/Kahya-restore/kahyad-restore.sock \
  -X POST http://kahyad/v1/reindex -H 'Content-Type: application/json' -d '{"full":false}'
```

Because the clone and the backup were taken at the same point, the incremental
(file-hash) reindex must be a **no-op**: `files_indexed`, `files_removed`, and
`chunks` all `0`, every file `unchanged`. A non-empty diff proves
markdown↔index drift and **fails** the drill — do not proceed as if the restore
were clean.

## 7 · Run the equivalence query and diff

Run the same query production answered and compare the `<hafiza>` block. The
`for_injection:true` response renders the block through the exact injection path
(FTS retrieval → tier-eligibility filter → renderer):

```bash
curl -sS --unix-socket ~/Library/Application\ Support/Kahya-restore/kahyad-restore.sock \
  -X POST http://kahyad/v1/memory/search -H 'Content-Type: application/json' \
  -d '{"query":"evlerimizden","k":8,"for_injection":true,"task_id":"restore-drill"}'
```

Normalize **only** volatile fields — `trace_id` (32-hex) and RFC3339/Nano
timestamps — then byte-compare the `hafiza_block` against the production
reference. Any other difference fails. (The automated drill does this with
`restore.Normalize`; see `scripts/restore-drill.sh`.)

Then confirm the ledger and episodes survived (they cannot come from markdown):

```bash
sqlite3 ~/Library/Application\ Support/Kahya-restore/brain.db \
  'SELECT (SELECT count(*) FROM events), (SELECT count(*) FROM episodes);'
```

Both counts must be **≥** the production reference counts recorded at backup
time.

## 8 · Active model_ver must match

The scratch daemon reuses the same local MLX embedding model files read-only.
The backup's active `model_ver` (`config.active_embed_model_ver`) must equal the
live one — **mixed-version KNN is forbidden** (HANDOFF §4 ⚑). If the machine was
rebuilt, re-download the pinned embedding model (W0-03) *before* running the
equivalence query, or the vector leg is unavailable and search silently
degrades to FTS-only (still correct for this drill, but not a full-fidelity
restore).

## 9 · Keychain loss (secrets are NOT in backups, by design)

Backups contain **no secrets**. If the Keychain was also lost, the three items
kahyad depends on must be rotated, not restored:

- `kahya.anthropic` — Anthropic API key
- `kahya.telegram` — Telegram BotFather token
- `kahya.anchor` — W4-05 ledger-anchor deploy key

Follow `docs/runbooks/keychain-loss.md` for the exact rotation commands. Losing
Keychain access is a rotation errand; losing `brain.db` without a verified
backup is unrecoverable — which is the entire point of this drill.

---

## Recording the result (production ledger)

On a real drill run, once every check above passes, the result is reported to
the **production** kahyad over its UDS, which appends a
`restore.drill.result` event (`{ok, ref_query_sha, backup_file, trace_id}` —
counts/hashes/flags only, **no memory content**). kahyad remains the sole
`brain.db` writer; the drill script never writes SQL to production. This row is
the evidence W78-06 readiness reads. `make restore-drill` automates steps 1–9
and this reporting step end to end.
