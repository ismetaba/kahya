# W3-02 — Policy engine: autonomy ladder + one-time approval tokens

**Status:** done
**Phase:** W3 — Policy + tools
**Depends on:** W3-01, W12-07
**Flags:** none
**Handoff refs:** §4 ladder ⚑, §5 safety enforcement plane ⚑, §4 IPC ⚑ (policy check)

## Goal
The binding policy decision point inside kahyad: given (tool, class, scope, session, task), it answers ALLOW / NEEDS_APPROVAL / DENY per the autonomy ladder, mints one-time approval tokens that side-effectful MCP tools must verify before executing, tracks promotion/demotion state, manages the W1 5-minute undo window, and writes a ledger event for every decision. This replaces the interim static policy from W12-07.

## Context you need
The enforcement plane — the single most important constraint of this task (HANDOFF §5):

> ⚑ **Uygulama düzlemi (önce oku):** `can_use_tool` bir **erken-ret/UX katmanıdır, güvenlik sınırı değildir** — worker sürecinin içinde çalışan bir SDK geri-çağrısıdır. Bağlayıcı politika kararı **kahyad'da** verilir; yan-etkili MCP araçları kahyad'ın verdiği **tek-kullanımlık onay jetonunu** doğrulamadan yürümez (ya da yan-etkili MCP sunucularını kahyad spawn edip sahiplenir, worker onlara yalnız kahyad üzerinden erişir).

The transport contract (HANDOFF §4 IPC):

> - Politika kontrolü: `~/Library/Application Support/Kahya/kahyad.sock` üzerinden **HTTP-over-UDS** `POST /policy/check`, timeout 5s; **her hata/timeout = RED (fail-closed)** — §5 "güvenlik yürütücüde" ilkesinin doğal sonucu.

The autonomy ladder (HANDOFF §4) — earned per *tool × class × scope* triple: L0 Gözlemci (everything needs approval), L1 Çırak (R auto), L2 Eşlikçi (R + W1 auto with 5-min undo + ledger), L3 Vekil (adds W2 from an earned allowlist), L4 Kâhya (R/W1/W2 auto; W3 always approval). Promotion/demotion rules, verbatim:

> - **Terfi:** eylem-sınıfı başına 20 ardışık onay + 0 red → ürün terfi **ÖNERİR**; kullanıcı onaylamadan asla otomatik terfi olmaz.
> - **Tenzil:** red / geri-alma / güvenlik ihlali → bir seviye düşer.
> - **W3 her seviyede, sonsuza dek kalıcı yazılı onay ister. Bu bir ayar değil, ürün ilkesi.

W1 undo window comes from the ladder table row: `L2 | Eşlikçi | R, W1 (5-dk geri-alma + defter)`. Ledger = the append-only `events` table from W12-02. W12-07 already exposes `/policy/check` with an interim "R allowed, all W denied" policy — you are replacing its decision logic, keeping its transport. Approval *surfaces* (Telegram, CLI prompt) are W3-06/W3-07; this task exposes the engine API they call.

## Deliverables
- `kahyad/internal/policy/engine.go` — decision function + ladder logic.
- `kahyad/internal/policy/tokens.go` — one-time approval token mint/verify/consume.
- `kahyad/migrations/NNNN_autonomy_policy.sql` (goose, next free number) — tables: `autonomy_state(tool, class, scope, level, consecutive_approvals, updated_at, PRIMARY KEY(tool,class,scope))`, `approval_tokens(token_hash, task_id, trace_id, tool, approved_bytes_hash, minted_at, expires_at, consumed_at)`, `undo_windows(task_id, tool, trace_id, opened_at, deadline, state)`.
- sqlc queries for the three tables.
- UDS endpoints in kahyad: `POST /policy/check` (upgraded), `POST /policy/consume-token`, `POST /policy/feedback` (approve/deny/undo outcomes feed promotion/demotion), `GET /policy/state` (dump ladder state for CLI).
- `kahya autonomy` CLI subcommand: list ladder state; `kahya autonomy promote <tool> <class> <scope>` — the ONLY promotion path (user-invoked).
- `kahya undo --trace <id>` CLI subcommand: triggers the registered undo recipe via kahyad while the window is open (recipe execution itself delegated to the owning tool, e.g. W3-03 fs).
- Tests in `kahyad/internal/policy/engine_test.go`, `tokens_test.go`.

