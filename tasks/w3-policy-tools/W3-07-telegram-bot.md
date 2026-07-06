# W3-07 — Telegram approval bot (telebot.v4, long-polling, W2-only approvals)

**Status:** todo
**Phase:** W3 — Policy + tools
**Depends on:** W3-06, W0-04
**Flags:** user-assist
**Handoff refs:** §5 safety #5 ⚑ Telegram bullets, §4 cost governor ⚑, §4 UI, §9 libraries

## Goal
A Telegram bot inside kahyad (`gopkg.in/telebot.v4`, long-polling) that delivers W2 approval requests with byte-exact diffs and inline Onayla/Reddet buttons, sends W3 "yerelde onay bekleniyor" notifications (never accepting W3 approval input), enforces a single fixed chat_id/user_id allowlist Go-side, redacts secret-lane content, labels remote approvals in the ledger, and delivers cost-governor alarms.

## Context you need
Binding bullets (HANDOFF §5 safety #5):

> ⚑ **W3 yazılı "onayla" YALNIZ yerel yüzeyden kabul edilir** (W3–W5: CLI istemi; W6+: Hammerspoon kartı). Telegram W3 için yalnız "yerelde onay bekleniyor" bildirimi gönderir, **onay girdisi kabul etmez**.
> ⚑ **Gizli-şerit (finans/sağlık/kimlik) etiketli tek bir bayt içeren diff Telegram'a gönderilmez** — bu onaylar yalnız yerel yüzeyde gösterilir, Telegram'a en fazla redakte başlık gider.
> ⚑ **Telegram auth:** tek sabit `chat_id`/`user_id` allowlist'i **Go tarafında (kahyad)** uygulanır — eşleşmeyen her update sessizce düşer + deftere loglanır; **long-polling** (gelen ağ yüzeyi yok); Telegram-kaynaklı onaylar defterde `remote` etiketli. Token Keychain'de (§9 kapsıyor).

Library is locked (HANDOFF §9): "`gopkg.in/telebot.v4` (Go — kahyad içinde, WYSIWYE onay kapısının parçası; grammY/TS DEĞİL — iki-süreç yığınıyla çelişir)". Telegram is also the delivery channel for cost alarms (HANDOFF §4):

> ⚑ **Maliyet valisi (somut):** görev-başına 500K token tavanı; günlük bütçe $10 / aylık $150. Tavanda görev **duraklar** + Telegram bildirimi; günlük bütçenin %80'inde yönlendirici bir kademe ucuza düşer (Opus→Sonnet→yerel). Cache-hit oranı ve günlük harcama **alarm verir** (Telegram'a) — sessiz cache-bozan maliyeti 5–10× katlar.

