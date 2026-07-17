# Kâhya — Project Review Findings

**Review date:** 2026-07-17
**Scope:** Full codebase (~114k LOC Go + Python), design conformance to `docs/HANDOFF.md` §4/§5, and "will it work" operability.
**Method:** 10 parallel finder passes across every safety invariant and subsystem, each finding adversarially verified by an independent skeptic (27 raw → 7 refuted → 17 confirmed/1 plausible). The top blockers were then re-verified by hand against the actual code.

---

## Bottom line: will it work?

**Verdict: `has-blockers`.** The architecture is genuinely strong and the safety-critical machinery is real (see "What is solid" below) — but the MVP **cannot ship safely as-is**. Three release-gating problems:

1. **It won't authenticate in the default config.** `credential_mode` defaults to `passthrough`, which strips the only credential the worker sends, so every cloud task 401s out of the box. (Finding #4)
2. **A prompt-injected worker can self-approve W3 actions** (money / identity / messaging-as-user) over the same control socket it already holds — the "local approval" gate is a self-declared flag, not an authenticated origin. (Finding #1)
3. **An ordinary "summarize this file" request leaks finans/sağlık/kimlik file contents to the cloud** because a sensitive `fs_read` never escalates the task's lane. (Finding #2)

All three are fixable without a redesign. None indicate the design is wrong — they are specific gaps in an otherwise rigorous defense.

### What is solid (verified by hand)

- **Builds clean.** All four Go binaries compile; Go test suite is 46/50 packages green (the 4 failures are macOS-only tools absent on the Linux review box — `vm_stat`, the Python/MLX worker, a git default-branch quirk — not product bugs; CI runs on `macos-latest`).
- **Schema complete.** 17 clean sequential migrations create every §5-mandated table with full column sets (facts with source_tier/evidentiality/log-odds confidence/valid_from-to, entities+aliases, evidence, merge_ledger, tasks, events/outbox, plus FTS5 + vec0).
- **Taint ledger is genuinely fail-closed.** `Get` returns `tainted` on both no-row and any DB error; `Raise` only ever raises; `InsertClean` rejects lowering. (`kahyad/internal/taint/taint.go`)
- **Secret-lane routing is enforced in Go.** Classification runs before the task row is written; a secret-lane task takes a separate path that never spawns a worker or opens the proxy at all. (`kahyad/internal/server/task.go:765`)
- **One-time approval tokens are textbook-correct.** 32 random bytes, only SHA-256 persisted, atomic single-use burn (`UPDATE ... WHERE consumed_at IS NULL`), token burned before the bytes-hash check, WYSIWYE canonicalization applied identically at mint and consume. (`kahyad/internal/policy/tokens.go`)
- **Egress is a single mutex-guarded chokepoint** with fail-closed canonicalization/budget errors and exact-match allowlisting. (`kahyad/internal/egress/gate.go`)

The gaps below are exactly that — gaps in a system whose bones are sound.

### The four user-blocked tasks

Only **W0-04 (Keychain)** actually gates operation, and finding #4 makes it doubly load-bearing: `keychain` mode is the only credential path that authenticates upstream, so no cloud task can run until the Keychain key is provisioned **and** `credential_mode` is switched to `keychain`.
- **W0-05 (design.html):** documentation only; does not gate runtime (but see #16 — coverage.md falsely claims it "covered").
- **W3-07 (Telegram):** optional notify-only surface; daemon runs fully without it (but the Telegram code that exists has the #5 redaction hole).
- **W3-09 (AppleScript):** one optional W2 tool; the assistant functions without it.

---

## Findings

Severity: **blocker** (ships broken / defeats a defining safety promise) · **high** (real exploit or data-loss under normal use) · **medium** (correctness/invariant erosion) · **low** (hygiene, hardening, TOCTOU, accounting).

| # | Sev | Title |
|---|-----|-------|
| 1 | blocker | Worker can self-approve W3 over the shared control socket |
| 2 | blocker | Sensitive `fs_read` leaks secret-lane file contents to the cloud |
| 3 | high | `shell_host git` on a model-writable repo runs arbitrary host code |
| 4 | high | Default `passthrough` credential mode 401s every cloud task |
| 5 | high | Telegram secret-lane redaction misses all script/message tools |
| 6 | high | Outbox re-claims done/failed tasks forever (zombie loop) |
| 7 | high | Re-affirming a denied fact never recovers its confidence |
| 8 | medium | CONNECT tunnel volume budget is never enforced per-connection |
| 9 | medium | 5-minute W1 undo collapses to "last write only" per task |
| 10 | medium | Incremental reindex freezes stale `source_tier` on unchanged bytes |
| 11 | medium | `sqlite-vec` pinned at 0.1.6, below the locked ≥0.1.9 floor |
| 12 | low | No wired path raises session taint from ingested content |
| 13 | low | Plain-HTTP request body double-counted against egress budget |
| 14 | low | `shell_docker` verifies digest then runs by mutable tag (TOCTOU) |
| 15 | low | Truncated cloud calls never billed to the cost governor |
| 16 | low | `coverage.md` marks a missing/blocked deliverable "covered" |
| 17 | low | `make test` hard-fails container tests if sandbox image absent |

---

### #1 — [BLOCKER] Untrusted worker can self-approve W3 actions over the shared control socket

**Invariants violated:** 4 (W3 always requires *written local* approval; Telegram/worker may notify but never approve) and 5 (worker is not a security boundary; binding decisions live in kahyad and must be human-gated).
**Where:** `kahyad/internal/server/policy.go:136`, `kahyad/internal/server/approvals_decision.go:103`, routes mounted on the single mux `kahyad/internal/server/server.go:390-436`, worker socket handed out at `kahyad/internal/spawn/spawn.go:566`.

**The defect.** The entire W3 written-local-approval guarantee rests on one check in `Engine.Approve`: `surface != "local"` (`engine.go:1060`). But `surface` is not authenticated to a human origin:
- `handlePolicyFeedback` reads the caller-supplied JSON field `req.Surface` and passes it straight to `Approve`, so a body of `{"kind":"approve","pending_approval_id":X,"surface":"local","typed":"onayla"}` is accepted as local.
- `handleApprovalsDecision` hardcodes `surface="local"` for **any** UDS caller.

Both routes are on the **same** control mux that the worker is handed via `KAHYA_SOCKET=cfg.Socket`. Worker and CLI run as the same uid with no per-connection auth, no `SO_PEERCRED` check, and no separate socket. The code comments at `mcp.go:256,290` already concede this: *"arbitrary POSTs to the shared control socket … All workers share one control socket today with no [auth]."*

**Failure scenario.** A prompt-injected/compromised worker: `POST /policy/check` for a W3 tool → receives `pending_approval_id` X → `POST /policy/feedback {kind:approve, pending_approval_id:X, surface:local, typed:onayla}` (or `POST /approvals/{id}/decision {approve:true, typed:onayla}`) → `Approve` mints a one-time W3 token bound to the worker's own `(task_id, tool, approved_bytes_hash)` → worker consumes it and executes the W3 action. No human ever saw an approval card. This defeats the single most load-bearing invariant of the product.

**Fix (detailed).**
1. `config.go`: add a `ControlSocket` field defaulting to a distinct filename in the same dir as `Socket` (e.g. `<dir>/kahyad-control.sock`), with the same `~`-expansion/dev-rename logic. Also generate a per-boot 32-byte `crypto/rand` secret held only in memory on `Server`.
2. `server.go` `Prepare()`: create a **second** `net.Listener` on `ControlSocket` (chmod 0600), build a **second** mux, and mount **only** the human/privileged routes there: `POST /approvals/{id}/decision`, `GET /approvals/pending`, `POST /policy/feedback`, `POST /policy/promote`, `POST /policy/undo`, `POST /halt`, `POST /debug/emit-approval`. **Remove** those routes from the worker-facing mux, leaving it only `/policy/check`, `/policy/consume-token`, `/v1/mcp`, `/v1/memory/*`, `/v1/task*`, `/health`. Serve the second listener from its own goroutine; unlink it in `Shutdown`.
3. Wrap the control mux in middleware requiring `Authorization: Bearer <boot-secret>` compared with `crypto/subtle.ConstantTimeCompare`; missing/mismatch → 401 **without** touching `policyEngine` (fail-closed).
4. `policy.go`: delete the `Surface` field from `policyFeedbackRequest`; `handlePolicyFeedback` (now only reachable on the control listener) calls `Approve(ctx, id, "local", req.Typed)`. `approvals_decision.go` keeps its hardcoded `"local"` (now safe). `Engine.Approve` is unchanged.
5. `spawn.go:566`: keep `KAHYA_SOCKET=<Socket>` for the worker; do **not** add `ControlSocket` or the secret to worker env/argv, and add the secret's env var name to `secretEnvDenylist`. Inject the bearer only into the CLI (`cmd/kahya/client.go`, from `KAHYA_APPROVAL_TOKEN` or a 0600 file kahyad writes) and Hammerspoon (`hammerspoon/kahya.lua`), pointing their approve/deny/promote/undo/decide/halt calls at `ControlSocket`.
6. **Regression test:** dial only the worker socket, obtain a W3 `pending_approval_id` from `/policy/check`, then assert both `POST /policy/feedback {kind:approve}` and `POST /approvals/{id}/decision {approve:true}` return 404 (route not mounted); a companion test on the control socket with the bearer asserts approve succeeds.
7. **Record as a known limitation:** a full same-uid RCE worker can still read another same-uid process's environ / any user-readable file, so the boot secret is not truly hidden from RCE. The real fix is OS-level privilege separation (dedicated non-privileged uid or a macOS `sandbox-exec` profile with no read access to the control socket/secret). Until then, extend the `mcp.go` accepted-limitation note to state the W3 boundary is enforced against a prompt-injected model and a worker confined to intended channels, **not** a full same-uid RCE.

---

### #2 — [BLOCKER] Sensitive `fs_read` of a finans/sağlık/kimlik file leaks its contents to the cloud

**Invariants violated:** 2 (no byte reaches a cloud model before secret-lane classification; secret-lane work never falls back to cloud) and 7 (post-sensitive-read egress control).
**Where:** `mcp/fs/server.go:441-471`, `kahyad/internal/server/task.go:653`, `kahyad/internal/secretlane/classifier.go:269-271`, `kahyad/internal/secretlane/router.go:155-166`.

**The defect.** Task classification at creation time uses **`ClassifyDeterministic`** (regex/lexicon only, no Qwen), which returns `SecretLane:false` on a non-match and never fails closed. The `kimlikKeywords` lexicon contains `"tc kimlik"`, `"kimlik numarası"`, `"pasaport numarası"` — but **not** bare `kimlik` or `pasaport`. So a prompt like *"Documents/kimlik/pasaport.txt dosyasını özetle"* trips nothing → `lane=normal` → a **cloud** worker is spawned. The worker `fs_read`s the path, which **does** match the secret glob `~/Documents/kimlik/**` (`policy.yaml`). But `HandleRead` on a secret hit only (a) marks the egress `SensitiveTracker` and (b) returns the file's base64 bytes to the worker — it does **not** escalate the owning task's lane (the comment at `task.go:253` confirms `Escalate` is "not read by handleTask today"). The task stays `lane=normal`, so the proxy backstop (which only 403s `lane==secret`) never fires, and the sensitive-read egress rule only blocks allowlist-**external** hosts — `api.anthropic.com` is allowlisted, so it is not blocked.

**Failure scenario.** User: *"summarize `~/Documents/kimlik/pasaport.txt`"* → classified `normal` → cloud worker → `fs_read` returns the passport file bytes to the worker → worker sends them to `api.anthropic.com`. The exact category of data the product promises never to send off-box is exfiltrated to the cloud.

**Fix (detailed).** Make an `fs_read` of a secret-lane path stickily escalate the owning task's lane to secret, so the already-wired proxy backstop 403s the worker's next cloud call. All changes run in-process in kahyad (kahyad stays sole brain.db writer).
1. `mcp/fs/server.go`: add `SecretLaneEscalator { EscalateTaskLane(ctx, taskID, traceID, category string) error }` interface + a `Server` field. Thread `taskID` into `HandleRead` (change its signature; the call site already has `taskIDFromRequest(req)` at `server.go:661`).
2. Inside the `if secretLane {` block, **after** `MarkSensitiveRead` and **before** returning content, fail-closed escalate:
   ```go
   if s.SecretLaneEscalator != nil && taskID != "" {
       if err := s.SecretLaneEscalator.EscalateTaskLane(ctx, taskID, traceID, ""); err != nil {
           return FsReadOutput{}, fmt.Errorf("fs_read: secret-lane escalation failed: %w", err)
       }
   }
   ```
   Returning the error (not content) on escalation failure is essential — otherwise the backstop won't yet see `lane=secret` when the worker makes its cloud call.
3. `kahyad/internal/server`: implement the escalator over the existing `SecretLaneStoreAdapter.SetTaskLane` (`secretlane_adapter.go:39`) plus `markSensitiveRead`:
   ```go
   func (e fsSecretLaneEscalator) EscalateTaskLane(ctx, taskID, traceID, category string) error {
       if category == "" { category = secretlane.CategoryUnknown }
       if err := e.store.SetTaskLane(ctx, taskID, secretlane.LaneSecret, category); err != nil { return err }
       if e.mark != nil { _ = e.mark(ctx, traceID, traceID) }
       return nil
   }
   ```
   Because it only ever runs on a secret hit, setting `lane=secret` is never a downgrade.
4. `main.go`: build the escalator using the same `st.Queries` adapter created for the backstop and the `markSensitiveRead` closure; set `fsTool.SecretLaneEscalator` after `mcpfs.New`.
5. **Regression test:** a `LaneNormal` task whose worker `fs_read`s a secret-glob path must, on its next request through the egress factory, receive `MsgSecretLaneCloudBlocked` from the backstop; assert `SetTaskLane` was called with `lane=secret`, and that a failed `EscalateTaskLane` makes `HandleRead` return an error instead of `ContentBase64`.
6. **Defense-in-depth (secondary):** in `gate.go` add a per-host `content_carrying` flag (true for `api.anthropic.com`/`api.telegram.org`/`api.github.com`) and change line 273 to `if sensitive && (!matched || g.isContentCarrying(canonical))`. The task-lane escalation is the load-bearing fix because `task_id` is the stable identity the backstop consults.

> Consider also closing the root cause: run the **local Qwen classifier** (not just the deterministic lexicon) for task classification, or broaden the lexicon to include bare `kimlik`/`pasaport`/`sağlık` path tokens, so the task is classified secret up front and never spawns a cloud worker at all.

---

### #3 — [HIGH] `shell_host git` on a model-writable repo executes arbitrary host commands (Docker sandbox bypass)

**Invariants violated:** 10 (all model shell runs in Docker `--network none`) and 8 (executed bytes ≠ approved bytes).
**Where:** `mcp/shell/hostexec.go:147-200`, `mcp/shell/runner.go:188`, `policy.yaml` deny-globs.

**The defect.** `shell_host` allows `git status|log|diff|show` on the **host** with an inherited environment. `validateGitArgs` only checks the subcommand token and rejects flag-shaped trailing args — but git honors **repo-local** config from `<repo>/.git/config` for exactly these subcommands: `[core] fsmonitor=<cmd>` runs an arbitrary program on any index-reading command including `git status` (reproducible on git 2.43 — a marker file is written purely by `git status --porcelain`); `[diff "x"] textconv`/`command` and `[diff] external` run programs during `diff`/`show`. `RepoPath` is model-supplied and only canonicalized (no workdir-scope gate), and the fs write-deny globs cover only shell rc / LaunchAgents / `~/.hammerspoon` / App Support/Kahya — a scratch repo dir and its `.git/config` are freely writable.

**Failure scenario.** A worker at L2+ (auto-W1) `fs_write`s `~/scratch/r/.git/config` with a malicious `core.fsmonitor`, then calls `shell_host {command:git, repo_path:~/scratch/r, args:[status]}`. The WYSIWYE approval card shows only the innocuous `git status ~/scratch/r`, but on approval kahyad runs attacker code **on the host, outside the `--network none` Docker sandbox** — full host code execution behind a deceptive approval card.

**Fix (detailed).** Do both layers.
- **Layer 1 (argv-level; `-c` provably overrides repo-local config):** in `hostexec.go` build the git argv as
  `git -C <canonRepo> -c core.fsmonitor=false -c core.hooksPath=/dev/null -c diff.external= -c uploadpack.packObjectsHook= <sub> [--no-ext-diff --no-textconv if sub ∈ {diff,show,log}] <rest of in.Args>`.
  Leave `buildHostExecToolInput` / the WYSIWYE hash unchanged (only the executed argv is hardened).
- **Layer 2 (env scrubbing):** give `processExecutor.Run` (`runner.go:188`) an option to set `cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")` for the git path; thread the flag through `NewHostExec`.
- **Residual/complete fix:** `filter.<name>.clean/smudge` can still run during `git diff` on tracked files and cannot be blanket-disabled by one `-c`. For full closure, run these subcommands **inside the pinned `shell_docker` sandbox** with the repo bind-mounted read-only. If Docker isn't adopted, additionally refuse the request when `<repo>/.git/config` (best-effort; remember `include`/`includeIf`) defines any `*.command`/`*.textconv`/`diff.external`/`core.fsmonitor`/`filter.*.clean`/`filter.*.smudge` key.
- **Test:** temp repo with `core.fsmonitor` set to a marker-writing script → run `HostExec.Handle git status` → assert the marker was **not** written and the executed argv contains `-c core.fsmonitor=false`.

---

### #4 — [HIGH] Default `passthrough` credential mode has no viable upstream-auth path; every cloud task 401s

**Where:** `kahyad/internal/config/config.go:919`, `kahyad/internal/spawn/spawn.go:500,569`, `kahyad/internal/anthproxy/proxy.go:319-321,523-546`.

**The defect.** `credential_mode` defaults to `passthrough`. `BuildEnv` unconditionally sets `ANTHROPIC_API_KEY=<per-task local token>` in the worker env. The claude CLI/SDK, when `ANTHROPIC_API_KEY` is set, sends that value as `x-api-key` — which `checkLocalAuth` accepts, and the passthrough `Director` then **strips** via `stripTokenHeader`, leaving the outbound `/v1/messages` request with **no** upstream credential. Passthrough assumes the worker attaches its own separate session credential to forward unchanged, but that header doesn't exist: the token-as-`ANTHROPIC_API_KEY` suppresses the CLI's OAuth bearer, OAuth bearers aren't sent to a custom `ANTHROPIC_BASE_URL` anyway, and `ANTHROPIC_AUTH_TOKEN` is removed by `secretEnvDenylist`. Result: upstream 401 → every task fails. Hermetic tests pass only because their mock upstreams never check auth.

**Failure scenario.** Fresh install, default config, any real cloud prompt → 401 → task fails. The assistant "runs but does nothing." Recoverable only by switching `credential_mode` to `keychain` (ties this to W0-04).

**Fix (detailed).**
- **Part A — stop the local token shadowing the credential:** in `BuildEnv`, gate on credential mode. `keychain`: keep `ANTHROPIC_API_KEY=<token>`. `passthrough`: do **not** set `ANTHROPIC_API_KEY`; instead set `ANTHROPIC_CUSTOM_HEADERS="X-Kahya-Task-Token: <token>"` (the CLI forwards it). In `checkLocalAuth`, in passthrough mode validate the token from `X-Kahya-Task-Token`. In the passthrough `Director`, replace `stripTokenHeader` with `req.Header.Del("X-Kahya-Task-Token")`.
- **Part B — give passthrough a real upstream credential:** add a config key `upstream_bearer` (or read it from Keychain in passthrough mode) and have the anthproxy inject it in the passthrough `Director` exactly as keychain mode injects the key: after deleting the local-token header, `req.Header.Set("Authorization", "Bearer "+bearer)`. The bearer lives only in kahyad, never in the worker env (preserving the key-never-to-worker invariant and the `sk-ant-` leak guard in `__main__.py`).
- **Part C — coverage:** add a hermetic test whose mock upstream **requires** a specific `Authorization`/`x-api-key` and 401s otherwise, asserting a passthrough call reaches upstream **with** the injected credential and **without** the local token.
- **Immediate mitigation until A/B land:** change `config.go:919` default from `CredentialModePassthrough` to `CredentialModeKeychain` so the shipped default can authenticate.

---

### #5 — [HIGH] Telegram secret-lane redaction only covers `fs_write`/`fs_delete`; script/message tools leak full content

**Invariant violated:** 9 (secret-lane content is never sent to Telegram; at most a redacted title).
**Where:** `kahyad/internal/telegram/redact.go:41-68`, `approvals.go:128`, `render.go:77`.

**The defect.** `OnPendingApproval` gates every class through `isSecretLane` before sending to Telegram, but `isSecretLane` only inspects `fs_write`/`fs_delete` tool_input and returns `false` for every other tool (`default: return false`). `policy.yaml` classifies `shell_docker`, `shell_host`, `applescript_run`, `jxa_run`, `shortcuts_run`, `telegram_send` as W2 — and W2 pending approvals are delivered to Telegram as a full inline-keyboard card via `sendApprovalCard → renderPendingApprovalPayload → BuildOsascript(toolInput)`, which renders the **entire raw tool_input JSON verbatim**.

**Failure scenario.** The secret-lane **local** worker (Qwen3-30B — exactly where secret-lane content legitimately lives) emits an `applescript_run` or `shell_docker` call whose script body embeds a TCKN/IBAN/card/health value → `isSecretLane` hits the default branch → `false` → the full script bytes (with the secret) are rendered into a Telegram card and sent to `api.telegram.org`.

**Fix (detailed).** In `redact.go`, replace the `default: return false` branch with:
```go
default:
    if secretlane.ClassifyDeterministic(string(toolInput)).SecretLane { return true }
    return false
```
Because `renderPendingApprovalPayload` dumps the entire tool_input JSON for any non-fs tool, scanning `string(toolInput)` covers script/command/body fields regardless of field name, and a newly-added content-bearing tool is covered automatically (`secretlane` is already imported). Add `redact_test.go` cases per class (applescript_run with a valid-checksum TCKN, shell_docker echoing an IBAN, shell_host with a card number) asserting `isSecretLane==true` and that the redacted notice — not the bytes — is sent, plus a control case with no secret asserting the full card is still sent.

---

### #6 — [HIGH] Outbox never acks a done/failed task's row → permanent zombie re-claim loop

**Where:** `kahyad/internal/outbox/dispatcher.go:301,344-349`, `kahyad/internal/store/queries/queries.sql:589-598`, `kahyad/internal/task/resume.go:318`.

**The defect.** The never-redeliver guard in `processResume` marks the row delivered only for `user_halted`/`blocked_user` tasks. For a task already `done`/`failed` it falls through to `machine.Transition(→executing)`, which is **illegal** from a terminal state, so it ledgers `outbox.transition_failed` and returns **without** `markDelivered`. The row keeps `dispatched_at=NULL`; `ListDueOutboxRows` excludes only `user_halted`/canceled/delivered, **not** done/failed, so the row is re-claimed every lease period forever — each claim bumps `attempts` and appends two events to the anchored, nightly-backed-up ledger. Duplicate resume rows are readily produced because `enqueueResume` has no dedup: while the dispatcher is blocked in a long synchronous `spawn.Run` (the ≥10-min acceptance target), the 30s resume scan enqueues a fresh row per still-`executing` not-live task each tick; once such a task completes → `done`, every remaining duplicate becomes a done-zombie. (No double-execution — `IsLive` + reconciliation prevent that; the harm is unbounded resource/ledger growth.)

**Fix (detailed).**
1. Broaden the guard at `dispatcher.go:301` to also match `task.StatusDone` and `task.StatusFailed` — the existing body (ledger + `markDelivered` + return) is the correct handling.
2. In the transition-failed branch (`344-349`), after ledgering add `if errors.Is(err, task.ErrIllegalTransition) { d.markDelivered(ctx, row.ID) }` before return (covers the guard-check/transition race).
3. Root-cause hygiene: add `CountUndeliveredResumeOutboxRows` (SELECT COUNT over `task_id=? AND kind='task_resume' AND dispatched_at IS NULL AND canceled_at IS NULL`), regenerate sqlc, extend the `OutboxEnqueuer` interface, and in `enqueueResume` before the insert do `if n,_ := r.outbox.Count...(...); n > 0 { return nil }` so the 30s scan can't stack duplicates.
4. **Test:** seed a task in `done` with an undelivered `task_resume` row, run `ClaimAndDispatch` once, assert the row is delivered and a second pass claims 0 rows.

---

### #7 — [HIGH] Re-affirming a user-denied fact never recovers its confidence

**Where:** `kahyad/internal/factengine/engine.go:558-609`.

**The defect.** `recomputeConfidence` dedups **positive** evidence by **tier** (`"pos|"+weight`) but **negative** evidence by **session** (`"neg|"+session`). Every `user_asserted` positive row carries the identical weight (+2.9444), so all affirmations across all sessions collapse to a single +2.9444 contribution, while each independent-session denial adds a fresh −2.94. There is an upside clamp but no floor. Once a fact receives ≥2 same-tier denials (the normal suppression path), no later re-affirmation can raise it: `WriteFact` inserts a new positive row but `recomputeConfidence` collapses it into the existing pos-tier key so it contributes nothing, and `ConfirmFact` never touches confidence. This contradicts `DenyFact`'s own doc ("a denied-but-still-active fact can still be reasserted later and its confidence can recover").

**Failure scenario.** `user_asserted (+2.94)` → Deny(T1) → Deny(T2) → below threshold, excluded from injection → user re-affirms "Doğru" (T3) → confidence stays negative **forever**. A fact the user has explicitly re-affirmed is permanently non-injectable, recoverable only by DB surgery.

**Fix (detailed).** Rewrite `recomputeConfidence` to accumulate positive evidence across **distinct sessions** (enabling recovery) while negative-weight tiers still saturate (preserving the anti-downward property), then apply the existing upside clamp to the **final** confidence:
1. Dedup negative rows by `"neg|"+session` as today, summing into `negSum`.
2. Dedup positive rows by `(tierWeight, session)`: key `"pos|"+FormatFloat(weight)+"|"+sessionKey` where `sessionKey = SessionID` if valid/non-empty else `"row:"+id`; group survivors by tier weight.
3. Per tier: if `tierWeight >= 0`, contribution = `tierWeight * (distinct-session count for that tier)` (positive-cap tiers **accumulate** so re-affirmation offsets denials); if `tierWeight < 0` (agent_derived), contribution = `tierWeight` (saturates at one instance). Track `maxCap` = largest positive-tier weight present, `hasCap` = any positive row seen.
4. `confidence = posSum + negSum`; keep `if hasCap && confidence > maxCap { confidence = maxCap }`; **no** floor clamp. Do **not** clamp the positive subtotal before adding negatives (that re-breaks recovery).
Existing tests stay green (single external_doc + one denial = −1.5537; agent_derived×2 saturates at −0.4055). Add a regression test: `WriteFact(user_asserted, A)` → `Deny(B)` → `Deny(C)` (below threshold) → `WriteFact(user_asserted, D)` → assert `confidence == 2*CapLogOddsUserAsserted + 2*DenialLogOdds` and `InjectionEligible == true`.

---

### #8 — [MEDIUM] CONNECT tunnel volume budget is never enforced per-connection

**Invariant eroded:** 7 (every off-box byte passes an allowlist + volume budget).
**Where:** `kahyad/internal/egress/proxy.go:248,305-329,417`, `gate.go:298`.

**The defect.** For HTTPS (CONNECT), `handleConnect` calls `Gate.Check` with `nbytes=0` as a pre-dial admission check; the real byte total is metered only **after** the tunnel closes via `MeterUsage`. With `nbytes=0` the budget test reduces to `current > limit`, so a CONNECT is admitted whenever the host isn't **already** over budget, and the `io.Copy` loops have no running cap. A single tunnel to an allowlisted host can stream unbounded bytes past the 25 MiB/day default; only the *next* connection is blocked. The same unbounded copy exists on the plain-HTTP response body path. Combined with #2, this is an exfiltration channel.

**Fix (detailed).**
1. Add `Gate.RemainingBudget(ctx, host) (int64, limited bool, err error)` under the `mu` lock (fail-closed to 0 on canonicalize/store error; `limited=false` when `limit<=0`).
2. In `handleConnect` after the Allow check, call it; on err → `denyResponse`; if `limited && remaining<=0` → `DenyBudget` + `denyResponse`.
3. Replace the CONNECT `io.Copy` accumulator with a shared atomic counter that, when `limited && newTotal > remaining`, closes both client and upstream and breaks (closing both unblocks the peer goroutine so the WaitGroup completes); keep `MeterUsage` after `wg.Wait` and add an `EventBlockedBudget` ledger entry when the cap was hit. Check `total+n > cap` **before** `dst.Write` to tighten the one-buffer overshoot.
4. Add `Gate.DenyBudget` mirroring `DenyDNSRebind`.
5. Apply the same capped-copy to the plain-HTTP response body at `proxy.go:417`.
6. **Test:** small per-host budget, one CONNECT streaming past it → assert the tunnel is severed and an `EventBlockedBudget` row is written.

---

### #9 — [MEDIUM] 5-minute W1 undo collapses to "last write only" per (task, tool)

**Where:** `kahyad/internal/policy/engine.go:696,857-878`, `mcp/fs/server.go:539,593`, `mcp/fs/undo.go:64-77,117-183`.

**The defect.** `spawn.go` sets one `KAHYA_TRACE_ID` per task, so every tool call shares one trace_id. `openUndoWindow` is idempotent on `(task_id, tool, trace_id)` and reuses an already-open window with its deadline fixed at first-open (never refreshed). The fs pre-image registry is keyed by `traceID` **alone**, so a second `fs_write` in the same task overwrites the first write's undo record (and orphans its fallback copy). Net: two W1 writes share one window whose clock started at the first write, and only the most recent write is recoverable. Additionally, the non-binding `/policy/check` (`can_use_tool` early-reject) calls `Engine.Check`, which mints a real token **and** opens the undo window — execution-plane side effects on a query-plane endpoint.

**Fix (detailed).**
- **Deadline:** add `RefreshUndoWindowDeadline` (`UPDATE undo_windows SET deadline=? WHERE id=?`); in `openUndoWindow`, when an open row is found, UPDATE its deadline to `now+5m` and ledger `undo_window_refreshed`.
- **Per-write pre-image:** change the `mcp/fs/undo.go` registry to `map[string][]undoRecord`; `Put` appends; add `Pop(traceID)` (last element, delete key when empty) and `RemainingUndo`. `UndoWrite`/`UndoDelete` use `Pop` so each undo consumes one write most-recent-first. In `server/policy.go`'s undo dispatch, after a successful undo, if `RemainingUndo(traceID) > 0` re-open the window with a fresh deadline. Fix `PurgeExpired` to iterate **all** records for the expired trace.
- **Query/execution split:** add an advisory `Engine.CheckAdvisory` that runs the same ladder decision but does **not** mint a token or open a window, and wire `handlePolicyCheck` to it; `/v1/mcp` and the side-effect tools keep the token-minting `Check`.

---

### #10 — [MEDIUM] Incremental reindex freezes stale `source_tier` on content-unchanged files

**Invariant eroded:** §5 memory #1 (source-trust lattice; `user_edit` reachability).
**Where:** `kahyad/internal/indexer/indexer.go:459,476-479`.

**The defect.** `processFile` resolves the git-author-derived tier every run, but the unchanged-file fast path returns `fileUnchanged` whenever hash matches and status is active — never comparing or persisting the newly-resolved tier. Since `resolveUserEditTier` depends on git state (clean tree + author=`user`), not bytes, a file's authoritative tier can change while its hash does not. Common ordering: user edits `notlar.md` externally → reindex runs while the tree is dirty → stored `user_asserted`; nightly consolidation then commits the file as author=user with no byte change and post-reindexes → `processFile` now resolves `user_edit` but the hash is unchanged → returns `fileUnchanged` before update → tier frozen at `user_asserted` forever. The symmetric over-trust edge (a byte-preserving author rewrite keeping a stale `user_edit`) also exists.

**Fix (detailed).** Add tier equality to the skip predicate:
```go
if !full && !notFound && existing.Status == statusActive &&
   existing.SourceHash.Valid && existing.SourceHash.String == hash &&
   existing.SourceTier == tier {
    return fileUnchanged, 0, nil
}
```
`existing.SourceTier` is a non-nullable string already returned by `GetEpisodeBySourceAndPathRow`. A tier-only change now falls through to the update branch. (Optionally add a lightweight `UpdateEpisodeTier` query to skip re-chunking on a tier-only change.) Tests: (1) under-trust — dirty→`user_asserted`, then `git commit --author=user` with no byte change, `Reindex(full=false)` → assert `user_edit`; (2) over-trust — clean as user→`user_edit`, then `git commit --amend --author=kahyad` → assert downgraded.

---

### #11 — [MEDIUM] `sqlite-vec` pinned at 0.1.6, below the locked ≥0.1.9 floor; `coverage.md` falsely "covered"

**Where:** `go.mod:6`, `docs/coverage.md:27`, `kahyad/internal/store/db.go:176-179`.

**The defect.** `go.mod` pins `github.com/asg017/sqlite-vec-go-bindings v0.1.6`, which embeds the sqlite-vec C amalgamation at exactly v0.1.6. The locked design mandates ≥0.1.9 (`HANDOFF.md:88`). `coverage.md:27` marks "sqlite-vec ≥0.1.9 … covered" — a false compliance claim. `assertSQLiteFeatures` runs `SELECT vec_version()` and checks the row scans, but never compares to 0.1.9, so a below-floor extension passes the boot check silently. Not a crash (vec0 KNN works on 0.1.6) — a locked-requirement/pin-consistency violation plus a dishonest status.

**Fix (detailed).**
1. **Runtime floor:** in `db.go` add `minVecMajor/Minor/Patch = 0/1/9` and a `parseVecVersion` helper (trim `v`, SplitN on `.`, Atoi, wrap malformed as `ErrSQLiteFeatureMissing`). After scanning `vec_version()`, compare and return `fmt.Errorf("%w: vec_version()=%s, need >= 0.1.9", ...)` when below floor. Add a unit test.
2. **Reconcile the pin:** `go get github.com/asg017/sqlite-vec-go-bindings@<tag embedding ≥v0.1.9>`, then `go mod tidy && make build && make test`. If such a binding release exists, commit the bump (coverage.md:27 becomes true). If none exists upstream yet, do **not** add the hard runtime floor (it would fail closed at boot on the only available binding) — keep the newest available pin, set the constant to the achievable floor with a TODO, and change coverage.md:27 to "partial — binding pins 0.1.6; ≥0.1.9 blocked on upstream release." Either way, coverage.md must stop asserting an unmet floor as covered.

---

### #12 — [LOW] No wired path raises session taint from ingested content (invariant-6 defense-in-depth gap)

**Where:** `kahyad/internal/reader/reader.go`, `reader/actor_seed.go`, `mcp/fs/server.go:441-467`, `kahyad/internal/policy/engine.go:627`.

**The defect.** The taint ledger, the policy taint-gate, and the Reader/Actor split are individually correct, but `reader.Runner.Run` and `actor_seed.Spawn` have **zero** production callers, and the only production source of a `tainted` row is `briefing.InsertUntrusted`. `fs_read` never raises the session **taint** tier — on a secret hit it marks only the orthogonal egress-sensitive flag. So a clean worker session that `fs_read`s an untrusted non-secret file (e.g. a phishing note the user saved) is never tainted, and the taint-gate is a no-op. **Scope:** HANDOFF §6 W4 scopes the Reader/Actor split to web/mail inputs, and this build has no web-fetch or mail-read tool — the Reader has no callers because its inputs don't exist yet. The concrete harm is independently bounded (factengine source-tier quarantine, egress allowlist + post-sensitive-read block, `mail_send` is W3, shell Docker `--network none`), so this is a latent trap, not a live blocker.

**Fix (detailed).**
- **Part A (close the immediate gap):** in `mcp/fs/server.go` add a `SessionTaintRaiser` interface + field; in `HandleRead`'s `if secretLane` block (same request-`traceID` key `MarkSensitiveRead` uses) also call `TaintRaiser.RaiseTaint(ctx, traceID, "fs_read:secret_lane")`. Implement the adapter in `kahyad/internal/server` resolving the session via the same server-side resolver the policy gate uses (never a caller-supplied session_id); fail-closed if it errors. Wire via `mcpfs.New`.
- **Part B (prevent the latent bypass):** add a CI grep guard asserting no external-content tool is introduced without either a taint-raise or a route through `reader.Runner.Run` + `actor_seed.Spawn`. When a web-fetch/mail-read tool lands, route untrusted external bytes through the toolless Reader whose schema-validated struct seeds a fresh Actor.

---

### #13 — [LOW] Plain-HTTP request body double-counted against the egress budget

**Where:** `kahyad/internal/egress/proxy.go:356-365,419-423`, `gate.go:309-313`.

**The defect.** `handlePlainHTTP` passes `nbytes=r.ContentLength` into `Gate.Check`, which `Add()`s it to the budget, then **also** wraps the body in a counting reader and calls `MeterUsage(host, n+reqBytes)` after `RoundTrip`. For a request with a known Content-Length the body is added twice. Over-counts in the fail-safe direction (premature `egress_blocked_budget` denials, no security hole) — a 10 MiB POST consumes 20 MiB + response of the 25 MiB/day budget.

**Fix (detailed).** Make plain-HTTP match CONNECT: delete the `nbytes`/`ContentLength` computation and change the `Check` call to pass `0`. With `nbytes=0` the guard still denies when already over budget but adds nothing at admission, so `MeterUsage(host, n+reqBytes)` becomes the sole request-body count. Test: POST a known-size body through the proxy and assert the daily budget rose by exactly (request-body + response-body) bytes.

---

### #14 — [LOW] `shell_docker` verifies the image digest then runs by mutable tag (pin TOCTOU)

**Where:** `mcp/shell/runner.go:486-513,740`.

**The defect.** The pin resolves `docker image inspect --format {{.Id}} <ImageTag>` and refuses unless it equals `PinnedDigest` (fail-closed, good), but the subsequent `docker run` targets `spec.ImageTag` (the mutable tag), not the verified image ID. A tag re-point between check and run executes an unpinned image despite the pin passing. Only exploitable by an actor with local docker control, hence low.

**Fix (detailed).** After the digest check, `actualDigest` holds the verified image CONFIG ID. Add an `ImageRef` field to `dockerRunSpec`, set `ImageRef: actualDigest`, and in `buildDockerRunArgs` change `args = append(args, spec.ImageTag, ...)` to `spec.ImageRef` (`docker run sha256:<id> /bin/sh` is valid). Note the digest is a config ID, not a manifest digest, so the `tag@digest` form would not resolve for a local-only build — run by ID.

---

### #15 — [LOW] Truncated cloud calls are never billed to the cost governor

**Where:** `kahyad/internal/anthproxy/proxy.go:643-744`.

**The defect.** `usageCapturingBody` embeds `io.ReadCloser` but doesn't override `Close()`; its `onDone` (which releases the reservation **and** calls `RecordUsage`) fires only from inside `Read` on the first EOF/error. When the worker's connection is torn mid-stream (task timeout → `killGroup` SIGKILLs the worker's process group), `httputil.ReverseProxy` returns on the write error without a further `Read`, then `Close()`s the body — so `onDone` never runs. The reservation is released by the deferred `ReleaseReservation`, but the call's real cost is never recorded into daily/monthly totals, so repeated truncated calls let real Anthropic spend accumulate while `daily.usd` stays flat, exceeding the $10/day cap.

**Fix (detailed).** Add a `Close()` to `usageCapturingBody` that records usage once on early teardown:
```go
func (b *usageCapturingBody) Close() error {
    if !b.done {
        b.done = true
        if b.isSSE { b.onDone(b.sse.Usage(), io.ErrUnexpectedEOF) } else {
            u, _ := ParseNonStreamUsage(b.jsonBuf.Bytes()); b.onDone(u, io.ErrUnexpectedEOF)
        }
    }
    return b.ReadCloser.Close()
}
```
The `done` guard makes it idempotent with the Read path (exactly-once `RecordUsage`). Use `io.ErrUnexpectedEOF` so the call is marked `status="error"`. Test: wrap a body, read part, `Close()`, assert exactly one `RecordUsage` with `status="error"`.

---

### #16 — [LOW] `coverage.md` marks `docs/design.html` "covered" but the file is missing and W0-05 is blocked

**Where:** `docs/coverage.md:99`.

**The defect.** `coverage.md:99` lists `` `docs/design.html` committed | W0-05 | covered``. The file is absent and `BACKLOG.md:17` marks W0-05 `[!]` blocked. The requirement→task "covered" columns are unenforced prose (unlike the §5 invariant→test map, which `coverage_map_test.go` parses and runs), so they drift — same pattern as the false sqlite-vec row.

**Fix (detailed).**
1. Change `coverage.md:99` status to `GAP (blocked — W0-05 [!], user must export design.html)`.
2. Enforce file-deliverable claims: in `tests/invariants/coverage_map_test.go` add `TestCoverageMapCommittedFilesExist` — parse `coverage.md` for rows whose Status is `covered` and whose cells contain a backtick-quoted repo-relative path, and assert `os.Stat` succeeds for each, else `t.Errorf`. Any future "covered" row citing a missing committed file then goes red.

---

### #17 — [LOW] `make test` hard-fails container tests when Docker is up but the sandbox image was never built

**Where:** `Makefile:105-111`, `mcp/shell/container_test.go:26-121`.

**The defect.** The `test` target auto-enables `KAHYA_DOCKER_TESTS=1` whenever `docker info` succeeds and declares container tests "must PASS, never skip", but `test` doesn't depend on `sandbox-image`. On a box that never ran `make sandbox-image`, the tag `kahya-sandbox:0.1.0` doesn't exist, so `Runner.Run`'s `docker image inspect` fails the pin check → `t.Fatalf`. A dev with Docker running gets red container tests that read like a shell-sandbox regression. CI dodges it only because CI has no Docker.

**Fix (detailed).** Prefer (A) to preserve the "must PASS, never skip" contract: in the Makefile's docker branch of `test`, run `$(MAKE) sandbox-image` before the docker-enabled `go test`. Since `make sandbox-image` rewrites `IMAGE_DIGEST` from the freshly built (reproducible) image, the pin then matches. Alternative (B): in `requireDockerTests`, after the env check, `docker image inspect <liveImageTag>`; on error `t.Skipf("sandbox image not present — run make sandbox-image first")`. A is preferred. Either way, add a `docker/README.md` line: with Docker running, run `make sandbox-image` before `make test`.

---

## Suggested sequencing

1. **Unblock operation:** #4 (flip default to `keychain`, then implement passthrough properly) + provision W0-04 Keychain. Without this nothing runs.
2. **Close the two safety blockers:** #1 (control-socket split) and #2 (fs_read lane escalation) — these defeat the product's defining promises.
3. **High-severity exploits/data-loss:** #3, #5, #6, #7.
4. **Medium invariant erosion:** #8, #9, #10, #11.
5. **Low hygiene/hardening:** #12–#17 (batchable).

Each fix above is self-contained and includes a regression test, so they can be handed to an implementing agent one finding at a time.