## Steps
1. Decision algorithm in `engine.go`: look up tool registration in loaded policy (W3-01); missing tool ⇒ DENY. Look up `autonomy_state` for (tool, class, scope); missing row ⇒ L0. Apply the ladder table. Result ∈ {ALLOW, NEEDS_APPROVAL, DENY}. Every decision writes an `events` row: `{event:"policy_decision", tool, class, scope, level, decision, trace_id, task_id}`.
2. ALLOW for a W1 at L2+ opens an `undo_windows` row with `deadline = now + 5m` and schedules finalization; ledger event `undo_window_opened` / `undo_window_expired`.
3. NEEDS_APPROVAL response includes a `pending_approval_id`; when an approval surface reports approval via `/policy/feedback`, mint a token: 32 random bytes, store SHA-256 only, TTL 10 minutes, single-use, bound to `task_id` + `approved_bytes_hash` (hash supplied by the WYSIWYE pipeline once W3-06 lands; until then bind to the serialized tool-call args).
4. `POST /policy/consume-token`: side-effectful MCP tools (W3-03/04/09, memory_write) MUST call this immediately before executing; verify unexpired + unconsumed + matching `approved_bytes_hash`; mark consumed atomically (single SQLite UPDATE ... WHERE consumed_at IS NULL). Any mismatch ⇒ DENY + ledger event `token_verify_failed`.
5. Promotion/demotion in `/policy/feedback`: approve ⇒ `consecutive_approvals++`; at 20 with 0 denials emit ledger event `promotion_suggested` + (once W3-07 exists) a Telegram/CLI notification — level does NOT change. Deny, undo, or violation (e.g. `token_verify_failed`, WYSIWYE reject) ⇒ level minus one (floor L0), counter reset, ledger event `demoted`.
6. Hard-code in Go (not config): class W3 NEVER returns ALLOW regardless of level; approval for W3 must carry `surface:"local"` (enforced again in W3-06/W3-07). Also hard-code the input surface: the decision function's inputs are ONLY (policy registration, `autonomy_state`, class, scope, taint/session flags) — no memory-derived data (facts, preferences, `<hafiza>` blocks) may reach the decision. This is §5 product principle "Hafıza bir izni asla düşüremez; tercihler işin *nasıl* önerileceğini bilgilendirir, *yapılıp yapılmayacağını* değil" — enforce with a test that `kahyad/internal/policy` does not import the memory/index packages (import-graph assertion or lint rule in `make lint`).
7. Fail-closed plumbing: any DB error, unknown class, or policy deny-all mode (W3-01) ⇒ DENY. Keep the 5s handler budget; the worker side already treats timeout as DENY.
8. Wire `can_use_tool` expectation: `/policy/check` response schema gains `{decision, pending_approval_id?, token?}` — token returned ONLY for ALLOW on side-effectful tools, so the tool can consume it. Document the schema in `kahyad/internal/policy/README.md` (10 lines max).
9. Tests: full ladder matrix (5 levels × 4 classes); 20-approvals suggestion emitted but level unchanged until `kahya autonomy promote`; demotion on deny; token single-use (second consume fails); token bytes-hash mismatch fails; W3 never ALLOW even with a forged L4 row; DB-error path returns DENY.

## Acceptance criteria
- [ ] `go test ./kahyad/internal/policy/...` green in `make test`; includes the W3-never-ALLOW test and the token single-use test.
- [ ] `curl --unix-socket ... -d '{"tool":"fs_write","class":"W1",...}' http://kahyad/policy/check` at fresh state (L0) returns NEEDS_APPROVAL; after `kahya autonomy promote fs_write W1 <scope>` to L2, same call returns ALLOW and `sqlite3 brain.db "SELECT * FROM undo_windows"` shows an open window.
- [ ] Ledger check: every decision visible via `sqlite3 brain.db "SELECT json_extract(payload,'$.event') FROM events WHERE json_extract(payload,'$.event') LIKE 'policy_%'"` with `trace_id` populated.
- [ ] 20 consecutive approvals produce a `promotion_suggested` event and NO `autonomy_state.level` change (test asserts both).
- [ ] A replayed (already-consumed) token is rejected and a `token_verify_failed` + `demoted` event pair appears in the ledger (test).

## Out of scope
- Approval surface rendering: Telegram inline buttons (W3-07), CLI "onayla" prompt + byte-exact diff (W3-06), Hammerspoon cards (W6-01).
- WYSIWYE normalization/hashing itself — W3-06 (this task only stores/compares the hash it is given).
- Taint tiers / Reader-Actor session gating — W4-03.
- Saga compensation executor — deferred (§8); graded execution + receipts are W4-02.
- Undo recipe *implementations* (Trash restore, git checkpoint) — W3-03.
