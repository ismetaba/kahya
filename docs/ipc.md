# Kâhya IPC sözleşmesi (W1–2, W12-07)

This document freezes the control-plane ↔ worker IPC contract HANDOFF §4 ⚑
requires to be fixed in W1–2. It is the deliverable itself ("IPC
sözleşmesi W1–2'de sabitlenir") — code must match this file, not the other
way around. Anything not explicitly covered here (real forward-proxy,
real Python worker, real policy engine/tokens, session resume) is out of
scope; see the "Out of scope / what changes later" section at the end.

Implementing packages: `kahyad/internal/spawn` (envelope + process
lifecycle), `kahyad/internal/policy` (interim static table),
`kahyad/internal/server` (`POST /v1/task`, `POST /policy/check`).

## 1 · Process model

kahyad spawns one worker process **per task** — never a long-lived worker
pool. The process is started as the leader of its **own new session**
(`setsid`, which also makes it the leader of a new process group: session
id = group id = the child's own pid) so a timeout, or the `⌥⎋` halt
(W6-03), can kill the whole tree — including any grandchild processes the
worker itself spawns — with a single `kill(-pgid, SIGKILL)`, and so kahyad
can attribute UDS callers to the worker by session (§5a "Caller
attribution": a session, unlike a process group, cannot be joined from
outside it).

The worker's stdin receives exactly one JSON object (the envelope, §2
below), after which **stdin is closed**. The worker's stdout is a JSONL
stream (§4 below), relayed live — not buffered until exit. The worker's
stderr is diagnostics only, logged by kahyad at `warn`; it is never shown
to the user and never affects task outcome.

kahyad has no timeout policy of its own inside the spawn layer — the
caller (the `/v1/task` handler) derives a `context.Context` with a
deadline of `cfg.task_timeout_min` minutes and passes it in. When that
context is done before the process exits on its own, kahyad SIGKILLs the
whole process group and waits for it to be fully reaped before continuing.
kahyad also SIGKILLs the group when the worker exits **on its own** (the
normal-exit path, not only on a timeout — G03 / B-14 robust review): the
worker's live-registry entry is dropped the instant `spawn.Run` returns, so
a descendant still in the group would otherwise outlive the task's
attribution and, with the secret, reach the control routes (§5a). No
in-group descendant survives a task, timed out or not; the only survivors
are ones that first leave the group (`setpgid(0, 0)`) or session
(`setsid`), which the sanctioned worker never does.

## 2 · Envelope v1 (worker stdin)

Single JSON object, written once, then stdin is closed:

```json
{
  "schema_version": 1,
  "task_id": "t_9f2c1a4b5e6d7f8091a2b3c4d5e6f708",
  "trace_id": "3f9a6b2c1d4e5f60718293a4b5c6d7e8",
  "session_id": null,
  "kind": "chat",
  "prompt": "Kadıköy'deki randevuyu hatırlat",
  "model": "claude-sonnet-5",
  "memory_injection": true,
  "created_at": "2026-07-10T12:00:00Z",
  "lane": "normal",
  "category": "",
  "resume": false,
  "intent": "chat",
  "deep_think": false
}
```

Field rules (`kahyad/internal/spawn.Envelope` + `Envelope.Validate`):

| Field | Rule |
|---|---|
| `schema_version` | Always `1` (`spawn.SchemaVersion`). Bump only with a documented, backward-compatible migration plan. |
| `task_id` | `"t_" + 32 lowercase hex chars` (`spawn.NewTaskID`, 16 random bytes). Minted by kahyad, never the client. |
| `trace_id` | 32 lowercase hex chars (`traceid.New`), or the client-supplied `trace_id` from the `/v1/task` request body/header if present. Propagated to the worker's environment (§3) and every ledger row for this task. |
| `session_id` | **Always present**, JSON `null` for a new task (Go: a nil `*string` — `encoding/json` renders this as `null` with no extra code). Reserved for W4-02 session resume; W1-2 never sets it on the way in. |
| `kind` | Always `"chat"` in W1-2. Other kinds (scheduled/background) are later work. |
| `prompt` | The user's text, non-blank (rejected at `/v1/task` before a task_id is even minted). Turkish text passes through byte-exact — no transliteration, no normalization. |
| `model` | Must be one of the HANDOFF §9 cloud set: `claude-opus-4-8`, `claude-sonnet-5`, `claude-haiku-4-5`, `claude-fable-5` (`spawn.AllowedModels`; worker mirror `ALLOWED_MODELS`). The HANDOFF §9 list ↔ `spawn.AllowedModels` ↔ worker `ALLOWED_MODELS` ↔ `router.Model*` ↔ the proxy pricing table are kept in lock-step by the drift guards `kahyad/internal/server` `TestModelIDsMatchHandoffSection9` and `worker/tests/test_envelope.py` `TestAllowedModelsHandoff` (G12-N2). **W4-08:** set by `kahyad/internal/router.SelectModel` (the §4 routing table, as data — see the "W4-08 note" below) rather than the static `cfg.default_model` W1-2 used; the routing **decision** is always Go's, never the prompt's. Set unconditionally even for a `lane:"secret"` task, or a task the router routed to the LOCAL lane (schema validity only — see the W4-08 note for why "local" is never itself a legal value here) — the ACTUAL enforcement that such a task never reaches this model is the mechanisms described below, not this field. |
| `memory_injection` | Always `true` in W1-2. The `<hafiza>` block itself is rendered by `/v1/memory/search?for_injection=true` (W12-05); the worker's `UserPromptSubmit` hook that actually calls it is W12-09. |
| `created_at` | Plain RFC3339 (`time.RFC3339`, e.g. `2026-07-10T12:00:00Z`, no fractional seconds), UTC. |
| `lane` | **W3-08.** `"secret"` \| `"normal"` \| `""` (omitted from JSON when empty via `omitempty` — `Envelope.Validate` treats empty identically to `"normal"`, so every pre-W3-08 envelope/test still validates unchanged). Set by `kahyad/internal/secretlane.ClassifyDeterministic` against the raw `prompt` **before** this envelope is even constructed — an IBAN/TCKN/card-number/CVV/finans-sağlık-kimlik-keyword hit is FINAL: no model consulted, no fallback to "normal". The worker never chooses or overrides this field; it only exists in the wire schema at all so a future worker-side integration can read it (see the "Worker-side wiring status" note below — today the worker is never even spawned for a `"secret"` task, see §4 below). |
| `category` | **W3-08.** `"finans"` \| `"saglik"` \| `"kimlik"` \| `""` (omitted via `omitempty`). Set alongside `lane` when a deterministic hit fires; informational only (CLI badge, Telegram redaction, logs) — never itself a security boundary. |
| `resume` | **W4-02.** `true` \| `false` (omitted via `omitempty` when `false` — every pre-W4-02 envelope/test still validates unchanged). Set only by `kahyad/internal/outbox.Dispatcher` when re-spawning a worker for an already-`session_id`-bearing task (`Envelope.Validate` requires a non-blank `session_id` whenever this is `true`). The worker (`kahya_worker.__main__._build_options`) reads this to construct `ClaudeAgentOptions(resume=session_id, ...)` instead of starting a fresh conversation — streaming input mode stays in force either way (hooks/`can_use_tool` still need it). |
| `intent` | **W4-08.** The §4 routing-table intent bucket (`kahyad/internal/router`'s `Intent*` constants: `plan`, `code_multi_file`, `hard_exec`, `subagent_exec`, `fanout`, `extract`, `writeback`, `route`, `classify`, `reader`, `chat`) — omitted via `omitempty` when empty, so every pre-W4-08 envelope/test still validates unchanged. Informational only (logs/CLI/ledger `intent_classified`/`routing_decision` events) — no enum validation in `Envelope.Validate` (the routing DECISION already happened before this envelope was built; the worker never reads this to choose anything, §4: "karar Go kodunda, istemde değil"). An ordinary `/v1/task` chat prompt with no CLI-declared kind is always `"chat"`, decided deterministically (no model call) — see the W4-08 note below for why. |
| `deep_think` | **W4-08.** `true` \| `false` (omitted via `omitempty` when `false`). The "derin düşün" opt-in: set by `kahya ask --derin`, or detected server-side (POST /v1/task) from the byte-exact Turkish prompt prefix `"derin düşün:"` (stripped from `prompt` before this envelope is built — never model-detected). `kahyad/internal/router.SelectModel` pins `model` to `claude-fable-5` whenever this is `true`, UNLESS `lane=="secret"` (which outranks it unconditionally). This field carries no enforcement of its own — it is only the audit trail of why `model` ended up being `claude-fable-5`. |
| `mode` | **W4-03/W6-02.** `""` (ordinary Actor/chat, every pre-W4-03 envelope) \| `"reader"` (toolless cloud-Haiku Reader, `spawn.ModeReader` — requires `schema` non-blank) \| `"stt"` (toolless, CLOUD-LESS local transcription, `spawn.ModeSTT` — requires `input_audio_path` non-blank). See the "W6-02 note" below for the `"stt"` case. |
| `schema` | **W4-03.** Non-blank only when `mode=="reader"` — names the registered Go-side struct (`kahyad/internal/reader.JobTypeMailSummary`/`JobTypeWebpageExtract`) the caller parses that session's single JSON response into. |
| `input_audio_path` | **W6-02.** Absolute path to a mono 16kHz wav. Non-blank only when `mode=="stt"` (`Envelope.Validate` enforces this) — see the "W6-02 note" below. |

## 3 · Worker environment

In addition to inheriting kahyad's own process environment (`PATH`, `HOME`,
etc. — W1-2's worker is a plain subprocess, not a network-isolated
container), kahyad sets these variables (`spawn.BuildEnv`; the two B-1
MCP timeouts only when an approval wait is configured):

| Variable | Value |
|---|---|
| `KAHYA_TASK_ID` | The envelope's `task_id`. |
| `KAHYA_TRACE_ID` | The envelope's `trace_id`. |
| `KAHYA_SOCKET` | `cfg.socket` — kahyad's own control socket, so the worker's `can_use_tool` hook (W12-09) can reach `POST /policy/check` over HTTP-over-UDS. |
| `KAHYA_LOG_DIR` | `cfg.log_dir` — the worker writes its own JSONL logs here (every process logs JSONL with `trace_id` on every line, HANDOFF §4 ⚑). |
| `ANTHROPIC_BASE_URL` | Since W12-08: `http://127.0.0.1:<ephemeral-port>` — the per-task `kahyad/internal/anthproxy.Proxy` listener kahyad opens (`127.0.0.1:0`) immediately before spawning this worker and closes immediately after it exits. The cost governor, cache-hit metric, and (since W3-05) the `egressGate` hook — now a real `kahyad/internal/egress.Gate.Check` call, gating `host=<AnthropicUpstreamURL's own host:port>` — are enforced at that proxy, never in this package. |
| `ANTHROPIC_CUSTOM_HEADERS` | Since FIX-I-1, whenever the spawn has a per-task proxy: exactly one line, `X-Kahya-Task-Token: kahya-task-<hex32>` — the **per-task random token** (`spawn.NewAPIKey`), **NOT a real Anthropic key** (HANDOFF §4 IPC ⚑: "API anahtarı worker'a verilmez"). The bundled CLI adds it to every model call; the per-task forward-proxy listener validates this header first (constant-time; `401` + ledger `proxy_auth_reject` on a mismatch, so no other local process can spend through kahyad by guessing the port) and strips it in both credential modes — it never reaches the upstream. An inherited value is stripped (`spawn.secretEnvDenylist`). |
| `ANTHROPIC_API_KEY` | Only in `credential_mode: keychain` (and the legacy empty mode): the same per-task token, the W12-08 carrier (the CLI sends it as `x-api-key`; the proxy still accepts it, strips it, and injects the real Keychain key). **Unset in `passthrough`** (FIX-I-1): the CLI picks exactly one credential source and a set `ANTHROPIC_API_KEY` always wins over its own Claude Code login, so any value there would disable the credential passthrough forwards. An inherited value is always stripped. |
| `ANTHROPIC_SMALL_FAST_MODEL`, `ANTHROPIC_DEFAULT_HAIKU_MODEL` | Since FIX-I-10: both `spawn.CLIAuxiliaryModel` (= `router.ModelHaiku`, a HANDOFF §9 id) — pins the bundled CLI's own auxiliary calls (summaries, titles, compaction) to an allowlisted, priced model instead of the CLI's built-in dated Haiku id. Inherited values of these and of `ANTHROPIC_MODEL` / `ANTHROPIC_DEFAULT_{SONNET,OPUS,FABLE}_MODEL` are stripped (`spawn.secretEnvDenylist`): model choice is Go's, never the parent shell's. |
| `KAHYA_MCP_BRIDGE` | Since W12-09: `cfg.mcp_bridge_path` — the absolute path to the `kahya-mcp` stdio↔UDS bridge binary (`bin/kahya-mcp`, W12-05). The worker execs this as its `"kahya_memory"` MCP server's `stdio` `command` (`ClaudeAgentOptions.mcp_servers`) — it never hardcodes or otherwise discovers this path itself. |
| `KAHYA_CREDENTIAL_MODE` | Since W12-09: `cfg.credential_mode` (`"keychain"` or `"passthrough"`, mirroring `anthproxy`'s own `CredentialMode` — see the W12-08 note below). The worker reads this to decide which startup env assertions apply (`kahya_worker.__main__._check_credential_env`, exit 2 + `worker_env_invalid` on a violation): in `"passthrough"`, `ANTHROPIC_CUSTOM_HEADERS` must carry the `X-Kahya-Task-Token: kahya-task-<hex32>` line **and** `ANTHROPIC_API_KEY` must be unset (FIX-I-1); in `"keychain"` (and any other value), `ANTHROPIC_API_KEY` must match the per-task token shape. Defaults to `"keychain"` if unset (belt-and-braces: the stricter posture is default-safe). |
| `KAHYA_MCP_TOOLCALL_TIMEOUT_S` | Since B-1, only when `approval_wait_seconds > 0`: `approval_wait_seconds + 60` — the `kahya-mcp` bridge's per-`tools/call` budget, so a call kahyad parks for a human approval (§6a) is not reported as "kahyad unreachable" after the bridge's ordinary 4 s. Every other JSON-RPC method keeps the 4 s budget, and the dial is always bounded by it. The worker also passes it explicitly in the `kahya_memory` stdio server's own `env` (`_build_options`). |
| `MCP_TOOL_TIMEOUT` | Since B-1, only when `approval_wait_seconds > 0`: `(approval_wait_seconds + 90) * 1000` (ms) — the bundled `claude` CLI's own hard per-MCP-call limit, set explicitly because its default is unknown. Set after the inherited environment, so a parent shell's value never shortens it. |
| `KAHYA_TMP_DIR` | Since W6-02: `config.Config.TmpDir()` (`<data_dir>/tmp`, e.g. `~/Library/Application Support/Kahya/tmp` — created 0700 at kahyad startup). `mode=="stt"` sessions read this to decide whether `input_audio_path` is safe to delete after transcription (`kahya_worker.__main__._maybe_delete_audio`): a file is deleted **iff** its resolved parent directory equals this exact value — never a repo fixture, never any other path. Empty/unset is a fail-CLOSED no-op (never delete). |

`KAHYA_SOCKET` alone reaches only the four **worker routes** (see §5a);
every control route (`/policy/promote`, `/policy/feedback`,
`/approvals/{id}/decision`, `/halt`, `/v1/task`, ...) additionally requires
kahyad's per-boot control secret (`<socket>.secret`), which is never in the
worker env (`spawn.secretEnvDenylist` strips `KAHYA_CONTROL_SECRET` /
`KAHYA_CONTROL_SECRET_PATH` defensively; neither exists today).

### W12-08 note — OWNER AUTH DECISION (HANDOFF deviation)

HANDOFF §4 assumes kahyad reads a real Anthropic API key from the macOS
Keychain and injects it into every proxied request. The owner decided NOT
to provision a separate Anthropic API key for this project: the worker
(`claude-agent-sdk`) instead authenticates through its own, already
logged-in Claude Code SDK session. `kahyad/internal/anthproxy` implements
BOTH modes behind one `CredentialSource` interface, selected by the new
`credential_mode` config key:

- `keychain` — the original HANDOFF design, fully implemented and tested
  as a valid fallback: strip every inbound auth header, inject the real
  key read from `kahyad/internal/secrets.Keychain` (never logged).
- `passthrough` (**default**, the owner-decision mode; re-confirmed by the
  owner for FIX-I-1, decisions G11-1/G11-2) — after validating the inbound
  per-task local token (`X-Kahya-Task-Token` above — this still fully
  upholds "API anahtarı worker'a verilmez": kahyad hands the worker no real
  Anthropic credential in either mode), strip the local token and forward
  the worker CLI's own `Authorization` header (its Claude Code login)
  completely unchanged — the proxy never inspects, replaces, or logs it.

**FIX-I-1 — how the local token travels (and why the original passthrough
never authenticated).** The bundled `claude` CLI picks exactly ONE
upstream credential source; in its non-interactive (SDK) mode a set
`ANTHROPIC_API_KEY` always wins over its Claude Code login. The original
design put the local token in `ANTHROPIC_API_KEY` and assumed the CLI would
add its login credential in a second header; it never does, so the proxy
stripped the token and every passthrough call left with **no credential at
all** (an upstream `401 authentication_error`, task `failed`). Now:

- `spawn.BuildEnv` always sets `ANTHROPIC_CUSTOM_HEADERS=X-Kahya-Task-Token:
  <token>` (the CLI's documented custom-header variable) and sets
  `ANTHROPIC_API_KEY=<token>` **only in keychain mode**. In passthrough the
  CLI therefore authenticates with its own login and sends
  `Authorization: Bearer <oauth>` (+ the `oauth-2025-04-20` beta) itself.
- `anthproxy.checkLocalAuth` checks `X-Kahya-Task-Token` first (when present
  it alone decides), then the legacy `x-api-key`/`Authorization` carriers
  (keychain mode, the W12-08 manual curl check); comparisons are
  constant-time. The Director strips the header in both modes.
- Passthrough refuses, locally, a request that would leave with no
  upstream credential once the token is stripped (`401` with
  `"Bulut kimlik bilgisi yok - worker'in Claude Code oturumu bulunamadi
  (claude /login) ya da credential_mode: keychain secin."`, ledger
  `proxy.no_upstream_credential` and `OnNonRetryableFailure(reason
  no_upstream_credential)` exactly once per proxy → task `failed` with
  `Bulut çağrısı kalıcı hatayla reddedildi (no_upstream_credential). Görev
  durduruldu.`). Only the header's presence is checked. The egress gate
  (secret-lane backstop) still runs first, so its `403` always wins.
- `CLAUDE_CODE_OAUTH_TOKEN` is deliberately **not** stripped from the
  worker env by kahyad (owner decision G11-1/G11-2; G00 H-2(b) not
  applied). It is **not** a working way to give the worker a real login,
  though: the worker's belt-and-braces leak check (`_check_real_key_leak`,
  "API anahtarı worker'a verilmez") refuses to start, in every credential
  mode, when any env value contains `sk-ant-` — and every real Claude Code
  OAuth token does (`sk-ant-oat01-…`, from `claude setup-token`). Such a
  task exits 2 before any SDK client exists, with one `worker.jsonl` line
  `{"event":"real_key_in_env","var":"CLAUDE_CODE_OAUTH_TOKEN"}` (the value
  is never logged). **The passthrough credential is the worker CLI's
  stored `claude /login` session** (macOS Keychain); do not export
  `CLAUDE_CODE_OAUTH_TOKEN` into kahyad's environment (launchd plist or
  shell). Only non-secret values pass the check — the hermetic e2e seam
  `CLAUDE_CODE_OAUTH_TOKEN=hermetic-dummy` below. Pinned by
  `worker/tests/test_main.py`
  `test_passthrough_refuses_real_claude_code_oauth_token_in_env` /
  `test_passthrough_accepts_non_secret_oauth_seam_value`. (Accepting a
  real `sk-ant-oat` token here would loosen the leak check and needs its
  own owner decision.)

Hermetic header-capture probe (handbook G11 I-1 Repro A, 2026-10-14,
pinned `claude-agent-sdk==0.2.111` → bundled CLI **2.1.202**; fresh
`HOME`/`CLAUDE_CONFIG_DIR`, `ANTHROPIC_BASE_URL` = a local 401-ing
listener, outbound network denied except loopback): `grep -a -c` finds
`ANTHROPIC_CUSTOM_HEADERS` 23× and `CLAUDE_CODE_OAUTH_TOKEN` 55× in the
binary. With `ANTHROPIC_API_KEY=kahya-task-…` the CLI sent **only**
`x-api-key: kahya-task-…` (no `Authorization`) — the bug. With
`ANTHROPIC_CUSTOM_HEADERS` + `CLAUDE_CODE_OAUTH_TOKEN=hermetic-dummy` and no
API key it sent `x-kahya-task-token: kahya-task-…` + `authorization: Bearer
<dummy>` and no `x-api-key`; every request went to `POST
/v1/messages?beta=true`. `tests/e2e` `TestW12PassthroughCloudAuth` runs that
exact passthrough shape end to end (real kahyad + worker + CLI, mock
upstream, the same `CLAUDE_CODE_OAUTH_TOKEN` seam); the keychain gate
`TestW12Acceptance` still passes unchanged.

Everything else this task specifies (cost governor, cache-hit metric,
egress-gate hook, usage/pricing) is auth-agnostic and built exactly per
the frozen contract above; only credential injection differs by mode. The
one deferred item is a **live** check with the real Claude Code login
(passthrough) or a real Keychain key — `make accept-w12`'s `cloud_auth`
criterion (`model_call status=ok` for the live task's trace; a
`task.failed` row is a FAIL) — see the W12-08 task file for the deferral
note.

**FIX-I-10 — model + path allowlist, never $0 pricing.** The worker's
CLI writes the proxied body, so the proxy re-checks it in Go
(`ServeHTTP` order, G00 C-8: local auth 401 → path allowlist 404 → egress
gate 403 → passthrough credential presence 401 → body → Fable-5 shaping →
model allowlist 400 → governor 503 → keychain 503 → upstream):

- **Paths:** only `/v1/messages` and `/v1/messages/count_tokens` (the URL
  path; the CLI's `?beta=true` query is fine). Anything else (files,
  batches, model listing, admin) is `404` with `"Izin verilmeyen uc nokta
  - istek reddedildi."` and ledger `proxy.path_rejected` `{task_id, path,
  method}`; widened only after a live `proxy.path_rejected` is reviewed
  (owner decision G11-4).
- **Models:** the body's `model`, after resolving a dated CLI snapshot id
  to its HANDOFF id (`anthproxy.CanonicalModel`; today
  `claude-haiku-4-5-20251001` → `claude-haiku-4-5`, owner decision G11-3
  (i); the body is forwarded unchanged), must be in `spawn.AllowedModels`
  (`ProxyConfig.AllowedModels`; nil = `anthproxy.DefaultAllowedModels()`,
  exactly the priced set — `TestSpawnAllowedModelsMatchProxyDefault` pins
  the two equal, so a model-id bump is data-only). Otherwise `400` with
  `"Izin verilmeyen model kimligi - istek reddedildi."` and ledger
  `proxy.model_rejected` `{task_id, model}`, before any budget
  reservation. A rejection does not fail the task by itself.
- **One exact-key body parser (review fix):** every body-derived decision
  (model allowlist + pricing, Fable-5 shaping, the governor's
  `max_tokens` estimate) reads the top-level object through
  `anthproxy.topLevelFields`: exact, case-sensitive member names, as the
  upstream reads them. A body that is not exactly one well-formed object,
  or whose top-level member names collide under encoding/json's case
  folding (`"model"` twice, `"model"` + `"MODEL"`, a lone `"Model"`, or any
  other colliding pair such as `"max_tokens"` + `"MAX_TOKENS"`), yields no
  model and gets the same `400` + `proxy.model_rejected` (`model: ""`).
  Without this, encoding/json's case-insensitive, last-match-wins struct
  decoding let a body be allowlisted and priced as one model while the
  upstream ran another (`TestProxyRejectsCaseVariantOrDuplicateModelKey`).
- **Pricing:** a forwarded call whose model has no pricing row is billed
  at `anthproxy.MaxPriceRow` (the costliest row valid now) with ledger
  `proxy.price_unknown` `{task_id, model, billed_as}`, and the governor's
  pre-forward estimate uses the same rate — a call is never billed $0.
- **CLI auxiliary model:** `ANTHROPIC_SMALL_FAST_MODEL` /
  `ANTHROPIC_DEFAULT_HAIKU_MODEL` = `spawn.CLIAuxiliaryModel` (env table
  above). The hermetic gate's `no_model_rejected` subtests
  (`TestW12Acceptance`, `TestW12PassthroughCloudAuth`) fail on any of the
  three ledger kinds; with the pinned CLI they see only the main
  `POST /v1/messages` call.

**FIX-I-11 — a client that disconnects mid-stream is still billed.** A
client that disconnects mid-stream still produces a `model_call` row
(`status=aborted`) with the usage observed so far (a lower bound: the
`message_delta` carrying `output_tokens` may not have arrived). Two shapes
reach it: `httputil.ReverseProxy` stops on a failed client write and
*closes* the upstream body (`usageCapturingBody.Close` records with
`errClientDisconnected`, flushing a trailing partial SSE line), or the
upstream read is cut by the worker's cancelled inbound context. Exactly
one row per call either way (a clean EOF still records `ok` once; the
later `Close` is a no-op); the governor's reservation is still released by
`ServeHTTP`'s defer. The stream is never drained on the worker's behalf.

The usage parser reads the reply as plain JSON/SSE, so the proxy's Director
replaces the worker's `Accept-Encoding` (the pinned CLI sends
`gzip, deflate, br, zstd`) with `identity` in both credential modes: a
compressed reply would otherwise be ledgered as `model_call` with zero
tokens and `usd=0` (`TestProxyBillsRealUsageWhenWorkerAcceptsCompression`;
the hermetic gate's `upstream_identity_encoding` subtests pin it for the
real CLI). A successful (`< 400`) reply that still arrives with a
non-identity `Content-Encoding` is forwarded unchanged but billed
fail-closed at the governor's reservation estimate
(`Governor.FailClosedUsage`: body bytes / 3 as 1h cache-write tokens +
`max_tokens` as output tokens, or `est_request_tokens` at the output rate)
and ledgered `proxy.encoded_body`
`{task_id, model, content_encoding, billed_usd}`. An error status carries
no usage and keeps `usd=0`.

