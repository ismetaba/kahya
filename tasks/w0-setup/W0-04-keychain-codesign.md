# W0-04 — Keychain + codesign

**Status:** todo
**Phase:** W0 — Day-1 setup
**Depends on:** W0-02
**Flags:** user-assist
**Handoff refs:** §7 secrets, §9

## Goal

A stable self-signed `Kahya Dev` code-signing identity exists; the Makefile signs `kahyad` on
every build and installs it to a stable path; the three Keychain items exist with an ACL
granting exactly that binary access. After this task, rebuilding kahyad never re-triggers
Keychain permission prompts, and W12-08 (forward-proxy), W3-07 (Telegram), W4-05 (anchor) can
read their secrets.

## Context you need

HANDOFF §7 (binding, quote verbatim):

> ⚑ **Sırlar (Keychain) + kod imzalama:** kahyad Makefile'da sabit self-signed kimlikle imzalanır (`codesign -s 'Kahya Dev' kahyad` — Apple Silicon'da Go'nun ad-hoc imzası her build'de değişip Keychain ACL'ini kırar). Sırlar `-T $(which kahyad)` ile eklenir:
> ```bash
> security add-generic-password -s kahya.anthropic -a kahya -T "$(which kahyad)" -w   # ANTHROPIC key
> security add-generic-password -s kahya.telegram  -a kahya -T "$(which kahyad)" -w   # BotFather token
> security add-generic-password -s kahya.anchor    -a kahya -T "$(which kahyad)" -w   # dış-çapa deploy key
> ```

> Keychain kilitli/erişilemezse (SSH oturumu, lock-keychain zaman aşımı → `errSecInteractionNotAllowed`): bulut şeridi **fail-fast + kullanıcı bildirimi**, yerel gizli-şerit çalışmaya devam eder.

Why kahyad only: §4 — kahyad is "**Keychain'den bulut anahtarını okuyan tek süreç**"; the API
key is never given to the worker (forward-proxy, W12-08). §5 safety #4 — the anchor credential
"Keychain'de **ayrı öğedir**, yalnız çapa-yazma kod yolunda okunur" (that is `kahya.anchor`).
§9 fixes the item names: `kahya.anthropic` / `kahya.telegram` / `kahya.anchor`.

Gotchas:
- `-T "$(which kahyad)"` requires kahyad on `PATH` at a stable location BEFORE adding items.
  This task installs to `~/bin/kahyad` (`make install`); the W12-01 launchd plist must point at
  the same path.
- `security add-generic-password` fails with `errSecDuplicateItem` on re-run; use `-U` to update.
- Creating/trusting the identity needs the user's login-keychain password and (for trust) an
  admin prompt — that is the 🧍 part, along with typing the secret values.
- W0-02 already produced a buildable stub `bin/kahyad`; do not wait for W12-01.

## Deliverables

