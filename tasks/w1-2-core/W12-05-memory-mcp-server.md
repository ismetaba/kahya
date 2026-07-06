# W12-05 — memory MCP server (kahyad-owned)

**Status:** todo
**Phase:** W1–2 — Core
**Depends on:** W12-04
**Flags:** none
**Handoff refs:** §4, §5 memory #1

## Goal
The worker can reach memory — but only through kahyad, and kahyad's side is the boundary. Three MCP tools (`memory_search`, `memory_write`, `memory_forget`) are implemented in Go inside kahyad; the write tools are gated **in kahyad's dispatcher** by the policy engine (interim: denied until W3), writes go markdown-first then reindex, injection-eligible search excludes quarantined tiers, and every injected `<hafiza>` block is ledgered.

## Context you need
Ownership is locked (HANDOFF §6 W1–2, verbatim): "hafıza MCP sunucusu (`memory_search` / `memory_write` / `memory_forget`, **kahyad içinde Go — brain.db'nin TEK yazarı kahyad'dır**)". Tool protocol (§4): "Araç protokolü | **MCP** (%100 entegrasyon — kendi araçların dahil)".

Quarantine rule that gates injection eligibility (HANDOFF §5 memory #1, verbatim):

> 1. **Kaynak-güven kafesi:** her olgu `source_tier` ∈ {`user_edit`(1.0) › `user_asserted`(≤.95) › `external_doc`(≤.8) › `screen`(≤.7) › `agent_derived`(≤.4)}. Ajan-türevi karantinada, kullanıcı onaylayana dek profil kartından/enjeksiyondan hariç.

Forensic ledger rule (HANDOFF §5 safety #4): "Her model çağrısındaki enjekte `<hafiza>` bloğu kaydedilir (zehirlenme adli izlenebilirliği)." — kahyad renders the block AND ledgers exactly those bytes; the worker hook (W12-09) must inject the returned block byte-exact, so render-and-record live on the same side of the wire.

Transport: the worker is an MCP *client* over stdio. kahyad is a daemon, so ship a thin stdio↔UDS bridge binary: it speaks MCP stdio to the SDK and forwards JSON-RPC to kahyad's `/v1/mcp` endpoint over the UDS. All tool logic executes in kahyad (single writer preserved); the bridge is a dumb pipe. Use the official MCP Go SDK (`github.com/modelcontextprotocol/go-sdk`, pinned) with its streamable-HTTP handler mounted on the UDS server.

Prior output: W12-03 search + `/v1/memory/search`, W12-04 indexer + front-matter tier mechanism (`kahya_source_tier`). Action classes (§4): `memory_search` = R; `memory_write`/`memory_forget` = W1 — under W12-07's interim static policy all W tools are DENIED until W3 lands; that is expected and correct (test the tool logic directly in Go tests below the gate, not through the worker).

**Where the deny is enforced is itself a locked constraint** (HANDOFF §5 ⚑, verbatim):

> ⚑ **Uygulama düzlemi (önce oku):** `can_use_tool` bir **erken-ret/UX katmanıdır, güvenlik sınırı değildir** — worker sürecinin içinde çalışan bir SDK geri-çağrısıdır. Bağlayıcı politika kararı **kahyad'da** verilir; yan-etkili MCP araçları kahyad'ın verdiği **tek-kullanımlık onay jetonunu** doğrulamadan yürümez (ya da yan-etkili MCP sunucularını kahyad spawn edip sahiplenir, worker onlara yalnız kahyad üzerinden erişir).

This server takes the "kahyad owns the MCP server" branch of that ⚑, so the binding gate lives in kahyad's `/v1/mcp` tool dispatch (step 6) — if only the worker's `can_use_tool` blocked `memory_write`, a compromised worker could POST `/v1/mcp` directly and write memory. That must be impossible from day one.

## Deliverables
- `mcp/memory/server.go` + `server_test.go` — tool registrations + handlers (Go package compiled into kahyad, per §7 skeleton `mcp/memory/`).
- `mcp/memory/render.go` + `render_test.go` — `<hafiza>` block renderer.
- `kahyad/cmd/kahya-mcp/main.go` — stdio↔UDS bridge binary (built to `bin/kahya-mcp`).
- `kahyad/internal/policy/interim.go` + test — static interim allow/deny table (per W12-07 step 5), consumed here by the `/v1/mcp` gate and later mounted by W12-07's `/policy/check`.
- kahyad wiring: MCP handler at `POST /v1/mcp`; `/v1/memory/search` extended with `for_injection`.
- Ledger events: `hafiza_injected`, `memory_write`, `memory_forget`.

## Steps
1. Extend `POST /v1/memory/search` with `for_injection: bool` and `task_id`. When true: (a) exclude results whose episode `source_tier='agent_derived'` (quarantine, §5 memory #1 above — the other four tiers are injectable in W1–2; per-fact confidence gating arrives with W5-04); (b) render the block; (c) append ledger event `kind='hafiza_injected'`, payload `{task_id, chunk_ids, block_sha256, block}`; (d) return `{results:[…], hafiza_block:"…"}`.
2. Renderer: top k (default 6) hits, each truncated to 400 runes at a rune boundary with `…`; format exactly:
   ```
   <hafiza>
   - [<repo-relative-path>#<seq>] <text>
   </hafiza>
   ```
   Deterministic ordering (fusion score desc), total block ≤ 4000 runes (drop trailing entries to fit), empty results ⇒ empty string (hook then injects nothing).
3. `memory_search` MCP tool: args `{query: string, k?: int}` → returns the raw results (path, seq, text, score, source_tier). No `for_injection` from the model side — injection eligibility is only decided by kahyad-internal callers (the hook path), never by a model-chosen flag.
4. `memory_write` MCP tool: args `{content: string, file?: string}`. Validate: `file` is a relative path, canonicalized under `cfg.memory_dir` (reject traversal/absolute/symlink escape), default `inbox/YYYY-MM-DD.md` (append with a `\n\n---\n\n` separator). Write markdown with front-matter `kahya_source_tier: agent_derived` (new files) — agent writes are ALWAYS `agent_derived`; there is no argument to claim a higher tier. Then commit in the memory repo: resolve the repo root ONCE via `git -C <cfg.memory_dir> rev-parse --show-toplevel` (prod: `~/Kahya`; hermetic tests: the temp fixture repo — never hardcode `~/Kahya`, or the `KAHYA_MEMORY_DIR` override breaks), then `git -C <root> add <file> && git -C <root> commit --author="kahyad <kahyad@local>" -m "memory_write: <file>"`, then incremental reindex of that file, then ledger event `memory_write` `{file, bytes, commit_sha}`. Return `{file, episode_id, commit_sha}`.
5. `memory_forget` MCP tool: args `{file: string, heading?: string}`. With `heading`: remove that markdown section (heading line through the next same-or-higher-level heading), write back; without: `git mv` the file into `.trash/<unix-ts>-<basename>`. Commit as in step 4, reindex (the `.trash` skip + deleted-file handling from W12-04 make it unsearchable), ledger event `memory_forget`. Refuse to touch anything outside `cfg.memory_dir`.
6. **Kahyad-side policy gate (the binding boundary — §5 ⚑ above):** the `/v1/mcp` tool dispatcher consults the interim policy table before executing ANY `tools/call`, after canonicalizing SDK-prefixed names (`mcp__kahya_memory__memory_write` → `memory_write`). This task creates `kahyad/internal/policy/interim.go` (static allow/deny lookup + Turkish reasons, exactly the table specified in W12-07 step 5 — read it now); W12-07 mounts the SAME package behind `POST /policy/check`. One table, two mount points — never two tables. Under the interim table `memory_search` is allowed; `memory_write`/`memory_forget` are denied: return a JSON-RPC tool error carrying the policy's Turkish `reason` and append a `policy_decision` ledger event exactly like the endpoint does. Keep the gate↔handler interface stable: W3-02 replaces the interim deny with approval-token verification, the handlers below don't change.
7. Register all three tools on the MCP server with descriptions in Turkish (model-facing docs are user-language per product; keep arg names English). Mount handler at `/v1/mcp`.
8. `kahya-mcp` bridge: reads MCP frames on stdin, POSTs to `http://kahyad/v1/mcp` via UDS (`KAHYA_SOCKET` env or default path), streams responses to stdout; propagates `KAHYA_TRACE_ID` env as `X-Kahya-Trace-Id` header. No logic, no state; JSONL logs to stderr only.
9. Tests: traversal attempts (`../../etc/x`, absolute paths, symlink out) rejected; **gate test: `memory_write` called through `/v1/mcp` is denied with the interim policy's Turkish reason + a `policy_decision` ledger row, while the same handler invoked directly below the gate performs the write** (proves the boundary is kahyad-side, not `can_use_tool`); direct-invocation tests against a fixture memory repo in `t.TempDir()`: write→file exists with front-matter→git log shows kahyad author→search finds the new text; forget(heading) removes only that section; forget(file) makes text unsearchable but present under `.trash/` and in git history; `for_injection=true` excludes an `agent_derived` fixture episode while `false` includes it; renderer golden test with Turkish text (byte-exact, e.g. `Kadıköy'de iki daire gezdik`); `hafiza_injected` ledger row's `block` equals the returned `hafiza_block` byte-for-byte (compare sha256).

## Acceptance criteria
- [ ] `make test` green including all step-9 tests; `make build` additionally produces `bin/kahya-mcp`.
- [ ] End-to-end over the bridge: `echo` an MCP `tools/list` request into `bin/kahya-mcp` (daemon running) and get back exactly three tools: `memory_search`, `memory_write`, `memory_forget`.
- [ ] Via the bridge (daemon running): a `memory_write` `tools/call` returns a JSON-RPC tool error whose message is the interim-policy Turkish deny reason, no file/commit is created, and `sqlite3 brain.db "SELECT count(*) FROM events WHERE kind='policy_decision';"` incremented — the deny is enforced in kahyad, not in `can_use_tool` (§5 ⚑).
- [ ] Go test (direct handler below the gate, fixture memory repo in `t.TempDir()`): after a write, `git log -1 --format='%an'` in the fixture repo prints `kahyad`; `/v1/memory/search` finds the written text; events gains a `kind='memory_write'` row.
- [ ] `POST /v1/memory/search {"query":"…","for_injection":true,"task_id":"t1"}` returns a `hafiza_block` starting with `<hafiza>` and writes a `hafiza_injected` event whose payload `block_sha256` matches `echo -n "$BLOCK" | shasum -a 256` (§5 safety #4).
- [ ] A chunk from an `agent_derived` episode appears with `for_injection:false` and is absent with `for_injection:true` (quarantine test — permanent, feeds W78-03).
- [ ] With kahyad stopped, `bin/kahya-mcp` fails fast: a `tools/list` request gets a JSON-RPC error response and the process exits non-zero within 5s (no hang; stderr JSONL explains).

## Out of scope
- The `UserPromptSubmit` hook that consumes `hafiza_block` — W12-09. Policy enforcement/approval tokens for W1 tools — W12-07 interim deny + W3-02.
- Fact-level operations (extraction, confidence, retraction) — W5-04; `memory_forget` here is file/section-level only.
- Consolidation commits, `author=user` commit discipline, git push — W5-02 / W4-06.
- Any second MCP server (fs/shell/AppleScript) — W3-03/W3-04/W3-09.
