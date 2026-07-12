# Keychain loss recovery (W4-06)

What to do when the macOS Keychain items kahyad depends on are lost,
corrupted, or the machine is rebuilt from scratch: `kahya.anthropic`
(Anthropic API key), `kahya.telegram` (BotFather token), `kahya.anchor`
(W4-05 ledger-anchor deploy key). All three are provisioned the same way
(W0-04): `-T $(which kahyad)`, account `kahya`.

**The one fact that matters most:** secrets are re-creatable. `brain.db`
is not. Losing Keychain access costs you a key-rotation errand; losing
`brain.db` without a verified `~/Kahya/backups/brain-YYYYMMDD.db` snapshot
(W4-06's own nightly job) costs you the ledger and every episode — neither
is derivable from `~/Kahya/memory/*.md` (tasks/README.md's own global
convention). **This runbook never touches brain.db, the backups directory,
or the memory repo.** If you are here because of BOTH a Keychain loss and
a suspected data loss, stop and run `docs/restore-runbook.md` (W78-05)
for the data side first — this document is Keychain-only.

## 1 · Rotate the Anthropic API key

1. Revoke/rotate the key in the Anthropic console (the exact key currently
   in `kahya.anthropic`, if it's still readable, is the one to revoke —
   check first, don't revoke blind).
2. Generate a new key.
3. Re-add it to Keychain (step 4 below covers the exact command for all
   three items).

## 2 · Regenerate the Telegram bot token via BotFather

1. Open a chat with **@BotFather** on Telegram.
2. `/mybots` → select the Kâhya bot → **API Token** → **Revoke current
   token** → confirm.
3. BotFather issues a new token immediately — copy it.
4. Re-add it to Keychain (step 4 below).
5. `kahya.telegram`'s `chat_id`/`user_id` allowlist pair (`config.yaml`'s
   `telegram_chat_id`/`telegram_user_id`) does **not** change — the bot's
   identity (token) rotated, not the user's. No need to re-DM the bot for
   new IDs unless `config.yaml` itself was also lost.

## 3 · Create a new anchor deploy key (W4-05)

1. Generate a fresh SSH keypair dedicated to the anchor repo (never reuse
   another key): `ssh-keygen -t ed25519 -f /tmp/kahya-anchor-new -C
   "kahya-anchor-$(date +%Y%m%d)"` (move/delete the private key file the
   moment step 4 below has it in Keychain — never leave a plaintext copy
   on disk).
2. Add the new **public** key as a deploy key on the anchor remote (append
   access only — the anchor's whole point per §5 safety #4 is that
   kahyad's normal identity cannot rewrite history there).
3. Remove the old deploy key from the remote once the new one is
   confirmed working (step 6 below).

## 4 · Re-add all three Keychain items

Same provisioning command HANDOFF §7 uses initially — `-U` here means
*update-if-present* (idempotent: safe whether the item is merely stale or
was deleted outright):

```bash
security add-generic-password -U -s kahya.anthropic -a kahya -T "$(which kahyad)" -w   # paste the NEW Anthropic key
security add-generic-password -U -s kahya.telegram  -a kahya -T "$(which kahyad)" -w   # paste the NEW BotFather token
security add-generic-password -U -s kahya.anchor    -a kahya -T "$(which kahyad)" -w   # paste the NEW anchor deploy private key
```

Notes:
- `$(which kahyad)` must resolve to the **codesigned** binary (`Kahya Dev`
  identity, HANDOFF §7) — the Keychain ACL is attached to that exact
  binary's signature, so `-T` here must match what `make install` actually
  installed, not a dev-loop `go run` binary.
- `-w` prompts for the secret value interactively (or reads stdin if
  piped) — never pass the secret as a bare CLI argument (it would land in
  shell history and `ps`).
- If the Keychain itself (not just these items) is the thing that's
  broken — locked, corrupted, `errSecInteractionNotAllowed` persisting
  after unlock — resolve that at the macOS level first (Keychain Access.app
  → repair/recreate the login keychain) before re-adding items; kahyad's
  own fail-fast behavior (HANDOFF §7: cloud lane fails closed + user
  notification, local secret-lane keeps working) is exactly what should be
  happening while you do this.

## 5 · Verify each item is readable

```bash
security find-generic-password -s kahya.anthropic -a kahya   # prints attributes, exit 0
security find-generic-password -s kahya.telegram  -a kahya
security find-generic-password -s kahya.anchor    -a kahya
```

A missing item or `errSecItemNotFound` means step 4 didn't take — repeat
it for that service before moving on.

## 6 · Round-trip health check

1. Restart kahyad (or `launchctl kickstart -k gui/$(id -u)/com.kahya.kahyad`
   if it's already running under launchd) so it re-reads Keychain fresh —
   `kahyad/internal/secrets.Keychain` caches a value for the process
   lifetime once read, so a running process never re-reads spontaneously.
2. `kahya log --trace <recent-boot-trace-id>` and confirm there is **no**
   `keychain_unavailable`/`key_override_ignored`/`telegram_disabled` line
   for this boot.
3. Ask kahyad something ordinary from the CLI (`kahya "merhaba"`) and
   confirm a real cloud-model reply comes back — proves the rotated
   Anthropic key actually works end to end, not just that Keychain can
   read it back.
4. Send the bot's chat one message and confirm kahyad's Telegram poller
   picks it up (`kahya log --trace <id>` shows a `telegram_update`-shaped
   line, or trigger a W2 approval and confirm the card arrives) — proves
   the rotated BotFather token works.
5. Trigger the anchor job once (`kahya-trigger ledger-anchor` once W4-05
   lands) and confirm the push succeeds against the new deploy key.

## What is NOT lost

Every one of the three secrets above is re-creatable from scratch, by
design — kahyad was built so that a total Keychain wipe is an
inconvenience, never a data-loss event:

- **No memory is lost.** `~/Kahya/memory/*.md` is a separate git repo,
  untouched by any Keychain state.
- **No ledger/episode data is lost.** `brain.db` (the ledger `events`
  table and `episodes`, which — unlike everything else — cannot be
  rebuilt from markdown) lives in `~/Library/Application Support/Kahya/`
  and is backed up nightly by this same task's `backup-nightly` job
  (`~/Kahya/backups/brain-YYYYMMDD.db`, 7-day retention, Time
  Machine–backed). Keychain loss and brain.db loss are independent
  failure modes; this runbook addresses only the former.
- The autonomy ladder's learned state, task history, and every prior
  approval/undo record in `brain.db` survive a full Keychain rebuild
  unchanged.
