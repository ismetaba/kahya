# W78-03 — §5 invariant CI tests + coverage map

**Status:** todo
**Phase:** W7–8 — Hardening + eval
**Depends on:** W3-10, W4-07, W5-05
**Flags:** none
**Handoff refs:** §5 preamble ⚑, §6 W7–8

## Goal

Every §5 invariant (safety #1–#6, memory #1–#5, product principles) is mapped to at least
one **permanent** automated code test, all collected into `make test` and a CI workflow.
A committed `docs/coverage.md` maps each invariant → its test(s), so a reviewer can verify
no invariant is untested.

## Context you need

Binding HANDOFF items (verbatim):

> **W7–8 · Sağlamlaştırma + değerlendirme.** … **§5 değişmez kod-testleri CI'da**; … → **Kabul:** … tüm §5 değişmez testleri yeşil; …

> ⚑ Her değişmezin hangi hafta inşa edildiği ve kod-testi §6'da bir kabul kriterine bağlıdır.

README convention: *"§5 invariants get permanent regression tests (collected in CI by W78-03)."*

The full invariant list to cover (from §5):

Enforcement plane (§5 preamble ⚑):
0. `can_use_tool` is an early-reject/UX layer, **NOT a security boundary** — the binding policy decision is made in kahyad, and side-effectful MCP tools refuse to execute without a valid **one-time approval token** issued by kahyad (W3-02). The test must prove a tool call refuses when the token is missing/invalid even if `can_use_tool` was bypassed entirely.

Safety:
1. Egress is a first-class gated capability (allowlist + volume; allowlist-external hard-blocked after sensitive read; approval cards count as egress; container default `--network none`).
2. Data-level taint + two-LLM (toolless Reader → schema-validated struct → clean Actor; taint only rises; missing record ⇒ untrusted).
3. `source_tier` + profile-card human gate (untrusted-sourced memory can't enter profile card / reflex injection).
4. Externally-anchored ledger (append-only separate-credential remote; every `<hafiza>` block logged).
5. WYSIWYE approval (NFC/bidi/zero-width/homoglyph normalize; canonical path/host; executed≠approved ⇒ reject; W3 local-only; secret-lane diff never to Telegram; Telegram chat_id allowlist Go-side).
6. Shell (binary allowlist is not a boundary; model-written shell defaults to Docker `--network none`; `osascript`/JXA ≥W2; fs write-deny globs).

Memory:
1. Source-trust lattice (`user_edit`›`user_asserted`›`external_doc`›`screen`›`agent_derived`; agent-derived quarantined).
2. Splittable, evidence-gated entity merge (never name-similarity alone; merge ledger + split op; suspicious same-name ⇒ new provisional entity).
3. Negative evidence + log-odds confidence (same-session repeat = one evidence; retraction; <0.3 exits injection).
4. ≥90-day hot window + detail-atom promotion; every summary from raw evidence, never sub-summary.
5. Ground-truth eval fed by human labels (gate before any consolidation/embedding/fusion change).

Product principles:
- W3 always requires persistent written "onayla".
- Security in the executor, not the prompt (static class metadata checked in Go before the tool runs).
- Privacy in code (finans/sağlık/kimlik → local-only Go branch + "yerel işlendi" badge).
- Memory never lowers a permission.

Most of these already have a test from the phase that built them (W3-10, W4-07, W5-05,
W12 acceptance, W78-01/02). This task's job is to **collect and fill gaps**, not to
re-implement enforcement.

## Deliverables

- `docs/coverage.md` — a table: invariant id (incl. preamble #0) → invariant text (short) → test function(s) `pkg/TestName` → owning task → `ci-hermetic` or `local-integration`. Every row must cite a real test that exists in the repo, and **every invariant must cite at least one CI-hermetic test**.
- New/gap-filling tests in the appropriate packages (`kahyad/internal/policy`, `.../egress`, `.../taint`, `.../memory`, `.../ledger`, `.../approval`) for any invariant not already covered.
- `.github/workflows/ci.yml` (or the repo's CI location) running `make test` + `make lint` on every push; matrix pinned to the Go toolchain and the pinned `claude-agent-sdk` version; red-team + retrieval evals invoked with record-replay fixtures and **synthetic** fixture corpora/datasets only (no network, no secrets, and NO real user memory — the real dataset lives in `~/Kahya`, per W78-01). **Hermeticity rule:** MLX, Docker, Keychain and the SDK are faked/replayed at the enforcement-decision layer in CI. Heavier real-integration variants (the real in-container `curl` bypass test from W3-10, real MLX load/unload/memory-pressure) are tagged `local-integration` in `docs/coverage.md` and run in the full local `make test` — W78-06 `make readiness` executes them on the dev machine.
- `Makefile`: `make invariants` target that runs only the invariant-tagged tests and fails if `docs/coverage.md` references a test that no longer exists.

## Steps

1. Read §5 in full plus the acceptance sections of §6 (each invariant is tied to a weekly gate). Inventory existing tests from W3-10, W4-07, W5-05, W12-10, W78-01, W78-02.
2. Build the coverage inventory: for each of the ~16 invariants (preamble #0 + 6 safety + 5 memory + 4 product principles), find an existing test or note a gap.
3. Write gap-filling tests. Each must be a real assertion of the enforcement (e.g. policy engine returns DENY on timeout; taint record missing ⇒ untrusted; fs write to `~/.zshrc` denied; W3 approval from Telegram rejected but CLI accepted; agent_derived fact excluded from injection). Tag them (Go build tag `invariants` or a naming convention) so `make invariants` can select them.
4. Write `docs/coverage.md` with the full mapping; add a small test (or a `make invariants` preflight) that parses `docs/coverage.md` and fails if any referenced test symbol is absent — keeps the map honest.
5. Author the CI workflow: checkout, set up pinned Go + Python env, `make build`, `make lint`, `make test` (CI-hermetic subset — `local-integration`-tagged tests excluded via build tags), `make eval-redteam` and `make eval-retrieval` with replay fixtures + synthetic corpora. No real Keychain/network/user data; secrets stubbed.
6. Run `make invariants`, `make test`, `make lint` locally; ensure the CI workflow is green (or a dry run of its steps passes locally).

## Acceptance criteria

- [ ] `docs/coverage.md` exists and lists the enforcement-plane preamble (#0), all six safety invariants, all five memory invariants, and all four product principles — each with ≥1 named **CI-hermetic** test and owning task (local-integration variants listed additionally where they exist).
- [ ] `make invariants` runs the tagged tests, all green, and fails if `docs/coverage.md` cites a nonexistent test (verified by temporarily renaming a test in a scratch run).
- [ ] CI workflow file present and its `make test` + `make lint` + eval steps pass with record-replay fixtures and no network access.
- [ ] Spot-check tests exist and pass for: egress hard-block after sensitive read (#1); taint missing-record⇒untrusted (#2); fs deny-glob on `~/.zshrc` (#6); WYSIWYE executed≠approved reject (#5); agent_derived quarantine from injection (memory #1); W3 Telegram-reject/CLI-accept (product principle); `POST /policy/check` error/timeout ⇒ DENY fail-closed (§4 IPC ⚑); secret-lane under simulated memory pressure or model-load failure ⇒ FAIL-CLOSED with **zero requests observed at the cloud forward-proxy** (§4 memory-pressure ⚑ — never falls back to cloud); side-effectful MCP tool without a valid kahyad one-time approval token refuses even when `can_use_tool` is bypassed (preamble #0).
- [ ] `make test` and `make lint` green.

## Out of scope

- Adding NEW enforcement behavior — if a test reveals a real gap in enforcement, fix it in the owning subsystem's follow-up, not by weakening the test. This task collects and verifies existing invariants.
- Metrics CLI (W78-04), backup drill (W78-05), dogfood checklist (W78-06).
- §8 deferred items (they have no invariant to cover in MVP).
