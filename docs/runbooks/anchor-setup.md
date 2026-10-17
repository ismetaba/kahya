# Ledger external anchor setup (W4-05)

This is the one-time, user-assist setup HANDOFF §5 safety #4 requires
(quoted verbatim):

> **Dış-çapalı defter.** Ayrıcalıklı taraf tek denetim otoritesi; zincir
> başı daemon'ın yeniden-yazamayacağı ayrı-yetkili bir depoya periyodik
> yazılır.

kahyad already implements the mechanism (running digest over the `events`
ledger, anchored to a remote every `anchor.interval_hours`, `kahya ledger
verify` detecting local tampering). What only YOU can do is create the
separately-credentialed remote and hand kahyad its own, narrowly-scoped
deploy key. Do these steps in order.

## 1 · Create a private, append-only-only git repo

1. Create a **new, empty, private** git repository on whatever host you
   use (GitHub/GitLab/a self-hosted server, or a bare repo on a second
   machine reachable over SSH) — used for **nothing except this anchor**.
   Do not reuse `~/Kahya`'s own memory repo or any other project repo:
   the whole point is a repo kahyad's normal identity cannot touch.
2. Generate a fresh, dedicated SSH keypair (never reuse any other key):

   ```bash
   ssh-keygen -t ed25519 -f /tmp/kahya-anchor -C "kahya-anchor-$(date +%Y%m%d)"
   ```

3. Add the **public** key (`/tmp/kahya-anchor.pub`) to the repo as a
   **deploy key with write access**.
4. On the repo's default branch (`main`), enable branch protection:
   - **Block force pushes.**
   - **Block deletions.**

   (GitHub: Settings → Branches → branch protection rule on `main` → check
   "Restrict force pushes" and "Restrict deletions". Other hosts: the
   equivalent "protected branch" setting — the two properties that matter
   are force-push and delete being refused server-side, not client-side.)
   This is what makes the remote genuinely append-only: even if kahyad's
   own deploy key were somehow compromised, it can still only ever push
   new commits forward, never rewrite or delete history.
5. Delete the private key file from `/tmp` the moment step 2 of §2 below
   has it safely in Keychain — never leave a plaintext copy on disk.

## 2 · Put the deploy key into Keychain

kahyad reads this from a **separate** Keychain item from
`kahya.anthropic`/`kahya.telegram` — read ONLY by
`kahyad/internal/anchor`'s own code path (HANDOFF §5 safety #4's own
Keychain-isolation clause; enforced permanently by that package's
`anchor_import_guard_test.go`).

```bash
security add-generic-password -U -s kahya.anchor -a kahya -T "$(which kahyad)" -w
# paste (or pipe) the PRIVATE key from step 1 when prompted
```

Notes (same as every other Keychain item this codebase provisions):

- `$(which kahyad)` must resolve to the **codesigned** binary (`Kahya Dev`
  identity) — the ACL is attached to that exact signature.
- `-w` prompts interactively (or reads stdin) — never pass the key as a
  bare CLI argument (shell history, `ps`).
- `-U` is idempotent: safe whether this is the first-ever provisioning or
  a later key rotation (see `docs/runbooks/keychain-loss.md` §3/§4 for the
  full rotation procedure).

Verify it landed:

```bash
security find-generic-password -s kahya.anchor -a kahya   # prints attributes, exit 0
```

## 3 · Set `anchor.remote` in kahyad config