**Post-review addition (BLOCKER 2 fix)** — `Governor.CheckBeforeForward`
is now an atomic check-and-reserve, not a plain check-then-act: it
reserves a conservative (fail-closed = over-, never under-estimated)
token/USD estimate for the about-to-be-forwarded request before it is
sent, and `RecordUsage` releases that reservation once the real usage is
known. When the request's own `max_tokens`/body size can't be parsed, the
estimate falls back to the new `est_request_tokens` config key (committed
default `50000`) — see `kahyad/internal/anthproxy/governor.go`'s
`estimateRequestLocked` for the full estimation strategy.

### W3-08 note — secret-lane branch (HANDOFF §4 ⚑ ordering invariant)

`kahyad/internal/secretlane` + `kahyad/internal/mlx` add the Go routing
branch that makes finans/sağlık/kimlik content physically unable to reach
a cloud model, without changing anything in §2-§6 above:

- **Classification is deterministic-first, ingest-time, and precedes
  everything.** `POST /v1/task`'s handler runs
  `secretlane.ClassifyDeterministic(prompt)` — IBAN/TCKN (checksum-
  verified)/card-number (Luhn-verified)/CVV regexes, plus a folded
  (`kahyad/internal/textnorm.Fold`) sağlık/finans/kimlik keyword lexicon
  matched at word starts only (prefix entries tolerate Turkish suffixes,
  `eft`/`tckn` are whole-word; FIX-B-2) —
  **before** the envelope above is even constructed, i.e. before any
  worker could be spawned or any Anthropic-proxy listener opened. A hit is
  final: `lane="secret"`, no model consulted. This call takes **no live
  Qwen dependency at all** — deliberately narrower than the full
  `secretlane.Classifier` (deterministic pre-pass + Qwen3-30B-A3B
  fallback, `kahyad/internal/mlx.Supervisor`-backed, fully implemented and
  unit/live-tested) that `kahyad/internal/secretlane.Escalate` exists for
  at the memory_write / mail-web-Reader ingestion points — see that
  package's own doc comments for the full rationale. `fs_read` is an
  ingestion point too and escalates deterministically since FIX-B-6 (see
  "Mid-task escalation" below).
