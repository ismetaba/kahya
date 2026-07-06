# W3-10 — W3 acceptance gate

**Status:** todo
**Phase:** W3 — Policy + tools
**Depends on:** W3-01, W3-02, W3-03, W3-04, W3-05, W3-06, W3-07, W3-08
**Flags:** none
**Handoff refs:** §6 W3 acceptance, §5 safety #1/#5, §4 routing ⚑

## Goal
The W3 weekly gate turned into permanent, repo-resident tests plus a scripted manual checklist. When this task is done, every clause of the §6 W3 acceptance sentence is demonstrated by a command or test that any future session can re-run, and the W3 phase can be marked complete in BACKLOG.md.

## Context you need
The gate, verbatim (HANDOFF §6 W3):

> → **Kabul:** W2 bir eylem Telegram'dan byte-tam diff ile onay istiyor; W3 eylem **Telegram'dan onaylanamıyor, CLI'dan yazılı "onayla" ile geçiyor**; gizli-şerit dokunuşlu eylemler yerel onaya düşüyor; egress allowlist devrede + **container içi `curl` allowlist'i atlayamıyor (test)**; gizli-şerit içerik bulut çağrısına çıkamıyor (test).

Note the parenthetical `(test)` items — those two MUST be automated tests, not manual demos. The other three clauses get both an automated test (fakes where surfaces are remote) and a one-time manual demo against the live system. W3-09 is intentionally NOT a dependency (slidable per §6 timing note). These tests are the seed of the §5 invariant CI suite that W78-03 later collects — write them to last, not as throwaway scripts. Everything you need already exists: engine + tokens (W3-02), fs tool (W3-03), Docker runner + `KAHYA_DOCKER_TESTS` guard (W3-04), egress gate + internal network (W3-05), WYSIWYE + CLI surface + `surface:"local"` enforcement (W3-06), Telegram bot with fake transport (W3-07), secret-lane router + proxy backstop (W3-08).

Global convention reminder (tasks/README.md): fail-closed everywhere; every log line JSONL with `trace_id`; user-facing strings Turkish, byte-exact fixtures.

Mapping table — gate clause → test → mechanism under test:

| § Clause | Test | Mechanism |
|---|---|---|
| W2 Telegram byte-tam diff onayı | Gate 1 | W3-02 tokens + W3-06 diff + W3-07 card |
| W3 Telegram'dan onaylanamıyor / CLI "onayla" | Gate 2 | W3-06 surface rule + W3-07 notify-only |
| Gizli-şerit → yerel onay | Gate 3 | W3-08 classifier + W3-07 redaction |
| Container `curl` allowlist'i atlayamıyor | Gate 4 | W3-04 network flags + W3-05 internal net/proxy |
| Gizli-şerit bulut çağrısına çıkamıyor | Gate 5 | W3-08 proxy backstop + ordering invariant |

## Deliverables
- `tests/w3/gate_test.go` (package `w3gate`) — the five gate tests below, using kahyad started in-process or as a child process against a temp `KAHYA_ENV=dev`-style config (temp brain.db, fixture policy.yaml, fake Telegram transport, stub Anthropic upstream behind the real W12-08 proxy).
- `tests/w3/fixtures/` — fixture policy.yaml (tiny budgets, test hosts), secret-lane content fixtures (Turkish, byte-exact), W2/W3 tool-call payloads.
- Makefile: `make test` runs `./tests/w3/...`; Docker-dependent tests gated by the existing `KAHYA_DOCKER_TESTS=1` (exported automatically when `docker info` succeeds).
- `docs/w3-gate-checklist.md` — the manual live-surface checklist (Telegram card + CLI onayla), with the exact commands and expected ledger rows; filled in once with the run's date + trace_ids.

