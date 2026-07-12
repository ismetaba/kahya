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
(kahyad/internal/anchor.Pusher.Run's own doc comment) — this is also
exactly why dev/test never pushes to a real remote: `anchor_remote`
defaults to empty.

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

Every anchor line kahyad successfully pushes to the remote is now ALSO
appended here (`O_APPEND`, best-effort — a failure to write the local
fallback never blocks or fails an otherwise-successful remote push).

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