- **A `lane:"secret"` task is answered ENTIRELY on-device — the worker is
  never spawned at all.** `kahyad/internal/server`'s `handleSecretLaneTask`
  calls `secretlane.Answerer` (backed by the same local Qwen3-30B-A3B
  server, an OpenAI-compatible `mlx_lm.server` kahyad supervises via
  `kahyad/internal/mlxsup`, reused verbatim — see `mlx/qwen/README.md`)
  directly, streaming its answer back over the identical SSE contract §5
  documents (one `delta` event, then `result` with
  `"processed_locally": true` — the `kahya` CLI prints `🔒 yerel işlendi`
  on this). There is no code path through which a secret-lane task's
  content could reach the worker process, let alone the Anthropic proxy —
  not merely a backstop that blocks it after the fact.
- **Worker-side wiring status:** the `lane`/`category` envelope fields
  above exist in the wire schema for a FUTURE worker-side integration, but
  today's worker (`claude-agent-sdk`) speaks the Anthropic Messages API,
  not `mlx_lm.server`'s OpenAI-compatible endpoint — rewiring the SDK
  itself to a local endpoint is out of reach for W3-08 (its own explicit
  escape hatch). Since the worker is never spawned for a secret-lane task
  at all (previous bullet), this is not a live gap today; it only matters
  if some future change ever routes a secret-lane task through the
  worker, which is exactly what layer 2 below exists for.
- **Layer 2 — the W12-08 proxy backstop (`secretlane.NewProxyBackstopHook`,
  composed with `egress.NewAnthproxyEgressGateHook` in `main.go`):** even
  if a secret-lane task somehow did reach a worker/proxy, the per-task
  `anthproxy.Proxy`'s `EgressGate` hook consults the task's DURABLY
  persisted `tasks.lane` row (by `task_id`, fixed per listener) on every
  single forwarded request and refuses with `403` + ledger
  `secretlane_cloud_blocked` — this is the enforcement even if a
  prompt-injected worker tries the cloud URL directly, since the worker
  never gets to choose or override its own task's lane.
- **Sticky, persisted lane:** `tasks.lane`/`tasks.secret_category`
  (`kahyad/migrations/0006_secret_lane.sql`) are set on `InsertTask` (the
  classification above already ran) and only ever WIDEN thereafter
  (`secretlane.Escalate` / `secretlane.EscalateVerdict`, enforced in Go,
  not SQL) — once a task is `lane="secret"` it stays secret for its
  lifetime, including a W4-02 resume. A lane READ error during an
  escalation is returned and nothing is written (FIX-B-6: it used to count
  as "no row yet" and could write `normal` over a secret row).
- **Mid-task escalation at `fs_read` (FIX-B-6).** A NORMAL-lane task whose
  worker reads a file that is secret-lane by path glob OR by content
  (`secretlane.ClassifyDeterministic`, no Qwen) is escalated before a
  single byte is returned, in this fixed order: `secret_lane_read` ledger
  row (`task_id`, `category`), durable sensitive-read mark (FIX-B-7),
  sticky `tasks.lane='secret'` via `mcp/fs.LaneEscalator` →
  `kahyad/internal/server.NewFSLaneEscalator` (ledger
  `secret_lane_escalated{task_id, lane, category, source:"fs_read",
  attribution}`). Either persistence failure refuses the read
  (`mcp/fs.MsgSecretLaneReadRefused`, ledger `secret_lane_read_refused`
  with `reason` `sensitive_mark_failed` / `lane_escalate_failed`). The
  escalated task is the CALLER's: for a live worker it comes from the UDS
  peer attribution (G03 / B-14; `attribution:"live_worker"`), never from
  the `X-Kahya-Task-Id` it declared; only an unattributed, control-secret
  caller (CLI/script/test harness) keeps its declared id
  (`attribution:"declared"`), and one with no task id or an unknown one
  is ledgered `secret_lane_escalate_no_task` (it has no task proxy to cut
  off). A live worker whose tasks row is missing fails closed. The worker
  does hold the bytes after a successful read (owner decision G02-2: the
  task is escalated, not refused); every later model call of the task is
  then refused by layer 2 below — `403 permission_error` with
  `MsgSecretLaneCloudBlocked`, one `secretlane_cloud_blocked` row per
  refused call — and the FIRST secret-lane refusal of a task raises one
  Turkish notice,
  `"Görev gizli-şerit (finans/sağlık/kimlik) içeriğe dokundu; bulut modeline devam edilemez."`
  (ledger + JSONL kind
  `secretlane_cloud_blocked_notice`; once per task per kahyad process,
  `secretlane.CloudBlockedNotices`). A lane lookup ERROR at the backstop
  still refuses with the same 403 body but is a distinct sentinel
  (`secretlane.ErrCloudBlockedLookupFailed`, not `ErrCloudBlocked`) and
  raises no notice. Known limit until G10 (G00 U-4): a refused task is
  not yet ended terminally — the real worker exits non-zero on the 403, the
  task is left non-terminal, and a resume respawns it; every respawn is
  refused the same way (zero bytes upstream) and the notice is still
  raised only once per kahyad process.