Add to `~/Library/Application Support/Kahya/config.yaml` (create it if it
doesn't exist yet):

```yaml
anchor_remote: git@your-host:you/kahya-anchor.git   # the SSH remote from step 1
anchor_interval_hours: 6                            # optional; 6 is the default
```

Restart kahyad (or `launchctl kickstart -k gui/$(id -u)/com.kahya.kahyad`).
Until `anchor_remote` is set, every anchor push is a permanent no-op
(kahyad/internal/anchor.Pusher.Run's own doc comment).

Only the prod daemon anchors with these settings. Every profile reads this
same config.yaml, but a non-prod daemon (`KAHYA_ENV=dev`, the restore
drill's `KAHYA_ENV=restore`, any other profile) loads with no
`anchor_local_fallback_path` and no `anchor_remote`, and never reads the
real `kahya.anchor` deploy key. The single exception is a dev daemon that
carries the dev-only `KAHYA_ANCHOR_KEY_OVERRIDE`: the hermetic W4/W6 gates
and `scripts/accept_w4.sh` set it, together with a throwaway `file://`
remote in their own temp HOME. `scripts/kahya-dev-env.sh` and the
com.kahya.dev agent never set it, so a dev daemon running beside prod
never appends to prod's fallback file and never pushes to prod's remote
(FIX-FINAL ops-config-r2-dev-profile-anchors-into-prod-and-polls-prod-telegram).
One anchor repo belongs to exactly one brain.db.

## 4 · Confirm it works

1. Wait for the next tick (or the next kahyad restart — a push is also
   attempted once at every startup) and check the JSONL log for an
   `anchor_pushed` line: `kahya log --trace <trace-id-from-that-line>`.
2. On the remote host, confirm `anchors.log` has a new line and `git log`
   shows append-only history (no force-push, no rewritten commits):

   ```bash
   git clone <anchor_remote> /tmp/anchor-check
   cd /tmp/anchor-check && cat anchors.log && git log --oneline
   ```

   Each line is `<event_id> <digest_hex> <timestamp> <hostname>`. The
   timestamp is the moment kahyad recorded the anchor
   (`anchor_log.anchored_at`, UTC, second precision), not the moment the
   push landed, so a push retried after an offline period produces the
   byte-identical line and is never appended twice (lines written before
   FIX-P-13 carry the push time; verify never reads the timestamp).
   kahyad's local clone (`~/Library/Application Support/Kahya/anchor-repo`)
   is only a cache: every push first resets it to the remote tip. If the
   remote already holds a later event than the one being anchored, or the
   same event with a different digest, kahyad pushes nothing, leaves the
   anchor pending and raises a `DEFTER UYARISI` alarm (`anchor.mismatch`,
   reason `remote_ahead` / `digest_mismatch`).

3. Run the tamper drill locally to prove `kahya ledger verify` actually
   catches a rewritten ledger (do this on a throwaway/dev brain.db, not
   your real one, unless you intend to immediately restore from backup
   afterward — see `docs/restore-runbook.md`):

   ```bash
   sqlite3 "$HOME/Library/Application Support/Kahya/brain.db" \
     "UPDATE events SET payload=json_set(payload,'\$.k','x') WHERE id=(SELECT MIN(id) FROM events);"
   kahya ledger verify   # must exit non-zero and print the mismatch line
   ```

   (This UPDATE only succeeds at all because it goes around kahyad
   entirely, straight at the SQLite file — the `events_no_update` trigger
   HANDOFF §5 safety #4 also enforces is a separate, in-DB layer of
   defense; the external anchor is what catches tampering that bypasses
   or disables that trigger too, e.g. via `DROP TRIGGER` first.)

## 5 · Optional offline fallback (different-uid local append-only file)

The primary mechanism above is the append-only git remote. HANDOFF §5
safety #4 also names a documented fallback for when the remote is
unreachable for an extended period: a **different-uid**, kernel-enforced
append-only file on the local machine. This is optional — the git remote
alone already satisfies the invariant when it's reachable — but it gives
you a second, offline-capable copy of every anchor line.

Run once, as an administrator:

```bash
sudo mkdir -p /usr/local/var/kahya
sudo touch /usr/local/var/kahya/anchor.log
sudo chown root:wheel /usr/local/var/kahya/anchor.log
sudo chmod 0622 /usr/local/var/kahya/anchor.log
sudo chflags sappnd /usr/local/var/kahya/anchor.log   # kernel-enforced append-only
```

`chflags sappnd` sets the **system append-only** flag: even the file's
owner (root) cannot truncate or rewrite it without first clearing the
flag as root — kahyad (running as your normal user) can only ever append,
never rewrite, exactly the append-only property the git remote's branch
protection provides remotely.

Then set in `config.yaml`:

```yaml
anchor_local_fallback_path: /usr/local/var/kahya/anchor.log
```

Every anchor line is now appended here the moment kahyad records the
anchor — before and independently of the remote push — exactly once per
anchor (`anchor_log.local_fallback_at`), so an offline period still leaves
a local append-only record. The line is byte-identical to the one the
remote gets (its timestamp is the anchor's `anchored_at`, UTC). Each
line is fsynced (F_FULLFSYNC) before `local_fallback_at` is committed, so
a power cut can never leave brain.db saying a line was written that the
file lost. A failed write or sync (`anchor_local_fallback_failed` in the
JSONL log) is retried on the next tick (a line that landed but failed to
sync may then appear twice) and never blocks the remote push. kahyad never reads this file
(it is `0622`, write-only for your uid); `kahya ledger verify` uses the git
remote. The fallback needs `anchor_remote`: with no remote configured
kahyad anchors nothing and writes nothing here.

## 6 · What `kahya ledger verify` checks, and recovery

Since FIX-P-3 the remote is the authority. `kahya ledger verify` clones the
anchor remote into a throwaway directory next to `anchor-repo`, reads
`anchors.log` from the remote's `main` ref, and treats every line as a
checkpoint: the running digest recomputed from the local `events` must
match each anchored digest. The local `anchor_log` table and
`ledger_digest_state` are only secondary consistency checks. Outcomes:

- `Defter doğrulandı: kurcalama tespit edilmedi.`, exit 0. When
  `anchor_remote` is unset, stderr adds `Uyarı: anchor_remote ayarlı
  değil — …`: only local self-consistency was checked, the external anchor
  was NOT.
- `DEFTER UYARISI: …`, exit 1, plus an `anchor.mismatch` ledger event
  whose `reason` is one of `digest_mismatch`, `remote_ahead`,
  `local_anchor_log_inconsistent`, `digest_state_mismatch`,
  `remote_non_monotonic` or `checkpoint_not_recomputable`.
- Exit 2 (`Defter doğrulaması çalıştırılamadı: …`) when the remote cannot
  be read. That means "could not verify", never "verified".

**Legacy events.** A brain.db that already held events before the ledger
digest existed (migration 0010) has a range of events that were never
digested or anchored. Migration 0023 records the last such id once
(`ledger_chain_base`); verify recomputes from the event after it and
prints `Not: defter özeti başlamadan önce yazılmış N eski olay …` on
stderr. Those events cannot be verified by any anchor. An anchor at or
below that id is reported as `checkpoint_not_recomputable`, so moving the
base cannot hide anchored history.

**Restore from a backup.** If brain.db is restored from a W4-06 backup,
verify turns red with `remote_ahead`. This is by design: a truncated
ledger and an older restored copy look the same from the remote. The
recovery is to rotate the anchor repo:

1. Create a new, empty anchor repository (§1) and point `anchor_remote`
   at it (§3). The deploy key can be reused or rotated.
2. Keep the old repository read-only as an archive of the old anchors.
   Never delete it.
3. Restart kahyad. The next push anchors the current ledger to the new
   repo. Rows pushed to the old repo stay valid local checkpoints; they are
   not expected on the new remote.

**One anchor repo belongs to exactly one brain.db.** A second machine
needs a second repository. Two ledgers pushing into one repo make it
non-monotonic or conflicting (`remote_non_monotonic` / `digest_mismatch`),
and kahyad refuses to push on top of a remote that is ahead of it.

**What may be deleted.** `~/Library/Application Support/Kahya/anchor-repo`
is only a cache: kahyad resets it to the remote tip before every push, so
it can be deleted at any time. A leftover `kahya-anchor-verify-*`
directory next to it (from a verify killed mid-run) can be deleted too.
The `anchor_log` table must NOT be deleted: verify reports
`local_anchor_log_inconsistent` without it.

## Alternative backend: S3 Object Lock (documented, not implemented)

HANDOFF §5 safety #4 also names S3 Object Lock (WORM mode) as a valid
alternative to an append-only git remote. This codebase does not
implement it (the git-remote mechanism above is the one W4-05 built and
tests) — if you prefer S3, the shape would be: a versioned bucket with
Object Lock in **compliance mode**, one PUT per anchor tick (object key
e.g. `anchors/<event_id>.txt`, same line format), and `kahya ledger
verify`'s remote cross-check re-pointed at a `GetObject`/`ListObjectsV2`
call instead of `git clone`/`pull`. This is a documented option for a
future task, not something you need to set up today.