- Login keychain: self-signed code-signing identity `Kahya Dev` (10-year validity).
- `/Users/matt/code/kahya/scripts/setup-signing.sh` — idempotent identity creation script.
- `/Users/matt/code/kahya/Makefile` — new `codesign` and `install` targets (extends W0-02's file).
- `~/bin/kahyad` + `~/bin/kahya` installed, `kahyad` signed by `Kahya Dev`.
- Keychain items `kahya.anthropic`, `kahya.telegram`, `kahya.anchor` (service names exact),
  account `kahya`, ACL trusting `~/bin/kahyad` — values typed by the user.

## Steps

1. Write `scripts/setup-signing.sh` (idempotent: exit 0 early if
   `security find-identity -v -p codesigning | grep -q "Kahya Dev"`): generate an RSA-2048
   self-signed cert with `keyUsage=critical,digitalSignature` and
   `extendedKeyUsage=critical,codeSigning`, CN=`Kahya Dev`, `-days 3650` via `openssl req
   -x509`; bundle with `openssl pkcs12 -export`; `security import <p12> -k
   ~/Library/Keychains/login.keychain-db -P <p12pass> -T /usr/bin/codesign`; then
   `sudo security add-trusted-cert -d -r trustRoot -p codeSign -k
   /Library/Keychains/System.keychain <crt>`. **USER:** approves the sudo/keychain prompts.
   (Fallback if scripting fights the keychain: Keychain Access → Certificate Assistant →
   Create a Certificate… → name `Kahya Dev`, type Code Signing — document this in the script header.)
2. Extend the Makefile (keep W0-02 targets untouched):
   ```make
   CODESIGN_ID ?= Kahya Dev
   codesign: build
   	codesign -f -s "$(CODESIGN_ID)" bin/kahyad
   install: codesign
   	mkdir -p $(HOME)/bin
   	install -m 0755 bin/kahyad $(HOME)/bin/kahyad
   	install -m 0755 bin/kahya  $(HOME)/bin/kahya
   ```
3. Run `bash scripts/setup-signing.sh`, then `make install`.
4. Verify PATH: `which kahyad` must print `/Users/matt/bin/kahyad`. If `~/bin` is not on PATH,
   ask the user to add it to their shell profile (do not edit shell rc files yourself — they
   are on the §5 #6 fs write-deny list as a matter of principle).
5. **USER:** run the three `security add-generic-password` commands from the quoted ⚑ block
   verbatim (each `-w` prompts for the secret value twice): Anthropic API key, Telegram
   BotFather token, external-anchor deploy key. If a value does not exist yet (e.g. no
   BotFather bot created, no anchor remote chosen), skip that item, list it in the
   blocked-user note — W12-08 needs `kahya.anthropic` (W1–2), W3-07 needs `kahya.telegram`
   (W3), W4-05 needs `kahya.anchor` (W4).
6. Verify items (metadata only — NEVER print secret values into logs/transcript):
   `security find-generic-password -s kahya.anthropic -a kahya >/dev/null && echo OK` (×3).
7. Signature stability check (the whole point of the ⚑): run `make install` twice; after each,
   `codesign -d -r- $(which kahyad) 2>&1` — the designated requirement must be identical both
   times and reference `Kahya Dev`.
8. Commit: `[W0-04] add Kahya Dev codesign step and Keychain item setup` (script + Makefile
   only; certs/keys/secrets never enter the repo).

## Acceptance criteria

- [ ] `security find-identity -v -p codesigning | grep 'Kahya Dev'` prints an identity.
- [ ] `make install` exits 0; `codesign --verify --strict ~/bin/kahyad` exits 0 and
      `codesign -dv ~/bin/kahyad 2>&1 | grep 'Authority=Kahya Dev'` matches.
- [ ] `which kahyad` prints `/Users/matt/bin/kahyad`.
- [ ] Designated requirement stable across two consecutive `make install` runs
      (`codesign -d -r- ~/bin/kahyad` output identical — no ad-hoc drift).
- [ ] `security find-generic-password -s kahya.anthropic -a kahya` exits 0; same for
      `kahya.telegram` and `kahya.anchor` — any missing item is listed in a `blocked-user`
      note naming exactly which secret the user must supply.
- [ ] `git -C /Users/matt/code/kahya show --stat HEAD` lists only `scripts/setup-signing.sh`,
      `Makefile`, and the task-protocol bookkeeping files (this task file + `tasks/BACKLOG.md`).
- [ ] No key material committed: `git -C /Users/matt/code/kahya log -p | grep -c -E
      'BEGIN (RSA |EC )?PRIVATE KEY|BEGIN CERTIFICATE|sk-ant-'` prints 0, and no `.p12`/`.crt`/
      `.pem` file is tracked (`git ls-files | grep -c -E '\.(p12|crt|pem|key)$'` prints 0).

## Out of scope

- Reading secrets from Go code / Keychain API integration and the `errSecInteractionNotAllowed`
  fail-fast behavior (W12-08 for anthropic; W3-07 telegram; W4-05 anchor).
- The forward-proxy, `ANTHROPIC_BASE_URL` spawning, budgets (W12-08).
- launchd plist and TCC permission grants under launchd (W12-01, §7 checklist, W6-01).
- SQLCipher — explicitly rejected by §4 (FileVault + Keychain instead); Apple Developer ID /
  notarization — self-signed is the locked design.
- Key rotation runbook (W4-06 Keychain-loss runbook).
