# W3-06 — WYSIWYE approval pipeline

**Status:** done
**Phase:** W3 — Policy + tools
**Depends on:** W3-02
**Flags:** none
**Handoff refs:** §5 safety #5 ⚑ (all bullets), §4 ladder (W3 written approval), §4 UI (CLI is the W1–5 surface)

## Goal
What You See Is What You Execute: a normalization + hashing pipeline in kahyad that canonicalizes every approval payload (NFC, bidi/zero-width/homoglyph strip, canonical path/host), hashes the exact bytes shown to the user, binds that hash to the one-time approval token, and rejects execution if the executed bytes differ. Plus the byte-exact diff renderer and the CLI approval surface (including the written "onayla" prompt that is the ONLY valid W3 approval at W3–W5).

## Context you need
The invariant (HANDOFF §5 safety #5):

> 5. **WYSIWYE onay.** NFC-normalize, bidi/sıfır-genişlik/homoglyph temizliği, kanonik yol/host, onaylanan baytın hash'i — yürütülen bayt farklıysa **ret**.
>    - ⚑ **W3 yazılı "onayla" YALNIZ yerel yüzeyden kabul edilir** (W3–W5: CLI istemi; W6+: Hammerspoon kartı). Telegram W3 için yalnız "yerelde onay bekleniyor" bildirimi gönderir, **onay girdisi kabul etmez**.

And from §4: "**W3 her seviyede, sonsuza dek kalıcı yazılı onay ister. Bu bir ayar değil, ürün ilkesi.**"

How it composes: W3-02 mints tokens bound to `approved_bytes_hash`; this task defines how that hash is computed and gives approval surfaces a shared renderer. Consumers: CLI prompt (this task), Telegram inline buttons for W2 (W3-07), Hammerspoon cards (W6-01). Path canonicalization already exists in `mcp/fs/paths.go` (W3-03) — factor shared code into `kahyad/internal/canon/` if W3-03 landed first; otherwise create it here and have W3-03 consume it (coordinate via the package, not copy-paste). Homoglyph strategy: you cannot "fix" homoglyphs — you must make them visible; confusable-with-ASCII code points are highlighted, and mixed-script tokens are flagged in the rendered diff. The Turkish alphabet (ı, İ, ş, ğ, ç, ö, ü) is expected content, NOT a homoglyph signal — never flag pure-Turkish words.

Fixture set to build early (used across steps 1, 6, 7 and later by the W78-02 red team):
- `bidi_filename`: `invoice‮fdp.txt` (contains U+202E) — must render with a visible escape.
- `homoglyph_host`: `pаypal.com` (Cyrillic `а` U+0430) — mixed-script flag required.
- `zero_width_url`: `https://api.tele​gram.org` (U+200B inside host) — flag + canonical host differs.
- `turkish_clean`: `Çağrı'nın özgeçmişi güncellendi` — must pass with zero flags.
- `nfd_path`: `~/Kahya/memory/proje-notları.md` encoded NFD — hash-equal to its NFC form.
Store them as Go raw strings or files under `kahyad/internal/canon/testdata/` — byte-exact, never re-typed by hand later.

## Deliverables
- `kahyad/internal/canon/normalize.go` — NFC normalization, bidi/zero-width strip list, confusable detection (pin `golang.org/x/text`; vendor the Unicode **UTS #39 `confusables.txt`** data file under `kahyad/internal/canon/data/` with its Unicode version recorded in a header comment, parsed at init — do not hand-roll Unicode data, do not fetch it at runtime).
- `kahyad/internal/approval/payload.go` — `ApprovalPayload{Kind, Summary, CanonicalBytes, Hash}` builder: tool call args → canonical rendering → SHA-256.
- `kahyad/internal/approval/diff.go` — byte-exact diff renderer (unified diff for file edits, canonical arg listing for commands/scripts, canonical URL/host line for egress) with visible escapes for stripped/flagged code points (e.g. `​`, `⚠ mixed-script: "pаypal"`).
- `kahyad/internal/approval/surface_cli.go` + `kahya` CLI wiring: pending-approval list, full diff display, `onayla` / `reddet` prompt over the UDS.
- Execution-time verification hook: side-effectful tools recompute the canonical hash of what they are ABOUT to execute and pass it to `/policy/consume-token`; mismatch ⇒ reject (W3-02 already compares; this task guarantees both sides use the same canonicalization).
- Tests: `normalize_test.go`, `payload_test.go`, `diff_test.go`, CLI surface test.

## Steps
1. `normalize.go`: NFC via `norm.NFC`; strip/flag set: bidi controls (U+202A–U+202E, U+2066–U+2069), zero-width (U+200B–U+200D, U+FEFF, U+2060), other Cf category defaults; confusable detection per-token: if a token mixes scripts or contains confusables mapping to ASCII, mark it (never silently rewrite user content — approval must show reality). Pure-Turkish fixture: `"Çağrı'nın özgeçmişi güncellendi"` passes untouched and unflagged (byte-exact fixture in tests).
2. `payload.go`: deterministic serialization per payload kind — `file_edit` (canonical path + unified diff of canonical bytes), `shell_script` (image digest + workdir + script bytes), `osascript` (script bytes), `egress` (method + canonical host + byte count), `message` (recipient + body). Hash = SHA-256 over the serialized canonical form. The SAME serializer runs at approval time and at execution time.
3. `diff.go`: render for terminals (CLI) and for Telegram (monospace block ≤4096 chars per message, chunked; W3-07 consumes this). Every stripped/flagged code point rendered as an escape, never dropped invisibly.
4. CLI surface: `kahya approvals` lists pending approvals (id, tool, class, summary, age); `kahya approve <id>` shows the full rendered diff, then prompts. For W1/W2: `[e]vet / [h]ayır`. For **W3**: require the literal typed word `onayla` (nothing else accepted — not `evet`, not `y`); prompt text: `Bu eylem geri alınamaz (W3). Devam etmek için 'onayla' yazın:`. The approval feedback to `/policy/feedback` carries `surface:"local"`.
5. Enforce surface rule in the engine hook: `/policy/feedback` approval for a W3 payload with `surface != "local"` ⇒ rejected + ledger event `w3_nonlocal_approval_rejected` (this is Go-side, arrives before W3-07 exists so Telegram can never be wired wrong later).
6. Execution-time check: extend the tool-side helper (used by W3-03/04/09): before executing, rebuild the canonical payload from the ACTUAL execution inputs and call `/policy/consume-token` with its hash. Add a regression test that mutates one byte between approval and execution (e.g. path `~/x.txt` → `~/x.txt ` trailing space, or a homoglyph swap `а`→`a`) and asserts rejection + `token_verify_failed` ledger event.
7. Tests: bidi-injection fixture (`file named "invoice‮fdp.txt"`) renders with visible escape and survives round-trip hashing; zero-width in a URL host flagged; NFD input path hashes equal to NFC input path (canonicalization before hash); W3 CLI prompt rejects `evet`, accepts `onayla`; chunker respects 4096.

## Acceptance criteria
- [x] `go test ./kahyad/internal/canon/... ./kahyad/internal/approval/...` green in `make test`.
- [x] Mutated-byte regression test green: approved-bytes≠executed-bytes ⇒ rejection, `token_verify_failed` in ledger (this is the §5 #5 "yürütülen bayt farklıysa ret" test that W78-03 will collect into CI).
- [x] Manual: trigger an `fs_write` (class W1 per policy.yaml — at L0 every W class needs approval, which is the point of the demo) at L0, run `kahya approvals` then `kahya approve <id>` — full byte-exact diff shown, `e` approves, tool executes, ledger links approval→token→execution under one `trace_id`.
- [x] Manual: trigger a W3-class tool (`mail_send` stub is fine); `kahya approve <id>` refuses `evet`, accepts typed `onayla`; ledger event records `surface:"local"`.
- [x] Test proves `/policy/feedback` with `surface:"telegram"` on a W3 payload is rejected with `w3_nonlocal_approval_rejected`.
- [x] Turkish-content fixture (`Çağrı'nın özgeçmişi güncellendi`) is not flagged as confusable (test).

## Out of scope
- Telegram delivery/buttons and secret-lane redaction — W3-07.
- Hammerspoon approval cards — W6-01 (CLI is the W1–5 local surface per §4).
- Taint/Reader-Actor gating of WHAT may request approval — W4-03.
- Undo execution — W3-03.
- Any relaxation of the W3-written-approval rule — it is a product principle (§5), permanently out of scope.