- **Local model supervision** (`kahyad/internal/mlx.Supervisor`, reusing
  `kahyad/internal/mlxsup`'s generic spawn/health-poll engine verbatim):
  fail-closed free-memory check (`kahyad/internal/mlx.HasSufficientMemory`,
  ~17GB model + 4GB headroom) before every spawn attempt (FIX-I-9: wired
  as `mlxsup.Config.BeforeSpawn`, so it gates crash-restarts too and never
  a call to an already-warm server; a refused spawn starts nothing and
  leaves the supervisor retryable by the next request); on-demand load
  (first secret-lane need, never at boot); idle-TTL unload
  (`mlx.qwen_idle_ttl_seconds`, default 600s, zero in-flight requests) via
  `mlxsup.Supervisor.StopForIdle` (a NEW primitive alongside `Stop` —
  unlike kahyad-shutdown's `Stop`, `StopForIdle` leaves the supervisor
  eligible for a later on-demand respawn); crash → respawn with backoff,
  capped at `MaxRestarts` (default 3), then the terminal `StateFailed`
  (also a new `mlxsup` state, opt-in via `Config.MaxRestarts`, `0` = the
  original W12-11 unlimited-retry behavior, unaffected). ANY failure mode
  here (insufficient memory, spawn/health failure, `MaxRestarts` exceeded)
  collapses to the ONE typed `mlx.ErrLocalModelUnavailable` sentinel —
  Turkish, byte-exact: `"yerel model için bellek yok"` + guidance
  `"ComfyUI/Wan kapatıp tekrar deneyin"` — the task's SSE stream ends in
  `error` with this message; it is NEVER rerouted to cloud.
- **Live-verified (`KAHYA_MLX_TESTS=1`, `kahyad/internal/mlx/live_test.go`):**
  a real spawn of the real ~16GB Qwen3-30B-A3B-4bit checkpoint became
  healthy in ~1.2s on this dev machine's NVMe storage (well under the 120s
  default `StartupGrace`); a warm classification round-trip measured
  ~280-1200ms — the task spec's own "<300ms warm" target does not hold in
  practice for this model size over HTTP on this hardware (a documented
  tuning finding for W4-08's intent router, which owns the combined
  routing+classification path going forward, not a correctness bug: every
  fail-closed/ordering-invariant/proxy-backstop guarantee holds regardless
  of how long classification actually takes). `mlx_lm.server`'s Qwen3 chat
  template defaults to an extended "thinking" trace before answering — the
  classifier and local answerer both pass
  `chat_template_kwargs:{"enable_thinking":false}` to get a direct answer.

### W4-08 note — intent router (HANDOFF §4 model-routing table)

`kahyad/internal/router` turns the §4 model-routing table into executable
Go data plus one pure function, `SelectModel(RouteInput) RouteDecision`,
that `POST /v1/task`'s handler consults to set `model`/`intent`/
`deep_think` above — replacing W1-2's static `cfg.default_model` with the
real table:

- **Table rows** (`plan`/`code_multi_file`/`hard_exec` → `claude-opus-4-8`;
  `subagent_exec`/`fanout` → `claude-sonnet-5`; `extract`/`writeback` →
  `claude-haiku-4-5`; `route`/`classify` → the local Qwen classifier itself,
  never a cloud model; `reader` → lane-dependent, matching W4-03's Okuyucu
  row unchanged; `chat`/unrecognized → `cfg.default_model`) are `SelectModel`'s
  own ordinary case, driven by `RouteInput.Intent`.
- **Ordering invariant, unchanged and reaffirmed:** `RouteInput.Lane` (the
  W3-08 classification already described above) is checked FIRST, before
  anything else — `lane=="secret"` PINS the local lane for every intent,
  outranking `deep_think`/downgrade alike. `POST /v1/task`'s ordinary chat
  path only ever passes `Intent:"chat"`, decided deterministically (no
  model call) — the combined Qwen `{secret_lane, category, intent}` call
  this extends (see the W3-08 note above; the JSON schema now also returns
  `intent`) is invoked only where W3-08 already invokes Qwen (the W4-03
  Reader ingest path) or where a caller explicitly asks to classify
  (`kahyad/internal/router.ClassifyIntent`) — **never** on every ordinary
  chat message.
- **`intent_classified` / `routing_decision` ledger events** are emitted
  for every task (`kahyad/internal/router.LogIntentClassified`/
  `LogRoutingDecision`), in that order, before the eventual `model_call`
  event a real cloud call produces — `kahya log --trace <id>` shows all
  three, in order, under one `trace_id`.
- **LOCAL LANE IS NOT AN ENVELOPE MODEL.** When `SelectModel` returns
  `Local:true` (a secret-lane pin, OR the cost-governor's Sonnet→yerel
  downgrade rung below), `POST /v1/task`'s handler answers the task via the
  SAME local-answer mechanism the W3-08 note above describes
  (`kahyad/internal/secretlane.Answerer`, `handleSecretLaneTask`) — it
  NEVER spawns a worker or opens an Anthropic-proxy listener for that task,
  and `model` in the envelope/tasks-row stays `cfg.default_model` for
  schema validity only (never itself the enforcement).
- **"Derin düşün" opt-in** pins `claude-fable-5` — the ONLY way it is ever
  selected. Two independent triggers, both setting the SAME `deep_think`
  flag: `kahya ask --derin` (client-supplied `taskRequest.deep_think`), and
  the byte-exact Turkish prompt prefix `"derin düşün:"`, detected and
  stripped **server-side**, in Go (never model-detected), before
  classification runs on the (now-bare) prompt. `claude-fable-5` still gets
  its mandatory `betas`/`fallbacks` shaping from the existing W4-04 proxy
  logic (`kahyad/internal/anthproxy/fable5.go`) — this task only creates
  the opt-in entry point that selects it, it does not re-implement that
  shaping.
- **Cost-governor 80% downgrade rung** (HANDOFF §4 ⚑, fixed chain
  Opus→Sonnet→yerel): `POST /v1/task`'s handler consults
  `kahyad/internal/anthproxy.Governor.Downgraded()` and passes it as
  `RouteInput.Downgraded`; `SelectModel` drops the table's chosen model ONE
  rung (`claude-opus-4-8`→`claude-sonnet-5`, `claude-sonnet-5`→the local
  lane) — never touching `claude-haiku-4-5` (no rung is defined for it) or
  an already-local decision. An explicit `--derin` opt-in is honored EVEN
  during an active downgrade (it is not itself a router choice to
  downgrade) — the handler ledgers `derin_during_downgrade` and prefixes
  the SSE `delta` stream with a Turkish spend warning in that case. The
  governor's own `budget_downgrade_unavailable` event (W12-08's stopgap for
  the Sonnet-class case, before this task's local lane existed) is
  **retired** — it is no longer emitted by any code path.
- **W4-03 Reader** (`kahyad/internal/reader`) now sources its own
  non-secret-lane cloud model choice from this SAME table
  (`router.SelectModel(RouteInput{Intent: router.IntentReader, ...})`)
  instead of a hand-maintained literal — value unchanged
  (`claude-haiku-4-5`), a routing-source change only.

### W6-02 note — push-to-talk / local STT phase (HANDOFF §4 ⚑ "mlx-whisper ... worker içinde kütüphane" + ordering invariant)

`kahya ask --audio <path>` (hammerspoon/kahya.lua's hold-to-talk release
handler) sends `POST /v1/task` with `input_audio_path` set and `prompt`
typically empty. `kahyad/internal/server.handleTask`'s own ordering
invariant for this case, enforced structurally (not by convention):

1. **Before** the ordinary blank-prompt check, `secretlane.
   ClassifyDeterministic`, intent routing, or envelope construction for
   the real task EVER run, `transcribeAudioLocally`
   (`kahyad/internal/server/stt.go`) spawns the worker in a SEPARATE,
   throwaway `mode="stt"` envelope (`spawn.ModeSTT`, `input_audio_path`
   set, `prompt` a fixed placeholder never read by the worker). This
   worker invocation never constructs a `ClaudeAgentOptions`/
   `ClaudeSDKClient`/MCP server at all (`kahya_worker.__main__.
   _run_stt_only`) and kahyad never opens a per-task Anthropic
   forward-proxy listener for it (`AnthropicBaseURL`/`APIKey` left
   blank) — it is CLOUD-LESS by construction, not merely by policy.
2. The worker resolves the model (`kahya_worker.stt.resolve_model`,
   `huggingface_hub.snapshot_download(..., local_files_only=True)` —
   fail-closed: a cache miss raises immediately, NEVER downloads) and
   transcribes (`mlx_whisper.transcribe(path, path_or_hf_repo=model_dir,
   language="tr")` — `language="tr"` is a FIXED LITERAL in
   `worker/kahya_worker/stt.py`, never a parameter/config/envelope
   field), then reports the transcript back over the SAME stdout
   protocol every other mode uses: one `{"type":"delta","text":
   "<transcript>"}` line + a terminal `{"type":"result","status":"ok"}`
   line (or a terminal `{"type":"error","message":"<Turkish>"}` line on
   any failure — a missing model, an empty/whitespace-only transcript,
   or any other transcription error). `kahyad/internal/spawn.Run`'s
   existing "delta"/"result"/"error" parsing needed NO changes for this
   — the same mechanism `kahyad/internal/reader.WorkerCloudModel`
   already uses to collect its own "reader"-mode worker's output.
3. Only once that call returns a non-blank transcript does `handleTask`
   set `req.Prompt` to it and fall through into the **exact same,
   unmodified** code every typed prompt already goes through:
   `secretlane.ClassifyDeterministic`, intent routing, envelope
   construction, the `lane=="secret"`/`decision.Local`/cloud branches.
   There is no separate, audio-specific routing decision anywhere past
   this point — a spoken finance-flavored command is classified and
   routed identically to the same typed command.

