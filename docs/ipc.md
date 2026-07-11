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
pool. The process is started in its **own process group**
(`setpgid`, new group id = the child's own pid) so a timeout, or a future
`⌥⎋` halt (W6-03), can kill the whole tree — including any grandchild
processes the worker itself spawns — with a single `kill(-pgid, SIGKILL)`.

The worker's stdin receives exactly one JSON object (the envelope, §2
below), after which **stdin is closed**. The worker's stdout is a JSONL
stream (§4 below), relayed live — not buffered until exit. The worker's
stderr is diagnostics only, logged by kahyad at `warn`; it is never shown
to the user and never affects task outcome.

kahyad has no timeout policy of its own inside the spawn layer — the
caller (the `/v1/task` handler) derives a `context.Context` with a
deadline of `cfg.task_timeout_min` minutes and passes it in. When that
context is done before the process exits on its own, kahyad SIGKILLs the
whole process group and waits for it to be fully reaped before continuing
— no orphan process ever survives a task, timed out or not.

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
  "created_at": "2026-07-10T12:00:00Z"
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
| `model` | Must be one of the HANDOFF §9 cloud set: `claude-opus-4-8`, `claude-sonnet-5`, `claude-haiku-4-5`, `claude-fable-5` (`spawn.AllowedModels`). W1-2 always uses `cfg.default_model` (static); the full intent router (task-type → model) is W4-08 — the routing **decision** is always Go's, never the prompt's. |
| `memory_injection` | Always `true` in W1-2. The `<hafiza>` block itself is rendered by `/v1/memory/search?for_injection=true` (W12-05); the worker's `UserPromptSubmit` hook that actually calls it is W12-09. |
| `created_at` | Plain RFC3339 (`time.RFC3339`, e.g. `2026-07-10T12:00:00Z`, no fractional seconds), UTC. |

## 3 · Worker environment

In addition to inheriting kahyad's own process environment (`PATH`, `HOME`,
etc. — W1-2's worker is a plain subprocess, not a network-isolated
container), kahyad sets exactly eight variables (`spawn.BuildEnv`):

| Variable | Value |
|---|---|
| `KAHYA_TASK_ID` | The envelope's `task_id`. |
| `KAHYA_TRACE_ID` | The envelope's `trace_id`. |
| `KAHYA_SOCKET` | `cfg.socket` — kahyad's own control socket, so the worker's `can_use_tool` hook (W12-09) can reach `POST /policy/check` over HTTP-over-UDS. |
| `KAHYA_LOG_DIR` | `cfg.log_dir` — the worker writes its own JSONL logs here (every process logs JSONL with `trace_id` on every line, HANDOFF §4 ⚑). |
| `ANTHROPIC_BASE_URL` | Since W12-08: `http://127.0.0.1:<ephemeral-port>` — the per-task `kahyad/internal/anthproxy.Proxy` listener kahyad opens (`127.0.0.1:0`) immediately before spawning this worker and closes immediately after it exits. The cost governor, cache-hit metric, and the `egressGate` hook (nil/always-allow until W3-05) are enforced at that proxy, never in this package. |
| `ANTHROPIC_API_KEY` | A **per-task random token**, `"kahya-task-" + 32 lowercase hex chars` (`spawn.NewAPIKey`) — **NOT a real Anthropic key**. The real key never leaves kahyad (HANDOFF §4 IPC ⚑: "API anahtarı worker'a verilmez"). Since W12-08, the per-task forward-proxy listener rejects any inbound request whose `x-api-key`/`Authorization` does not match this exact token (`401` + ledger `proxy_auth_reject`), so no other local process can spend through kahyad's real key by guessing `ANTHROPIC_API_KEY`. |
| `KAHYA_MCP_BRIDGE` | Since W12-09: `cfg.mcp_bridge_path` — the absolute path to the `kahya-mcp` stdio↔UDS bridge binary (`bin/kahya-mcp`, W12-05). The worker execs this as its `"kahya_memory"` MCP server's `stdio` `command` (`ClaudeAgentOptions.mcp_servers`) — it never hardcodes or otherwise discovers this path itself. |
| `KAHYA_CREDENTIAL_MODE` | Since W12-09: `cfg.credential_mode` (`"keychain"` or `"passthrough"`, mirroring `anthproxy`'s own `CredentialMode` — see the W12-08 note below). The worker reads this to decide which startup env assertion applies to `ANTHROPIC_API_KEY` (`kahya_worker.__main__`'s step-6 check): required to match the per-task token shape in `"keychain"` mode; not enforced as the worker's own auth in `"passthrough"` mode, since the SDK subprocess authenticates via its own forwarded credential instead. Defaults to `"keychain"` if unset (belt-and-braces: the stricter posture is default-safe). |

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
- `passthrough` (**default**, the owner-decision mode) — after validating
  the inbound per-task local token (`ANTHROPIC_API_KEY` above — this still
  fully upholds "API anahtarı worker'a verilmez": no real Anthropic
  credential ever reaches the worker either way), strip only the header
  that carried that local token and forward any OTHER auth header the
  worker's own HTTP client attached completely unchanged (its Claude Code
  SDK session credential — the proxy never inspects, replaces, or logs
  it).

Everything else this task specifies (cost governor, cache-hit metric,
egress-gate hook, usage/pricing) is auth-agnostic and built exactly per
the frozen contract above; only credential injection differs by mode. The
one deferred item is a live check with a real Keychain credential/session
(no CI test exercises a real key or session) — see the W12-08 task file
and its closing commit for the explicit deferral note.