Also §4 UI: Telegram is for remote approval of "yalnız geri-alınabilir eylemler". Read with the §4 class definitions (W3 = geri dönüşsüz; W1/W2 are both recoverable — W2 with effort) and the §6 W3 gate ("W2 bir eylem Telegram'dan byte-tam diff ile onay istiyor"), the rule implemented here is: **Telegram may approve W1 and W2 payloads; W3 NEVER, under no configuration.** Every Telegram approval is ledgered `remote`. The per-tool `reversible` flag is a different axis (it feeds the undo window and diff wording), not the Telegram eligibility test — class < W3 is the test. Approval cards are egress (§5 #1) — every outbound message passes `egress.Check` (W3-05). Diff rendering/chunking comes from W3-06 `diff.go`. Token comes from Keychain item `kahya.telegram` (W0-04). Secret-lane labeling of a payload: path matched `secret_lane_globs` (W3-01) or content flagged by W3-08's classifier; if W3-08 hasn't landed, glob-label alone applies (fail-closed on the stronger side once W3-08 lands).

Gotchas:
- Telegram callback data is limited to 64 bytes — put only the `pending_approval_id` there, never any payload content.
- Callbacks can arrive twice (Telegram redelivery): approval handling must be idempotent; the one-time token semantics (W3-02) already make double-execution impossible, but answer the duplicate callback gracefully (`Zaten işlendi`).
- Edit the card after resolution (`✅ Onaylandı` / `❌ Reddedildi` / `⏰ Süresi doldu`) so a stale phone screen can't mislead.
- Pending approvals expire with the token TTL (10 min, W3-02) — expired callback ⇒ `Onay süresi doldu, yeniden isteyin.`

## Deliverables
- `kahyad/internal/telegram/bot.go` — telebot.v4 long-polling setup, Keychain token read, allowlist middleware.
- `kahyad/internal/telegram/approvals.go` — W2 approval cards (chunked diff + inline buttons), W3 notify-only cards, result routing to `/policy/feedback` with `surface:"telegram"`.
- `kahyad/internal/telegram/redact.go` — secret-lane redaction: if ANY byte of the payload is secret-lane-labeled, send only `🔒 Yerel onay gerekiyor: <redacted-title> (gizli şerit)` — no diff, no path, no content.
- `kahyad/internal/telegram/alarms.go` — cost/budget/cache-hit alarm sink consumed by W12-08's alarm hooks.
- Config: `telegram.chat_id`, `telegram.user_id` in kahyad config (single fixed pair; empty ⇒ bot disabled, everything falls back to local surface).
- Tests: `bot_test.go` (allowlist middleware), `approvals_test.go`, `redact_test.go` — against a fake telebot transport; no live API in `make test`.

## Steps
1. Boot: read token via Keychain (`kahya.telegram`); missing/locked Keychain ⇒ log `telegram_disabled` + continue (fail-fast per §7 Keychain note, local surfaces unaffected). Start long-polling only — assert no webhook/listen config exists (no inbound network surface).
2. Allowlist middleware: every update's `chat_id` AND `user_id` must equal the configured pair; mismatch ⇒ drop silently (no reply!) + ledger event `telegram_unauthorized_update` with sender IDs. This runs before ANY handler.
3. W2 approval flow: subscribe to the engine's pending-approval feed (W3-02). For a W2 payload: run `redact.go` check; clean payloads get the W3-06 rendered diff in monospace chunks (≤4096 each, final chunk carries inline keyboard `✅ Onayla` / `❌ Reddet` with the `pending_approval_id` in callback data). Callback → verify allowlist again → `/policy/feedback` with `surface:"telegram"`; engine mints the token bound to the SAME approved-bytes hash. Ledger event for the approval carries label `remote`.
4. W3 flow: send notification only — `⏳ Yerelde onay bekleniyor (W3): <summary>. Terminalden 'kahya approve <id>' çalıştırın.` NO buttons attached; the bot registers no handler that could approve a W3 (defense: the engine's `w3_nonlocal_approval_rejected` from W3-06 step 5 is the backstop — add a test that a forged W3 callback is rejected at the engine).
5. Egress compliance: all sends go through a helper calling `egress.Check(host="api.telegram.org", nbytes=len(rendered), session)` first; blocked ⇒ ledger `egress_blocked_*` + fall back to local notification. This makes approval cards obey the sensitive-read rule automatically.
6. Alarms: implement the sink interface W12-08 defined (task-paused-at-ceiling, 80% daily budget downgrade, cache-hit degradation, daily spend). Turkish strings, e.g. `⚠️ Görev duraklatıldı: 500K token tavanı (trace: <id>)`, `📉 Cache-hit oranı düştü: %<n>`.
7. Manual setup (user-assist): user creates the bot with BotFather, stores token via `security add-generic-password -s kahya.telegram -a kahya -T "$(which kahyad)" -w`, messages the bot once; you read the update to capture chat_id/user_id into config. If the user is unavailable, set `Status: blocked-user` with exactly what you need (token + one message to the bot).
8. Tests: middleware drops non-matching update and ledgers it; W2 card contains byte-exact diff chunks (fixture with Turkish content `Bütçe raporu ği üşç.md` survives byte-exact); secret-lane fixture produces ONLY the redacted title (assert no payload byte appears in any sent message); forged W3 callback rejected; long-poll config asserted (no webhook).

## Acceptance criteria
- [ ] `go test ./kahyad/internal/telegram/...` green in `make test` (fake transport, no network).
- [ ] Manual (needs live bot): a W2 `fs_write` at L0 sends a Telegram card with the byte-exact diff; tapping `✅ Onayla` executes the action; `sqlite3 brain.db` shows the approval event labeled `remote` with the task's `trace_id`. (Mirrors §6 W3 gate: "W2 bir eylem Telegram'dan byte-tam diff ile onay istiyor".)
- [ ] Manual: a W3 action produces ONLY the notify message on Telegram (no buttons); `kahya approve <id>` + typed `onayla` completes it. (Gate: "W3 eylem Telegram'dan onaylanamıyor, CLI'dan yazılı "onayla" ile geçiyor".)
- [ ] Redaction test green: secret-lane-labeled diff never leaves — grep the fake transport's sent messages for any payload substring ⇒ zero matches.
- [ ] Unauthorized-update test green: drop + `telegram_unauthorized_update` ledger row, no reply sent.
- [ ] Cost alarm path: fire W12-08's ceiling hook in a test ⇒ Turkish alarm message queued through the egress-checked sender.

## Out of scope
- W3 approval via Telegram — permanently prohibited (§5 #5), not a future toggle.
- Webhook mode / inbound HTTP — prohibited (long-polling only).
- Secret-lane content classification — W3-08 (this task consumes labels).
- The weekly truth ritual reusing this bot — W5-03.
- iPhone app — deferred (§8: "Telegram zaten telefon").