The worker also emits JSONL events to `worker.jsonl` (`kahya log --trace
<id>` surfaces these): `stt.completed` (`{"chars":<n>,"duration_ms":<n>}`)
on success, `stt.empty` on an empty/whitespace-only transcript (task then
fails with the byte-exact Turkish `"Ses anlaşılamadı — lütfen tekrar
deneyin"`), and deletes `input_audio_path` afterward in every case (see
`KAHYA_TMP_DIR`'s own row above) — the recording is consumed whether or
not usable text came out of it.

## 4 · Worker stdout protocol (JSONL)

One JSON object per line. Every line has a `"type"` field; kahyad ignores
any line whose `type` it does not recognize, and ignores (does not crash
on) any line that fails to parse as JSON at all — a malformed line is
silently skipped, not fatal to the task.

| `type` | Other fields | Meaning |
|---|---|---|
| `"delta"` | `"text"` | Incremental answer text. Relayed live to the `/v1/task` SSE stream as an `event: delta` frame (§6). |
| `"session"` | `"session_id"` | Persists `session_id` onto the task's `tasks` row (`UpdateTaskSession`) as it arrives — not just at the end. Session **resume** (using a stored `session_id` to continue a *later* task) is W4-02; W1-2 only records the value. |
| `"result"` | `"status"` (default `"ok"` if omitted) | Terminal success. Ends the stream with an `event: result` SSE frame; `tasks.state` → `done`; ledger `task_done`. |
| `"error"` | `"message"` (Turkish, user-facing) | Terminal application-level error. Ends the stream with an `event: error` SSE frame carrying that message verbatim; `tasks.state` → `error`; ledger `task_error`. |

**Unexpected termination:** if the worker process exits — any exit code,
including 0 — without ever sending a `"result"` or `"error"` line, kahyad
treats this as `task_error` with the generic Turkish message
`"Görev beklenmedik şekilde sonlandı. Ayrıntı: kahya log --trace %s"` (the
trace_id substituted in). This also covers the case where kahyad could not
even manage the process at all (e.g. `worker_cmd` misconfigured).

**`tasks.status` after the worker exits (W4-02/FIX-I-2):** a terminal
`error` line moves `tasks.status` to `failed` (the worker itself said the
task is over — nothing is left to resume), and so does a run kahyad could
not manage at all (`worker_cmd` misconfigured); a clean `result` moves it to
`done`. An exit without any terminal line, a timeout and
`cloud_unreachable` leave it `executing` for the resume scan (W4-07), whose
own re-dispatches are now counted in `tasks.crash_resumes` (migration 0020)
and capped by `task_max_crash_resumes` (config, default 3, ≥ 1): at the cap
the task moves to `blocked_user` with the notice `"Görev %s: işçi süreci %d
kez yarıda kesildi, otomatik devam limiti doldu — 'kahya task resolve %s'
ile karar ver."` (`task.resume_cap_exceeded`). Cloud-park redispatches never
count, and `kahya task resolve <id> --retry` resets the count. A status a
proxy callback, the budget pause or a halt already set mid-run is never
overridden. A **budget block** (task token ceiling, daily or monthly budget)
moves the task to `blocked_user` too (owner decision G10-1: resumed only by
`kahya task resolve <id> --retry`, never auto-parked to a later day):
`tasks.state` stays `paused_budget`, the SSE `error` message is
`"Görev bütçe tavanı nedeniyle duraklatıldı. Ayrıntı: kahya log --trace %s —
devam için: kahya task resolve %s --retry"`, ledger `task_budget_blocked`;
the proxy pauses, ledgers `task_paused_budget` and alarms once per dispatch
(every blocked request still gets its 503).

**Timeout:** if `cfg.task_timeout_min` elapses before the process sends a
terminal line, kahyad kills the whole process group and ends the stream
with `event: error`, message
`"Görev zaman aşımına uğradı (%d dk)."` (minutes substituted in);
`tasks.state` → `error`; ledger `task_timeout`.

**A killed worker is never a cloud failure (FIX-I-6):** when the worker
dies or is killed (task timeout, halt, shutdown) while one of its model
calls is open through the per-task proxy, the proxy sees its *inbound*
request cancelled. `retryTransport` returns `anthproxy.ErrInboundCanceled`
for it — checked before the cloud-error taxonomy, so it is never retried,
never parked and never `OnNonRetryableFailure` — and the proxy only writes
the JSONL line `proxy.inbound_canceled` (`task_id`, `attempts`). The task's
status is decided by whoever cancelled the worker: a timeout leaves it
`executing` for the resume scan, a halt leaves `user_halted`. A pending
inline-retry backoff sleep (up to 60 s with `Retry-After`) also ends on the
cancel. `cloudretry.Classify` treats a bare `context.Canceled` as
Retryable (defense in depth), and `task.CloudRetry.FailNonRetryable`
leaves a task that can no longer reach `failed` (already `user_halted` or
`failed`) untouched: no transition attempt, no second notification, one
`task.cloud_failure_skipped` ledger row.

**One park per outage; kahyad reports it (FIX-I-7):** the worker CLI
retries the proxy's 503 on its own, and every retry is a new inbound
request with its own inline-retry loop. The per-task proxy therefore fires
`OnCloudUnreachable` (and ledgers `proxy.cloud_unreachable`) once per
`Proxy` — once per dispatch; a redispatch opens a new one — and only logs
the JSONL line `proxy.cloud_unreachable_repeat` for the repeats (each
response is still the 503 + marker). `task.CloudRetry.ParkOrGiveUp` on a
task that is already `bekliyor-yeniden-deneme` changes nothing (the first
park's `next_retry_at`, outbox row and notification stand) and ledgers
`task.waiting_retry_dup`. The SDK hands the proxy's 503 to the worker as
`ResultMessage(is_error=True)`, not as an exception, so the worker checks
the same markers there (`_is_cloud_unreachable_text`) and prints
`{"event":"cloud_unreachable"}` + exit 3. kahyad does not rely on that line:
when the task's status is `bekliyor-yeniden-deneme` after the worker exits,
`handleTask` answers with `tasks.state` `waiting_retry`, ledger
`task_waiting_retry` and the SSE `error` message `task.MsgCloudParked`,
whatever the worker's terminal line said.

**`event`-keyed lines (not `type`-keyed):** a few worker lines carry a
top-level `"event"` key **instead of** `"type"` — kahyad matches these on
`spawn.stdoutLine.Event`, never conflated with the `type` switch above.
`{"event":"cloud_unreachable"}` (W4-04) is terminal (see §3). Since **W78-07**,
`{"event":"clarification_turn"}` is a **non-terminal** line the worker emits
at most once, immediately before its terminal `"result"`, when the assistant
asked the user a clarifying question **before acting** (HANDOFF §6 ⚑
*açıklama-turu*). It is **not** relayed to the SSE stream and never changes
`tasks.state`; kahyad — the sole `brain.db` writer — turns it into one
`events` row `kind="clarification_turn"` (carrying the task's `trace_id`) that
the W78-04 north-star metric counts. The worker only ever *signals* the turn;
it never writes the ledger itself.

## 5 · `POST /v1/task` (HTTP-over-UDS, `~/Library/Application Support/Kahya/kahyad.sock`)

Request body (matches `kahyad/cmd/kahya/client.go`'s `StreamTask`, W12-06):

```json
{"prompt": "test sorusu", "trace_id": "3f9a6b2c1d4e5f60718293a4b5c6d7e8"}
```

`trace_id` is optional in the body — if absent, kahyad falls back to the
`X-Kahya-Trace-Id` request header, or mints a fresh one if that is also
absent. An empty/whitespace-only `prompt` is rejected locally with `400`
before a task is minted at all — UNLESS `input_audio_path` is set (W6-02:
`kahya ask --audio <path>`), in which case that check runs against the
transcript kahyad produces locally first (see the "W6-02 note" below);
`input_audio_path` itself must be an absolute path or the request is
rejected with `400` before anything else happens.

**Body size cap:** the request body is capped at 8 MiB
(`http.MaxBytesReader`, `kahyad/internal/server.taskBodyMaxBytes`) before
`json.Decode` ever runs — generous for even a very long prompt, but bounded
so an oversized body can't tie up the daemon decoding it. A body over the
cap (or otherwise malformed) is rejected with `400` or `413`, same as any
other pre-SSE validation failure — this happens before the SSE response
starts, so no task is minted and no worker is spawned.

Response: `Content-Type: text/event-stream`. Exactly the SSE contract
W12-06 already implements client-side — this task (W12-07) is the server
side that must match it, not the other way around:

```
event: delta
data: {"text":"..."}

event: delta
data: {"text":"..."}

event: result
data: {"status":"ok","task_id":"t_...","session_id":""}

```

or, on any failure path (timeout / unexpected exit / worker-reported
error):

```
event: error
data: {"message":"<Turkish>"}

```

Exactly one terminal event (`result` or `error`) ends the stream; zero or
more `delta` events precede it. `session_id` is the empty string `""` in
W1-2 (session resume lands in W4-02 and will start populating a real
value — the field is always present so the CLI parser never has to change
shape).

**Persistence per task** (`kahyad/internal/store/sqlcgen`, tables `tasks`
+ `events`):
- `InsertTask` — `state='running'`, `taint_tier='untrusted'`, `model`, the
  full envelope JSON (only for a task handed to a worker: a locally
  answered secret-lane / downgrade-routed task stores none — FIX-I-14
  review), before the SSE response even starts.
- Ledger `task_spawned` right after the insert.
- `UpdateTaskSession` — as the worker's `"session"` line arrives, if any.
- `UpdateTaskState` — `done` or `error` once the terminal outcome is known.
- Ledger `task_done` / `task_error` / `task_timeout` to match.

**Every task row walks the status machine (FIX-I-14):** cloud or local,
`tasks.status` goes `intent → executing → done|failed` (plus the W4-02
park/block/halt edges). Secret-lane and downgrade-routed local tasks
(`handleSecretLaneTask`) used to stay at `intent` forever, so `kahya task
show` reported an answered task as `intent` and every halt-all swept it;
they now enter `executing` before the local answer and end `done` (answer
delivered) or `failed` (any local-answer error, `yerel model için bellek
yok` included — fail-closed, never a cloud fallback). The morning
briefing's own task row does the same (`failed` on a spawn/summary error,
`done` once the summary validated; `tasks.state` `done`/`error` alongside).
A task that has not started yet can be halted (`intent → user_halted`, the
one new edge), and a halt counts — `"⛔ N görev durduruldu"`, `halted_at`,
`task.user_halted` — only when the `user_halted` transition really
happened. Migration 0021 (data only) moves old `intent` rows whose
`state` is `done`/`error` to `done`/`failed`. A task row without a stored
envelope (the briefing's) is never resumed: the outbox dispatcher fails it
and acks the resume row (`outbox.envelope_invalid`, once — G10-E2).

**Sessions that run inside kahyad are live; local rows are never handed to
a worker (FIX-I-14 review):** a local answer and a briefing run have no
worker pid, yet their task is `executing` for the whole run. Each holds
`task.LiveRegistry.BeginInProcess(task_id)` from before its `executing`
transition until after its terminal one (`handleSecretLaneTask` via
`SetTaskLiveRegistry`, the briefing via `Orchestrator.Live`; main.go hands
the same registry to the resume scan and the outbox dispatcher), so a
resume-scan tick in the middle never takes the running session for a
crashed worker (`PID`/`BySID` still report only real workers). A cloud
task holds one too, from its `executing` transition until `handleTask`
returns, covering the window before its worker registers. A locally
answered task — secret lane or downgrade route — stores **no**
`tasks.envelope` (it is never given to a worker), and the outbox
dispatcher never gives a secret-lane row (row lane or stored envelope
lane) to a worker: after a kahyad restart mid-answer, or a `kahya task
resolve --retry`, it fails the task and acks the row
(`outbox.resume_refused_secret_lane`; a downgrade-routed row takes the
envelope-less G10-E2 path). A task the outbox could never resume
(`task.Resumable` false: no stored envelope, or secret lane) is never
parked or budget-blocked either: `CloudRetry.ParkOrGiveUp` fails it
(`task.failed` reason `not_resumable`, no notice) and the budget pause
fails it instead of `blocked_user`. A resume row whose task already ended
(`done`/`failed`) is acked (`outbox.redelivery_guarded`) instead of an
illegal `→ executing` attempt; an envelope-less row found parked ends
`failed` through `bekliyor-yeniden-deneme → executing → failed`.

**A redispatch is bounded and counted (FIX-I-3, G10-E1):** the outbox
dispatcher runs every redispatched worker under the same
`task_timeout_min` deadline `POST /v1/task` gives a first spawn
(`Dispatcher.SetTaskTimeout`; spawn.Run SIGKILLs the process group when it
passes) — before, one hung resumed worker blocked `ClaimAndDispatch`
forever (its lease heartbeat kept the row claimed and the overlap guard
skipped every later tick). Every finished redispatch acks its row: a
worker that ends without a terminal line (crash, kill, that deadline, or
kahyad failing to start it) leaves the task `executing` and ledgers
`outbox.redispatch_crashed` / `outbox.redispatch_timed_out`; the task's
next attempt comes only from the resume scan, the one path that counts
`tasks.crash_resumes` against `task_max_crash_resumes` (a lease-expiry
re-claim would bypass that cap). A resumed worker's own terminal `error`
line fails the task (`outbox.redispatch_worker_error`), the same rule
`handleTask` applies at first spawn (FIX-I-2). A row's lease now only
matters when kahyad itself dies while dispatching it.

All of the above tasks-table/ledger writes after the worker starts use the
HTTP request's own context (not the per-task timeout context) so that a
timeout — which cancels the timeout context by design — never prevents
kahyad from recording that the task timed out.

## 5a · Control-plane identity (G03 / B-3)

kahyad's one UDS listener serves both the human surfaces (the `kahya` CLI,
`kahya-trigger` under launchd, Hammerspoon — which only ever talks through
the CLI) and every worker kahyad spawns (`KAHYA_SOCKET`, §3). Socket mode
0600 separates uids only and the worker runs as the same uid, so kahyad
needs a second identity to tell them apart.

**Secret.** On every boot (`server.Prepare`, while the startup flock is
held, before `Serve`) kahyad mints 32 random bytes and writes them as 64
lowercase hex chars plus `\n` to `<socket>.secret` (e.g.
`~/Library/Application Support/Kahya/kahyad.sock.secret`, mode 0600, in the
0700 socket dir; temp file + rename). The path is derived from the socket
path (`config.ControlSecretPath`), so the dev profile gets
`kahyad-dev.sock.secret` automatically; there is no config key. If the file
cannot be written, `Prepare` fails and no listener is served (fail-closed).
`Shutdown` removes the file as its last step (after the listener is closed,
while the flock is still held). The secret rotates on every restart; clients
re-read the file on every request (`config.ReadControlSecret`), so a
running Hammerspoon or a long test harness picks up the new value.

**Header.** `X-Kahya-Control-Secret: <64 hex>` (`config.ControlSecretHeader`),
compared in constant time against the secret kahyad holds in memory (never
against the file).

**Route classes** (`kahyad/internal/server/controlauth.go classifyRoute`,
exact path match):

| Class | Paths | Rule |
|---|---|---|
| open | `/health` | no identity needed |
| worker | `/policy/check`, `/v1/memory/search`, `/v1/mcp`, `/policy/consume-token` | reachable without the secret (a worker never has it) — since B-14 only by a caller attributable to a live worker; anyone else needs the secret (see "Caller attribution" below) |
| control | everything else — including any unknown/unclean path and every route added later without a deliberate classification | requires the secret |

Read-only routes `GET /metrics`, `GET /readiness`, `GET /v1/log`,
`GET /policy/state` are deliberately **control** (owner decision G03-4): a
tainted worker must not read other tasks' log lines or ledger summaries.
`POST /session/sensitive-read` is control too — it has no caller outside
kahyad's own process today.

**Rejection.** A control route called with a missing or wrong secret
answers `403` + `{"error": "Kontrol anahtarı eksik ya da geçersiz
(fail-closed). Bu uç nokta yalnızca kahya CLI, kahya-trigger ve Hammerspoon
içindir."}` (`server.MsgControlSecretRejected`, byte-exact) and is ledgered
as `kind=control_auth_rejected` with `payload.reason` = `missing_secret` |
`bad_secret` (plus `path`, `method`, and the peer's `peer_pid`/`peer_pgid`/
`peer_sid`) under the request's trace id; the caller-attribution reasons
(`unattributable_worker_route`, `live_worker_on_control_route`,
`attributed_worker_without_trace`, `unresolved_peer`) are below. The outer
`event=http_request` JSONL line still records `status=403`.

**Who sends it.** The `kahya` CLI (`Client.newRequest`, every request),
`kahya-trigger`, the test harnesses (`tests/w3`, `tests/acceptance/w4,w6`,
`tests/e2e`, `kahyad/internal/mlxe2e`) and the shell scripts
(`scripts/restore-drill.sh`, `scripts/accept_w4.sh`, `scripts/w5_gate.sh`,
`scripts/accept-w12.sh`: `-H "X-Kahya-Control-Secret: $(cat
"$SOCK.secret")"`) — the last two only call worker routes, which need it
from a shell since B-14 (below). `surface=local` on
`/approvals/{id}/decision` and `/policy/feedback` therefore means "a
control-secret-authenticated UDS call". The `kahya-mcp` bridge is
worker-plane and never reads or sends the secret.

**Caller attribution (G03 / B-14).** The `task_id`/`trace_id` a caller
*declares* on a worker route (JSON body on `/policy/check`,
`/v1/memory/search`, `/policy/consume-token`; `X-Kahya-Trace-Id` /
`X-Kahya-Task-Id` headers on `/v1/mcp`) are no longer trusted. kahyad
records the OS-level peer pid of every accepted connection
(`http.Server.ConnContext`: `LOCAL_PEERPID` on macOS, `SO_PEERCRED` on
Linux) and that process's **session id** (`getsid`), once, at accept time,
and on every request maps that session to a live worker it spawned
(`task.LiveRegistry.BySID` — `spawn.Run` makes each worker the leader of its
own session (`setsid`), `handleTask` and the outbox dispatcher register it
with `RegisterWorker(task_id, trace_id, pid, sid)`, sid == pid). The
`kahya-mcp` bridge — and every other process the worker's tree spawns —
inherits the worker's session, so it is attributed to the same task.

The key is the session, **not** the process group, on purpose: `setpgid(2)`
lets a process join any other process group of its own session or leave its
group with `setpgid(0, 0)`, so with workers that all shared kahyad's session
(the original `setpgid`-only spawn) a compromised worker's child could join a
clean worker's group and be attributed to the clean task (taint laundering
without naming a foreign id), and a `setpgid(0, 0)` child became
unattributable. A session cannot be joined from outside it: the cross-worker
`setpgid` fails with `EPERM`, a `setpgid(0, 0)` child keeps its session (still
pinned, still refused on control routes), and the only way out — `setsid()`
in a forked child — yields a fresh session no worker is registered under
(`TestWorkerSessionAttributionResistsGroupJoin`).

A connection whose peer cannot be resolved to a session — the connecting
process exited before kahyad accepted (its fd may live on in a forked child;
macOS keeps reporting the dead pid), or the connection is not a UDS peer —
is refused on every route except `/health`, **even with the secret**
(`reason=unresolved_peer`): kahyad cannot rule out a live worker there.

| Caller | Worker route | Control route |
|---|---|---|
| attributable to a live worker | served; ids **pinned** to the worker's own task (headers overwritten, body ids overridden); a conflicting declaration is ledgered as `kind=worker_identity_override` (`source` = `header` \| `body`, `claimed_task_id`, `claimed_trace_id`, `peer_pid`, `peer_pgid`, `peer_sid`) under the worker's own trace | **403** even with the secret (`reason=live_worker_on_control_route`) |
| not attributable (shell script, test harness, CLI) | served only with the secret, declared ids honored; else **403** `reason=unattributable_worker_route` | secret required (above) |
| unresolved peer (no session) | **403** even with the secret (`reason=unresolved_peer`) | **403** even with the secret (`reason=unresolved_peer`) |

A live entry registered without a trace id (legacy pid-only `Register`)
fails closed (`reason=attributed_worker_without_trace`). Because pinning
happens before the handlers run, the outer `event=http_request` JSONL line
keeps the trace id the caller *declared*, while `policy_decision`,
`hafiza_injected` and `worker_identity_override` rows carry the pinned one —
deliberately, so the two traces cross-reference each other.

The live-worker rules above hold **while the worker is registered**, i.e.
while its task is running. The registration is dropped when `spawn.Run`
returns (`handleTask`'s deferred `Unregister`), so a descendant that
outlives the task would afterwards be unattributable — and, reading
`<socket>.secret`, could promote the ladder on a control route or launder a
clean task's ids on a worker route. To close that, `spawn.Run` SIGKILLs the
worker's process group when the worker exits, on the **normal-exit path as
well as on a timeout** (G03 / B-14 robust review; §1): no descendant that
stayed in the worker's group outlives the task. A child that first leaves
the group — `setpgid(0, 0)` (a new group in the same session) or `setsid()`
(a new session), neither of which the sanctioned worker does — escapes the
reap and is unattributable; it is refused on worker routes without the
secret.

**Same-uid limit.** A worker compromised to the point of reading arbitrary
files as the user can read `<socket>.secret`; this gate closes the path for
everything that only uses what a worker is *given* (its env, its tools, a
prompt-injected model), and B-14 refuses the secret on control routes from
anywhere inside a live worker's session. Two residuals remain, each
requiring a read of the secret file: **(a)** a forked worker child that
calls `setsid()` lands in a fresh, unregistered session, so it is
unattributable **immediately** — including while its worker's task is still
running — and with the secret it reaches the control routes and has its
declared ids honored on worker routes; **(b)** a forked child that calls
`setpgid(0, 0)` stays in the worker's session (pinned on worker routes,
refused on control routes while the task runs) but escapes the group reap,
so once it outlives the task it is unattributable with the same
consequences as (a). Sandboxing the worker (`sandbox-exec`), a Keychain-ACL'd
secret, or a separate uid would close even that, and were considered and
declined for now (owner decisions G03-1/2/3).

## 6 · `POST /policy/check` (HTTP-over-UDS, same socket)

The binding, fail-closed policy decision point (HANDOFF §4/§5 ⚑:
`can_use_tool` inside the worker is an early-reject/UX layer only, never
the security boundary — the worker's `can_use_tool` hook (W12-09) calls
this endpoint for every tool-use attempt, but the endpoint itself, running
inside kahyad, is what's binding). **Caller timeout budget: 5 seconds;
any error or timeout on the caller's side must be treated as `deny`
(fail-closed)** — the endpoint itself does no I/O beyond one ledger
insert, so it always answers in well under that budget. Worker side
(FIX-I-16): `kahya_worker.hooks` runs the synchronous stdlib UDS call
(`udshttp.post_json`) in a worker thread via `asyncio.to_thread`, for both
`can_use_tool` and the `UserPromptSubmit` memory search, so the SDK's event
loop keeps draining the CLI's stdout/control messages (and parallel tool
checks really run in parallel) while kahyad answers; the fail-closed
handling is unchanged.

**Body size cap:** the request body is capped at 1 MiB
(`http.MaxBytesReader`, `kahyad/internal/server.policyCheckMaxBody`) before
`json.Decode` ever runs — a real `tool_input` is tiny, so this can never
meaningfully eat into the 5s budget above (an uncapped multi-megabyte body
alone can take seconds just to read).

Request:

```json
{
  "trace_id": "3f9a6b2c1d4e5f60718293a4b5c6d7e8",
  "task_id": "t_9f2c1a4b5e6d7f8091a2b3c4d5e6f708",
  "session_id": null,
  "tool_name": "Read",
  "tool_input": {},
  "preflight": true
}
```

`preflight` (since B-1) is sent `true` by the worker's `can_use_tool` on
every call (`kahya_worker.hooks.POLICY_CHECK_PREFLIGHT`): kahyad decides
and ledgers the decision (payload `preflight:true`) but **mints nothing** —
no `pending_approvals` row, no token, no undo window — so the response
never carries `pending_approval_id` or `token`. The binding decision, and
the only pending approval a human is ever asked about, comes from the
tool-side gate on `/v1/mcp` (§6a). The worker maps `needs_approval` for
one of kahyad's own MCP tools (`mcp__kahya_memory__*`) to "allow — defer
to the binding gate" (`event=tool_gate decision=defer`) and denies it for
any SDK built-in tool, which has no kahyad-side executor (`"Bu araç için
onay gerekiyor ancak bu yüzeyden onaylanamaz (reddedildi)."`). A
non-preflight call (a script with the control secret) keeps the original
behavior below.

`task_id`/`trace_id` stay in the body (backward compatibility), but for a
call from a live worker kahyad spawned the **authority is the UDS peer's
attributed identity** (§5a): the body's values are overridden with the
worker's own ids (and a mismatch is ledgered as `worker_identity_override`).
Only an unattributed, control-secret-authenticated caller (a script) has its
declared ids honored.

`tool_name` is canonicalized before lookup: an SDK-style
`mcp__<server>__<tool>` prefix (e.g.
`mcp__kahya_memory__memory_search`) is stripped down to the bare tool name
(`memory_search`) — built-in tools arrive bare already
(`kahyad/internal/policy.Canonicalize`). The Actor worker starts the CLI
with `--tools ""`, `--disallowedTools …` and `--setting-sources=` (B-15:
`tools=[]`, `disallowed_tools=BUILTIN_TOOLS_DISALLOWED`,
`setting_sources=[]` in `kahya_worker.__main__._build_options`; the Reader
gets the same three), so no Claude Code built-in tool is defined in the
session and no `settings.json` hook, permission allow rule or `CLAUDE.md`
is loaded; the CLI auto-allowed its read-only built-ins (Read/Glob/Grep
inside cwd) without ever calling `can_use_tool`, which is why omission plus
this gate was not enough. A built-in tool name reaching `/policy/check` is
therefore only ever a defense-in-depth event, denied as an unknown tool.
`scope` is optional (defaults to
`"global"`) — the ladder's third key dimension alongside `tool`; `class`
is never accepted from the caller, only ever resolved from the loaded
`policy.yaml`.

Response:

```json
{"decision": "allow", "rule": "ladder-v1", "token": "<hex, only for a side-effectful class>"}
```

or

```json
{"decision": "needs_approval", "reason": "<Turkish>", "rule": "ladder-v1", "pending_approval_id": "<opaque>"}
```

or

```json
{"decision": "deny", "reason": "<Turkish>", "rule": "ladder-v1"}
```

**W3-02 autonomy-ladder engine** (`kahyad/internal/policy/engine.go`,
replacing W12-07's interim static table — "one engine, two mount points,
never a second copy": the SAME engine also gates `POST /v1/mcp`'s
`tools/call` dispatch). Given (tool, class, scope): look up the tool in
the loaded `policy.yaml` (missing ⇒ `deny`, reason
`"Tanınmayan araç reddedildi (fail-closed)."`); look up
`autonomy_state(tool,class,scope)` (missing row ⇒ L0); apply the HANDOFF
§4 ladder table (R auto at L1+, W1 at L2+ — opening a 5-minute
`undo_windows` row, idempotently reusing an already-open one for the same
task_id/tool/trace_id on a retried call — W2 at L3+; **W3 never
auto-allows, at any level, hard-coded in Go**). `allow` on a non-R class
mints a one-time approval token (bound to `task_id` + `tool`/`class`/
`scope` + a sha256 of `tool_input`) the caller must present to
`POST /policy/consume-token` before executing; `needs_approval` inserts a
server-issued, single-use `pending_approvals` row (32 random bytes hex,
bound to the RESOLVED tool/class/scope/task_id/trace_id/
approved_bytes_hash, 10-minute TTL) and returns its id as
`pending_approval_id` — an opaque reference an approval surface later
resolves via `POST /policy/feedback`, never a caller-decodable blob.
`Engine.Approve`/`Deny` look this row up by id and atomically consume it
(`consumed_at IS NULL`); a forged, expired, or already-consumed id is
rejected outright, before any token is minted or any bookkeeping runs -
and `POST /policy/consume-token` failures demote the token's REAL bound
`(tool,class,scope)` (recovered from `approval_tokens` by `token_hash`),
never whatever the request body itself claims.

- **Malformed request body, or one over the 1 MiB cap above, ⇒ HTTP 400 or
  413**, but the body still says `{"decision":"deny",...}` (`rule` still
  `"ladder-v1"`, `reason` `"Geçersiz istek gövdesi
  (fail-closed)."`) — so a sloppy client cannot parse an "allow" out of a
  transport-level error. A best-effort `policy_decision` ledger row IS
  still written for this case, under the `trace_id` `withTraceLogging`
  already resolved from the `X-Kahya-Trace-Id` header (independent of the
  unparseable/oversized body itself) — fail-closed applies to the ledger
  too, so evidence that a deny happened is never silently dropped just
  because the body couldn't be parsed.

**Ledger:** every decision writes exactly one `kind='policy_decision'`
event, payload includes `event`, `tool`, `class`, `scope`, `level`,
`decision`, `task_id` (and `reason` when not `allow`; `preflight:true` for
a worker preflight; `redeemed_pending_approval_id` when an `allow` came
from redeeming a human approval, §6a) — under the request's own
`trace_id`. See `kahyad/internal/policy/README.md` for the full wire
schema of `/policy/consume-token`, `/policy/feedback`, `/policy/state`,
`/policy/promote`, and `/policy/undo`.

## 6a · Approval loop: needs_approval → human → exactly the approved bytes (B-1)

Every in-process binding gate — `/v1/mcp`'s `policyGateMiddleware`
(`memory_*`, the dev stub) and the fs/shell/osascript tools' own
`Server.FSPolicyClient` — calls `checkWithApprovalWait`
(`kahyad/internal/server/approval_wait.go`):

1. `Engine.Check` answers `needs_approval` and mints ONE `pending_approvals`
   row (bound to task, tool, scope and the sha256 of the canonical tool
   bytes); the approval card goes out (Telegram for a reversible tool,
   Hammerspoon, `kahya approvals`).
2. The in-flight `tools/call` **parks inside kahyad** for
   `approval_wait_seconds` (config, default 300, at most 600 — the pending
   approval's own TTL — and, when written explicitly, below
   `task_timeout_min*60`; `0` = never park — the
   call answers at once with `"⏳ Yerel onay bekleniyor (onay kimliği:
   <id>). …"`). Ledger `approval_wait_started`. A `kahya ask` stream of the
   same task gets the delta line `"⏳ Onay bekleniyor (onay kimliği: <id>)
   — başka bir terminalde 'kahya approve <id>' çalıştır."` and an SSE
   comment keepalive (`: keepalive`) every 10 s while it waits (the CLI
   drops a stream idle for 30 s).
3. The human answers (`kahya approve <id>` / `kahya approvals decide`,
   Hammerspoon, or a Telegram tap for a `reversible: true` tool —
   `Engine.Approve` refuses an irreversible tool from any non-local
   surface, B-11). `Engine.Approve` persists a single-use **grant**
   (`approval_tokens.grant_kind='approval'`); the engine's decided hook
   wakes the parked call at once (a 2 s DB probe is the fallback).
4. The same call re-checks **bound to its own pending id**
   (`RedeemPendingApprovalID` + `NoMintPending`: never a second pending
   row). `Engine.Check` **redeems** that id's grant and no other — never a
   sibling call's grant for the same task and bytes, never a ladder
   auto-allow (a level raised while the call waited does not count) —
   burned atomically, ledgered
   `approval_grant_redeemed` — and answers `allow` with a fresh
   `grant_kind='exchanged'` token the tool consumes through the unchanged
   `ConsumeToken` (WYSIWYE hash check included). The tool executes exactly
   the approved bytes, once; they never pass back through the model.
   Ledger `approval_wait_ended{outcome:"approved"}`.

Other outcomes (`approval_wait_ended.outcome`): `denied` (a denial or a
halt's `InvalidateApprovalsForTask` — the model reads `"❌ Kullanıcı bu
eylemi reddetti; …"`; final: no re-check runs, so nothing executes), `timeout` (nobody decided; the id stays live and the
model is told to retry the identical call after an approval — that retry
redeems the grant), `canceled` (the bridge connection closed — worker
killed by `task_timeout_min` or a halt — or kahyad is shutting down:
**never redeemed**, so a late approval executes nothing without a live,
client-attached call). The go-sdk handler context never cancels on a
client disconnect, so the server binds the wait to the HTTP request's own
`Done()` channel (`requestDoneKey`). An approved grant nobody redeems
expires after 10 minutes (`approval_grant_expired`); a halt revokes it.
Every tool-side gate passes the calling task's id to `Engine.Check`
(`fs_read` included: `mcp/fs.HandleReadForTask`), so the pending row, the
grant, a halt's sweep and the `kahya ask` notice are keyed on the real
task; a grant for a call that carried no task id is redeemable only by
that call's own parked re-check, never by a hash-matched retry.

`POST /policy/feedback` and `POST /approvals/{id}/decision` answer
`{ok, error?}` — **no raw token** any more: the grant is redeemed
server-side, so a raw value on the wire could only be presented to
`/policy/consume-token` by some other process.

**First-run autonomy (`autonomy_bootstrap_levels`):** at boot kahyad seeds
the configured `"tool/class/scope": level` rows (default
`{"memory_search/R/global": 1}` — the assistant may read its own memory on
a fresh install) **only where no `autonomy_state` row exists yet**, ledgered
`autonomy_bootstrapped`; an existing row — a demotion back to L0 included —
is never touched (equivalent to one `kahya autonomy promote` per key on
first boot). `{}` disables it.

## 7 · Cross-references

- SSE contract, CLI-side: `kahyad/cmd/kahya/client.go` (`StreamTask`,
  `readSSE`), frozen in W12-06's task file.
- Autonomy-ladder engine + `/v1/mcp` gate: `kahyad/internal/policy`
  (`engine.go`, `tokens.go`, `README.md`), `kahyad/internal/server/mcp.go`
  (W12-05/W3-02).
- `tasks`/`events` schema: `kahyad/migrations/0001_init_schema.sql`
  (W12-02); `autonomy_state`/`approval_tokens`/`undo_windows`:
  `kahyad/migrations/0003_autonomy_policy.sql` (W3-02); `tasks.lane`/
  `tasks.secret_category`: `kahyad/migrations/0006_secret_lane.sql`
  (W3-08).
- Secret-lane classifier/router/local-answer path: `kahyad/internal/
  secretlane` (`classifier.go`, `router.go`, `answer.go`, `httpchat.go`);
  local Qwen3-30B-A3B supervisor: `kahyad/internal/mlx` (`supervisor.go`,
  `memcheck.go`, `adapters.go`), reusing `kahyad/internal/mlxsup` verbatim;
  venv/setup: `mlx/qwen/README.md` (W3-08).

## 8 · Out of scope / what changes later

- **Real Python worker** (`ClaudeSDKClient`, streaming input mode,
  `UserPromptSubmit` hook, `can_use_tool` client side) — landed in W12-09
  as `worker/kahya_worker` (this package's own `kahyad/internal/spawn`
  tests still exercise the fake worker scripts under
  `kahyad/internal/spawn/testdata/`, since those tests are about the Go
  spawn/env plumbing, not the real worker's own SDK session logic - that
  lives in `worker/tests`, hermetic against a mocked `ClaudeSDKClient`).
  The one deferred item is a **live** end-to-end run (real Anthropic
  credential/Claude Code session, real seeded corpus) — no CI test in
  this repo exercises that; see W12-09's task file and closing commit for
  the explicit deferral note.
- **Forward-proxy + real `ANTHROPIC_BASE_URL`** — landed in W12-08 (see
  the note above); the model-call `egressGate` hook is now the real
  `kahyad/internal/egress.Gate.Check` allowlist/budget/sensitive-read gate
  (W3-05). The "→yerel" downgrade rung landed in W4-08 (see that note
  above) — `budget_downgrade_unavailable` is retired, no longer emitted by
  any code path.
- **W3-05 egress-security review fixes (post-W3-05, this pass)** —
  tightened the gate/proxy without changing the IPC contract's own wire
  shapes:
  - **Taint/session key is always `trace_id`, never a model-supplied
    argument.** `mcp/fs`'s `fs_read` used to take an optional,
    caller-supplied `session_id` tool-call argument as its taint key —
    skippable (omit it) and launderable (present a different id). That
    argument is REMOVED; `fs_read`'s secret-lane taint mark is now
    unconditional and keyed on the REQUEST's own server-assigned
    `X-Kahya-Trace-Id`. The anthproxy egress-gate factory
    (`kahyad/internal/egress.NewAnthproxyEgressGateHook`, wired by
    `kahyad/internal/server.SetAnthproxy`) keys `SessionInfo.SessionID` on
    that SAME `trace_id`, so a secret-lane read anywhere in a task's own
    lifetime hard-blocks that task's later model-call egress the instant
    it would leave the allowlist. Since FIX-B-7 the mark is durable
    (brain.db `sensitive_reads`, migration 0019): it survives a kahyad
    restart + resume (a resumed task keeps its `trace_id`), and a mark that
    cannot be persisted makes `fs_read` refuse the read
    (`mcp/fs.MsgSecretLaneReadRefused`, ledger `secret_lane_read_refused`)
    instead of returning the bytes. Since FIX-B-6 the same read also
    escalates the calling task's persisted lane to secret (same refusal on
    failure), so the allowlisted model call itself is cut off at the proxy
    backstop - the sensitive-read mark alone never blocked it, because
    `api.anthropic.com` is on the allowlist.
  - **`needs_network:true` container egress is attributed via a
    per-task Proxy-Authorization credential.** `mcp/shell`'s
    `EgressTokenRegistrar` seam (`kahyad/internal/server.
    NewEgressTokenRegistrar`, backed by
    `kahyad/internal/egress.ProxySessionRegistry`) mints a per-task token
    embedded as the container's `HTTP_PROXY`/`HTTPS_PROXY` URL userinfo
    (`http://<token>:@kahya-egress-fwd:3128` — curl/wget/most HTTP clients
    send that as `Proxy-Authorization: Basic ...` automatically). The
    shared `kahyad/internal/egress.Proxy` listener (the ONE forward-proxy
    every `needs_network:true` container reaches, for the whole daemon's
    lifetime — unlike the per-task anthproxy listener above) looks the
    token back up to attribute the connection to that task's own
    `trace_id`. A request with no/unknown token is an ANONYMOUS,
    UNTAINTED session — still fully allowlist+budget-gated (Gate.Check's
    ordinary empty-`SessionID` path), just never sensitive-read-tainted;
    this is the documented floor for a connection the proxy cannot
    attribute to any known task (fail-closed on TAINT is not possible
    without an identity to key it on).
  - **An `egress.allowlist` entry with no `ports:` now means 443
    (HTTPS) only**, never "any port" — every entry in this repo's own
    `policy.yaml` already relied on this (all HTTPS endpoints); an
    operator who genuinely needs a different/additional port must say so
    explicitly via `ports:`.
  - **DNS rebinding**: an allowlisted HOSTNAME's resolved address is now
    validated (every resolved IP must be public) and the ACTUAL dial is
    pinned to that validated literal (`kahyad/internal/egress/proxy.go`'s
    `resolveAndPin`) — a hijacked/rebound DNS answer for e.g.
    `api.anthropic.com` pointing at a private/link-local/loopback address
    is denied at dial time (`egress_blocked_dns_rebind`), even though the
    hostname itself is allowlisted.
  - **`kahya-egress`'s own gateway IP** (on-link, reachable by direct IP
    from any attached container even with `--internal`'s "no route out")
    is now blocked via an iptables `DROP` rule kahyad installs inside the
    colima VM's own `INPUT` chain (`mcp/shell.EgressNetworkEnsurer.Ensure`
    — re-applied idempotently before every `needs_network:true` run).
    This is a within-colima-VM mitigation: it does not (and cannot, from
    inside the VM's own iptables) protect against a hypothetical escape
    from the colima VM to the macOS host itself.
- **Approval surface rendering** (Telegram inline buttons, CLI "onayla" +
  byte-exact WYSIWYE diff, Hammerspoon cards) — W3-06/W3-07/W6-01. W3-02
  only exposes the engine API those surfaces call
  (`POST /policy/feedback`'s `approve`/`deny`, and the `pending_approval_id`
  a decision returns). Since B-1 an approval actually executes the parked
  tool call (§6a).
- **Undo recipe implementations** (Trash restore, git checkpoint restore,
  ...) — W3-03. `kahya undo --trace <id>` / `POST /policy/undo` only
  trigger the window + demotion; executing the actual recipe is the owning
  tool's job.
- **Session resume, receipts, retries, outbox dispatch** — landed in
  W4-02 as `kahyad/internal/task` (state machine `machine.go`, receipt/
  idempotent-replay lifecycle `receipts.go`, resume-scan `resume.go`) and
  `kahyad/internal/outbox` (lease-based redelivery `dispatcher.go`); the
  `resume`/`session_id` envelope fields above are that task's own wire
  addition. Cloud-retry taxonomy and `bekliyor-yeniden-deneme`
  *population* remain W4-04; `user_halted` *semantics* remain W6-03 (only
  the enum value + the dispatcher's never-redeliver guard exist so far).
  FIX-I-12: `Receipts.Execute` runs an (external) effect with no
  transaction and no brain.db connection checked out (`tx` is nil) and
  writes the receipt in one short statement afterwards, so a slow effect
  never blocks `/policy/check` or the ledger on the single pooled
  connection; `Receipts.ExecuteInTx` keeps the in-transaction semantics
  for short effects that write brain.db and must commit atomically with
  their receipt. Because the periodic resume scan now runs while an effect
  is in flight, `*Receipts` is also a resume-scan `LiveChecker`
  (`task.AnyLive(liveRegistry, receipts)`): a task whose side effect is
  still executing inside kahyad (its worker may already be dead — W4-07
  scenario A) is left alone until the receipt or failure is written.
- **Taint checks in policy decisions** — W4-03.
- **Intent router / dynamic model routing** — landed in W4-08 (see that
  note above): `model` is now set by `kahyad/internal/router.SelectModel`,
  not a static `cfg.default_model`.
- **Secret-lane classifier wired into memory_write/fs-read/mail-web
  Reader ingestion** — W4-03 wires the W4-03 Reader path (see that
  package's own doc comment); fs_read escalates the calling task's lane
  since FIX-B-6 (see the W3-08 note above); memory_write escalation
  remains future work (G13, G00 U-3). W3-08 (see its own note above) exposes the full
  `secretlane.Classifier` (deterministic + Qwen fallback, now also
  returning `intent`, W4-08) and `secretlane.Escalate` (sticky lane
  widening) for those tasks to call; `POST /v1/task`'s own classification
  uses the deterministic pre-pass only, per that note's own reasoning.
- **Subagent/fan-out execution itself** — post-core; W4-08 only encodes
  the `subagent_exec`/`fanout` table row's model constant
  (`claude-sonnet-5`) for a future runner to read via
  `kahyad/internal/router.SelectModel`.

## 9 · W1-2 gate — how to re-run

W12-10 froze the HANDOFF §6 W1-2 acceptance sentence — a CLI question
answered with a `<hafiza>` injection, `'evlerimizden'` finding the `'ev'`
seed note, one `trace_id` spanning every JSONL log, every injected
`<hafiza>` block in the ledger — as two runnable checks:

1. **`make test`** — the HERMETIC gate
   (`tests/e2e/w12_gate_test.go`, build tag `e2e`; `make test`'s own
   `test: venv build` prerequisite chain is what guarantees the binaries
   and worker venv this test needs already exist, so it is never silently
   skipped). Everything real *except* the cloud: real `kahyad`, the real
   Python worker (`worker/kahya_worker`), the real pinned
   `claude-agent-sdk`/bundled `claude` CLI, and a real (throwaway, fixture)
   seeded corpus — with `anthropic_upstream_url` pointed at
   `tests/e2e/mockanthropic`'s mock `/v1/messages` server instead of the
   real API. `KAHYA_ENV=dev` + `KAHYA_ANTHROPIC_KEY_OVERRIDE` (W12-08's
   dev-only seam) substitute for a real Keychain credential, so this run
   needs neither a Keychain item nor a real `ANTHROPIC_API_KEY`. Asserts,
   as named subtests: `retrieval`, `injection_into_model_call`, `answer`,
   `single_trace_id`, `ledger_forensics`, `derived_index_property` (delete
   `brain.db`, restart, reindex, re-run retrieval — same top hit, proving
   SQLite is genuinely rebuilt from the markdown source of truth).
   `TestW12PassthroughCloudAuth` (FIX-I-1) runs the default
   `credential_mode: passthrough` the same way, with the worker CLI's own
   credential supplied by `CLAUDE_CODE_OAUTH_TOKEN=hermetic-dummy` (a
   test-only seam, never the developer's real login — a real `sk-ant-oat01-…`
   token in that variable is refused by the worker's leak check, see the
   W12-08 note; the CLI's other egress is pinned to a dead local proxy):
   the mock must see `Authorization: Bearer hermetic-dummy` and never the
   local token.

2. **`make install-agent`** (or, for a quick foreground run instead of a
   launchd-managed one, `make run-daemon` in a separate terminal) — brings
   up a REAL `kahyad` against the real `~/Kahya` corpus (W0-01) and,
   depending on `credential_mode`, a real Keychain item or an already
   logged-in Claude Code SDK session. Skip this step if a live `kahyad` is
   already running.

3. **`make accept-w12`** — the LIVE gate (`scripts/accept-w12.sh`): `kahya
   health`, an `'evlerimizden'` search against the real corpus (top-3 must
   contain a seed note containing `'ev'` — the seeded iOS home-design note,
   not this repo's hermetic fixture), the actual
   `bin/kahya "Evlerimizden ne konuşmuştuk?"` call, a trace_id check across
   `kahyad.jsonl`/`worker.jsonl`, the `hafiza_injected` ledger
   sha256 self-consistency check, and `cloud_auth` (FIX-I-1: the live
   task's trace has a `model_call` row with `status=ok`; a `task.failed`
   row, e.g. `no_upstream_credential`, is a `FAIL`). Prints one `PASS`/`FAIL`/`DEFERRED` line
   per criterion and a summary; exits nonzero on any `FAIL`. A criterion
   that needs a real Anthropic credential/Claude Code session this
   environment does not have prints `DEFERRED`, not a false `PASS` — see
   the W12-10 closing commit for this repo's own deferred run and exactly
   why.