**Post-review addition (BLOCKER 2 fix)** — `Governor.CheckBeforeForward`
is now an atomic check-and-reserve, not a plain check-then-act: it
reserves a conservative (fail-closed = over-, never under-estimated)
token/USD estimate for the about-to-be-forwarded request before it is
sent, and `RecordUsage` releases that reservation once the real usage is
known. When the request's own `max_tokens`/body size can't be parsed, the
estimate falls back to the new `est_request_tokens` config key (committed
default `50000`) — see `kahyad/internal/anthproxy/governor.go`'s
`estimateRequestLocked` for the full estimation strategy.

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

**Timeout:** if `cfg.task_timeout_min` elapses before the process sends a
terminal line, kahyad kills the whole process group and ends the stream
with `event: error`, message
`"Görev zaman aşımına uğradı (%d dk)."` (minutes substituted in);
`tasks.state` → `error`; ledger `task_timeout`.

## 5 · `POST /v1/task` (HTTP-over-UDS, `~/Library/Application Support/Kahya/kahyad.sock`)

Request body (matches `kahyad/cmd/kahya/client.go`'s `StreamTask`, W12-06):

```json
{"prompt": "test sorusu", "trace_id": "3f9a6b2c1d4e5f60718293a4b5c6d7e8"}
```

`trace_id` is optional in the body — if absent, kahyad falls back to the
`X-Kahya-Trace-Id` request header, or mints a fresh one if that is also
absent. An empty/whitespace-only `prompt` is rejected locally with `400`
before a task is minted at all.

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
  full envelope JSON, before the SSE response even starts.
- Ledger `task_spawned` right after the insert.
- `UpdateTaskSession` — as the worker's `"session"` line arrives, if any.
- `UpdateTaskState` — `done` or `error` once the terminal outcome is known.
- Ledger `task_done` / `task_error` / `task_timeout` to match.

All of the above tasks-table/ledger writes after the worker starts use the
HTTP request's own context (not the per-task timeout context) so that a
timeout — which cancels the timeout context by design — never prevents
kahyad from recording that the task timed out.

## 6 · `POST /policy/check` (HTTP-over-UDS, same socket)

The binding, fail-closed policy decision point (HANDOFF §4/§5 ⚑:
`can_use_tool` inside the worker is an early-reject/UX layer only, never
the security boundary — the worker's `can_use_tool` hook (W12-09) calls
this endpoint for every tool-use attempt, but the endpoint itself, running
inside kahyad, is what's binding). **Caller timeout budget: 5 seconds;
any error or timeout on the caller's side must be treated as `deny`
(fail-closed)** — the endpoint itself does no I/O beyond one ledger
insert, so it always answers in well under that budget.

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
  "tool_input": {}
}
```

`tool_name` is canonicalized before lookup: an SDK-style
`mcp__<server>__<tool>` prefix (e.g.
`mcp__kahya_memory__memory_search`) is stripped down to the bare tool name
(`memory_search`) — built-in tools arrive bare already
(`kahyad/internal/policy.Canonicalize`). `scope` is optional (defaults to
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
`decision`, `task_id` (and `reason` when not `allow`) — under the
request's own `trace_id`. See `kahyad/internal/policy/README.md` for the
full wire schema of `/policy/consume-token`, `/policy/feedback`,
`/policy/state`, `/policy/promote`, and `/policy/undo`.

## 7 · Cross-references

- SSE contract, CLI-side: `kahyad/cmd/kahya/client.go` (`StreamTask`,
  `readSSE`), frozen in W12-06's task file.
- Autonomy-ladder engine + `/v1/mcp` gate: `kahyad/internal/policy`
  (`engine.go`, `tokens.go`, `README.md`), `kahyad/internal/server/mcp.go`
  (W12-05/W3-02).
- `tasks`/`events` schema: `kahyad/migrations/0001_init_schema.sql`
  (W12-02); `autonomy_state`/`approval_tokens`/`undo_windows`:
  `kahyad/migrations/0003_autonomy_policy.sql` (W3-02).

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
  the note above); the model-call `egressGate` hook itself is still a
  nil/always-allow stub until W3-05 fills in the real allowlist, and the
  "→yerel" downgrade rung stays unavailable (ledgered as
  `budget_downgrade_unavailable`) until W3-08's local lane lands.
- **Approval surface rendering** (Telegram inline buttons, CLI "onayla" +
  byte-exact WYSIWYE diff, Hammerspoon cards) — W3-06/W3-07/W6-01. W3-02
  only exposes the engine API those surfaces call
  (`POST /policy/feedback`'s `approve`/`deny`, and the `pending_approval_id`
  a decision returns); until then, an approval can only be driven by hand
  (`curl`/tests) or via `kahya autonomy promote`.
- **Undo recipe implementations** (Trash restore, git checkpoint restore,
  ...) — W3-03. `kahya undo --trace <id>` / `POST /policy/undo` only
  trigger the window + demotion; executing the actual recipe is the owning
  tool's job.
- **Session resume, receipts, retries, outbox dispatch** — W4-02.
- **Taint checks in policy decisions** — W4-03.
- **Intent router / dynamic model routing** — W4-08. W1-2's `model` is
  always the static `cfg.default_model`.

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
   `kahyad.jsonl`/`worker.jsonl`, and the `hafiza_injected` ledger
   sha256 self-consistency check. Prints one `PASS`/`FAIL`/`DEFERRED` line
   per criterion and a summary; exits nonzero on any `FAIL`. A criterion
   that needs a real Anthropic credential/Claude Code session this
   environment does not have prints `DEFERRED`, not a false `PASS` — see
   the W12-10 closing commit for this repo's own deferred run and exactly
   why.