## Steps
1. Test harness: boot kahyad with temp dirs (`t.TempDir()` for App Support path override — add a `KAHYA_APP_SUPPORT` env override to kahyad config if W12-01 didn't), fixture policy.yaml, fake Telegram transport injected, W12-08 proxy pointed at a local stub upstream recording every request.
2. **Gate test 1 — W2 needs byte-exact Telegram diff approval:** request `fs_write` (W2 via fixture policy that maps it W2, or use `shell_docker`) at L0; assert a Telegram card is emitted on the fake transport containing the byte-exact WYSIWYE diff (compare against expected canonical bytes, Turkish filename fixture included); approve via simulated callback; assert execution happened exactly once and ledger chain `policy_decision → approval(remote) → token consume → exec` shares one `trace_id`.
3. **Gate test 2 — W3 not approvable via Telegram, passes via CLI "onayla":** request a W3 tool (`mail_send` stub). Assert: fake transport received ONLY the notify-card (no inline keyboard); a forged approval callback for it is rejected (`w3_nonlocal_approval_rejected` in ledger); driving the CLI surface programmatically (UDS approval endpoint with `surface:"local"`, input `evet`) is rejected; input `onayla` succeeds; execution follows only after `onayla`.
4. **Gate test 3 — secret-lane-touching actions fall to local approval:** payload whose content matches the classifier's deterministic pre-pass (fixture: `IBAN TR33 0006 1005 1978 6457 8413 26 için ödeme talimatı`); assert NO Telegram card with payload bytes is emitted (at most redacted title), the pending approval is routed to the CLI surface, and the session's sensitive-read flag is set.
5. **Gate test 4 (automated, Docker) — in-container curl cannot bypass the egress allowlist:** run the W3-04 sandbox with `needs_network: true` on the `kahya-egress` internal network; inside, attempt (a) `curl` via proxy env to a non-allowlisted host ⇒ 403, (b) `curl --noproxy '*' https://1.1.1.1` ⇒ no route/timeout, (c) DNS resolution of any external name ⇒ failure; and with default `--network none` ⇒ all egress fails. Assert corresponding `egress_blocked_*` ledger events for (a).
6. **Gate test 5 (automated) — secret-lane content cannot reach a cloud call:** mark a task secret-lane via the classifier path, have the worker-side (or a direct HTTP call standing in for it) attempt a completion via the W12-08 proxy; assert 403, `secretlane_cloud_blocked` ledger event, and the stub upstream recorded ZERO requests for that `trace_id`. Include the hanging-classifier variant: classification incomplete ⇒ zero bytes upstream (ordering invariant).
7. Manual checklist (`docs/w3-gate-checklist.md`): live Telegram W2 card approve (mirrors test 1), live W3 CLI `onayla` (mirrors test 2), live secret-lane demo with `🔒 yerel işlendi` badge and `pgrep -f mlx_lm.server` showing the local model. Record trace_ids and `sqlite3` ledger query outputs in the doc.
8. Run the full suite: `make test` green, `make lint` green. Mark W3-10 done in BACKLOG.md per protocol; if W3-09 slid, leave it `[ ]` with a note — it does not block this gate.

## Acceptance criteria
- [ ] `go test ./tests/w3/... -count=1` green in `make test`; with Docker running, `KAHYA_DOCKER_TESTS=1 go test ./tests/w3/... -run TestGate -v` shows all five gate tests RUN (zero skips).
- [ ] Gate test 1 asserts byte-exact diff equality (not substring) between the approval card and the canonical payload, including the Turkish filename fixture.
- [ ] Gate test 2 proves both rejections (forged Telegram callback AND CLI `evet`) and the `onayla` success path in one test, with ledger rows for each.
- [ ] Gate test 4 covers all four bypass vectors: proxy-403, direct-IP, DNS, and network-none.
- [ ] Gate test 5 asserts zero upstream requests for the secret-lane `trace_id` at the recording stub — the strongest form of "gizli-şerit içerik bulut çağrısına çıkamıyor".
- [ ] `docs/w3-gate-checklist.md` committed with a completed live run (date, trace_ids, ledger query outputs pasted).
- [ ] All five tests are plain `go test` tests (no manual setup beyond Docker running) so W78-03 can lift them into CI unchanged.

## Out of scope
- Fixing bugs found by the gate — file/fix them under the owning task ID and its file (never bundle two tasks in one commit), then re-run this gate.
- W3-09 verification — its own acceptance criteria apply when it lands (slidable).
- Red-team suite (homoglyph bypass, poisoned mail, etc.) — W78-02, under `KAHYA_ENV=dev` profile.
- Taint persistence across restart tests — W4-03/W78-02.
- CI workflow wiring — W78-03 (tests must merely be CI-liftable now).
