# W12-10 — W1–2 acceptance gate

**Status:** todo
**Phase:** W1–2 — Core
**Depends on:** W12-01..09
**Flags:** none
**Handoff refs:** §6 W1–2 acceptance, §5 safety #4

## Goal
Prove the W1–2 gate end-to-end and freeze it as an automated, repeatable check: a CLI question is answered with a `<hafiza>` injection from seeded memory, `'evlerimizden'` retrieves the `'ev'` seed note, one `trace_id` spans all JSONL logs, and every injected block is in the ledger. Nothing in Phase W3 starts until this is `[x]`.

## Context you need
The gate (HANDOFF §6 W1–2, verbatim):

> → **Kabul:** CLI'dan sorulan bir soru, tohumlanmış hafızadan bir `<hafiza>` bloğu enjekte edip yanıtlanıyor; **`'evlerimizden'` sorgusu `'ev'` içeren tohum notu buluyor** (Türkçe morfoloji); her şey tek `trace_id` taşıyor (JSONL loglarda doğrulanır).

FTS5-only is sufficient (HANDOFF §6 ⚑ timing note): "W1–2 kabulü FTS5-only aramayla sağlanır — embedding hattı … ayrı iş kalemidir, sığmazsa W3–4'e kayar" — do NOT wait for W12-11.

Ledger requirement being verified (HANDOFF §5 safety #4): "Her model çağrısındaki enjekte `<hafiza>` bloğu kaydedilir (zehirlenme adli izlenebilirliği)."

Two verification modes, both required:
1. **Hermetic gate (in `make test`, runs forever after):** everything real except the cloud — kahyad + real worker + real seeded-fixture corpus, with `cfg.anthropic_upstream_url` pointed at a mock Anthropic server (SSE `/v1/messages` responder). The mock asserts the request contains a `<hafiza>` block with the fixture note's text — proving injection reached the model call itself, not just the hook.
2. **Live gate (once, by hand, recorded):** same flow against the real API and the real `~/Kahya` seed corpus (W0-01). This is the §6 sentence made literal.

Prior output: all of W12-01..09; env overrides (`KAHYA_DATA_DIR`, `KAHYA_MEMORY_DIR`, `KAHYA_SOCKET`) from W12-01 make the hermetic mode possible.

## Deliverables
- `tests/e2e/w12_gate_test.go` — the hermetic gate (Go test tagged `e2e`, run by `make test`; skips with a clear message if `bin/*` artifacts or python venv are missing).
- `tests/e2e/mockanthropic/mock.go` — reusable mock Anthropic server (SSE + usage fields; also needed by W7-8 record-replay work).
- `tests/e2e/fixtures/memory/ev-notlari.md` — fixture corpus (byte-exact Turkish, includes the standalone word `ev`).
- `Makefile`: `accept-w12` target (live gate: runs the checks below against the real daemon, prints PASS/FAIL per criterion).
- `docs/ipc.md` appendix: "W1–2 gate — how to re-run" (3 commands).

## Steps
1. Fixture `ev-notlari.md` (front-matter-free, so it indexes as `user_asserted`):
   ```markdown
   # Ev arayışı
   İstanbul'da yeni bir ev bakıyoruz; Kadıköy öne çıktı, iki daire gezdik.
   ```
   Plus a decoy file (`gold-token-notlari.md` about NATS/saga) to make ranking non-trivial.
2. Hermetic gate test flow: build/locate binaries → `t.TempDir()` for `KAHYA_DATA_DIR` and a fixture-populated `KAHYA_MEMORY_DIR` (init a throwaway git repo there — memory_write paths need it) → set `KAHYA_ENV=dev` + `KAHYA_ANTHROPIC_KEY_OVERRIDE=hermetic-dummy` (W12-08's dev-only seam; without it the proxy 503s on the Keychain-less CI machine — the override is inert in prod by construction) → start mock Anthropic (asserts+records request bodies; streams a fixed Turkish SSE answer `Kadıköy öne çıkmıştı.` with plausible usage numbers) → start kahyad with `anthropic_upstream_url` = mock URL → `POST /v1/reindex` → run `bin/kahya "Evlerimizden ne konuşmuştuk?"` capturing stdout+the `iz:` trace footer.
3. Assertions (each its own named subtest so failures localize):
   - **retrieval:** `POST /v1/memory/search {"query":"evlerimizden","k":3}` → top-3 contains the `ev-notlari.md` chunk, ranked above the decoy.
   - **injection into the model call:** mock-recorded request body contains `<hafiza>` and the substring `Kadıköy öne çıktı` (fixture bytes, not a paraphrase).
   - **answer:** CLI stdout non-empty and equals the mock's streamed text.
   - **single trace_id:** extract `<T>` from the `iz:` footer; every line matching `<T>` in `kahyad.jsonl` + `worker.jsonl` parses as JSON with `.trace_id == <T>`; AND the set of events for the task (`task_spawned`, `policy_decision` if any, `hafiza_injected`, `model_call`, `task_done`) all carry `<T>` in the `events` table.
   - **ledger forensics (§5 #4):** the `hafiza_injected` payload's `block` sha256 equals the sha256 of the `<hafiza>…</hafiza>` bytes found inside the mock-recorded request body.
   - **derived-index property:** delete `brain.db`, restart kahyad, reindex, re-run the retrieval assertion — same top hit (SQLite is rebuildable from markdown; pre-verifies the W7-8 restore drill).
4. `make accept-w12` (live): assumes launchd daemon + real seed corpus + Keychain key. Runs: `bin/kahya health`; the `evlerimizden` search curl against the real corpus — top-3 must contain a seed note containing the substring `ev` (the real corpus has one: the seeded iOS home-design note, "iOS **ev** tasarım app"; the fixture's house-hunting text exists only in tests — do not expect `Kadıköy` here). If the user's post-review corpus somehow has no `ev` note, set `Status: blocked-user` and ask the user to confirm one — do not weaken the criterion; then `bin/kahya "Evlerimizden ne konuşmuştuk?"`; then greps both logs for the printed trace and runs the ledger sha256 comparison via `sqlite3` + `shasum`. Prints each criterion PASS/FAIL; exit non-zero on any FAIL.
5. Run the live gate once for real. Paste its full output into the task-completion commit message body.

## Acceptance criteria
- [ ] `make test` runs the hermetic gate green on a machine with no real `ANTHROPIC_API_KEY` and no Keychain item (mock upstream + `KAHYA_ENV=dev` key-override seam only).
- [ ] Subtest `retrieval` proves `'evlerimizden'` → note containing `'ev'` with the trigram index (this is additionally covered forever by W12-03's unit test).
- [ ] Subtest `single trace_id` passes: one `<T>` across kahyad.jsonl, worker.jsonl, and the events table for the whole flow.
- [ ] Subtest `ledger forensics` passes: `hafiza_injected.block` == bytes inside the actual model-call request (sha256 equality).
- [ ] `make accept-w12` against the live system exits 0; its output is captured in the `[W12-10]` commit message.
- [ ] BACKLOG.md rows W12-01..W12-10 can all be honestly marked `[x]` — if any earlier task's criteria regressed, fix it before closing this gate (the gate wins over convenience).

## Out of scope
- Embedding/KNN retrieval quality — W12-11 (slidable); the gate must NOT depend on vectors.
- Policy/approval flows beyond the interim R-allow table — W3-10 is that gate.
- Durability (SIGKILL/resume/offline) — W4-07. Red-team scenarios — W78-02. Retrieval QA precision ≥80% — W78-01.
- Fixing weak model answers by prompt tuning — the gate checks plumbing (injection/trace/ledger), not answer quality.
