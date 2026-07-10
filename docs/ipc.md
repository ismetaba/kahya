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
container), kahyad sets exactly six variables (`spawn.BuildEnv`):

| Variable | Value |
|---|---|
| `KAHYA_TASK_ID` | The envelope's `task_id`. |
| `KAHYA_TRACE_ID` | The envelope's `trace_id`. |
| `KAHYA_SOCKET` | `cfg.socket` — kahyad's own control socket, so the worker's `can_use_tool` hook (W12-09) can reach `POST /policy/check` over HTTP-over-UDS. |
| `KAHYA_LOG_DIR` | `cfg.log_dir` — the worker writes its own JSONL logs here (every process logs JSONL with `trace_id` on every line, HANDOFF §4 ⚑). |
| `ANTHROPIC_BASE_URL` | **TODO(W12-08):** until the forward-proxy lands, this is set directly from `cfg.anthropic_upstream_url` (`https://api.anthropic.com` by default) — i.e. the worker currently talks to the real Anthropic API directly if it were to use this var. From W12-08 on, this instead points at kahyad's own localhost forward-proxy listener, and the cost governor / cache-hit metric / egress gate are enforced at that proxy. |
| `ANTHROPIC_API_KEY` | A **per-task random token**, `"kahya-task-" + 32 lowercase hex chars` (`spawn.NewAPIKey`) — **NOT a real Anthropic key**. The real key never leaves kahyad (HANDOFF §4 IPC ⚑: "API anahtarı worker'a verilmez"). W12-08's per-task forward-proxy listener will reject any inbound request whose key does not match this exact token, so no other local process can spend through kahyad's real key by guessing `ANTHROPIC_API_KEY`. Until W12-08 lands, nothing actually checks this value. |

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
(`kahyad/internal/policy.Canonicalize`).

Response:

```json
{"decision": "allow", "rule": "interim-static-v1"}
```

or

```json
{"decision": "deny", "reason": "<Turkish>", "rule": "interim-static-v1"}
```

**Interim static table** (`kahyad/internal/policy`, package doc: "one
table, two mount points, never a second copy" — this SAME table also
gates `POST /v1/mcp`'s `tools/call` dispatch, W12-05). Binding until
W3-01/W3-02 replace it with the real `policy.yaml` + autonomy ladder:

- **Allow, exactly:** `memory_search`.
- **Deny** (reason `"W3 politika altyapısı gelene dek yalnız hafıza araması (memory_search) açık."`): `memory_write`, `memory_forget`, and every SDK built-in R-class tool — `Read`, `Glob`, `Grep` — plus `Bash`, `WebFetch`, `WebSearch`, `Write`, `Edit`. `Read`/`Glob`/`Grep` are denied *even though R-class actions are nominally auto-approved* per the §4 autonomy ladder, because no secret-lane classification exists before W3-01's `policy.yaml` globs land — HANDOFF §4's ordering invariant ("hiçbir bayt, gizli-şerit sınıflandırması yerel/deterministik olarak tamamlanmadan bulut modele gitmez") means an allowed file read could put unclassified bytes in front of a cloud model with no classification having happened. `memory_search`'s corpus is the user-reviewed seed, which is a designed exception.
- **Deny, unknown tool** (reason `"Tanınmayan araç reddedildi (fail-closed)."`): any tool name not in the table at all — a distinct reason from the known-deny case, so a typo'd or future tool name is visibly distinguishable in logs/ledger.
- **Malformed request body, or one over the 1 MiB cap above, ⇒ HTTP 400 or
  413**, but the body still says `{"decision":"deny",...}` (`rule` still
  `"interim-static-v1"`, `reason` `"Geçersiz istek gövdesi
  (fail-closed)."`) — so a sloppy client cannot parse an "allow" out of a
  transport-level error. A best-effort `policy_decision` ledger row IS
  still written for this case, under the `trace_id` `withTraceLogging`
  already resolved from the `X-Kahya-Trace-Id` header (independent of the
  unparseable/oversized body itself) — fail-closed applies to the ledger
  too, so evidence that a deny happened is never silently dropped just
  because the body couldn't be parsed.

**Ledger:** every well-formed decision writes exactly one
`kind='policy_decision'` event, payload = the full request (`trace_id`,
`task_id`, `session_id`, `tool_name`, `tool_input`) plus the decision
(`decision`, `rule`, and `reason` when denied) — under the request's own
`trace_id`.

## 7 · Cross-references

- SSE contract, CLI-side: `kahyad/cmd/kahya/client.go` (`StreamTask`,
  `readSSE`), frozen in W12-06's task file.
- Interim policy table + `/v1/mcp` gate: `kahyad/internal/policy`,
  `kahyad/internal/server/mcp.go` (W12-05).
- `tasks`/`events` schema: `kahyad/migrations/0001_init_schema.sql`
  (W12-02).

## 8 · Out of scope / what changes later

- **Real Python worker** (`ClaudeSDKClient`, streaming input mode,
  `UserPromptSubmit` hook, `can_use_tool` client side) — W12-09. Every
  test in this repo against this contract uses a fake worker script under
  `kahyad/internal/spawn/testdata/`.
- **Forward-proxy + real `ANTHROPIC_BASE_URL`** — W12-08. Until then,
  `ANTHROPIC_BASE_URL` is the real upstream URL and `ANTHROPIC_API_KEY` is
  a token nothing yet validates.
- **Real policy** (`policy.yaml`, autonomy ladder, one-time approval
  tokens, W1 5-minute undo) — W3-01/W3-02. This document's §6 table is
  explicitly interim and will be replaced; the endpoint's request/response
  *shape* is what's frozen, not today's allow/deny table.
- **Session resume, receipts, retries, outbox dispatch** — W4-02.
- **Taint checks in policy decisions** — W4-03.
- **Intent router / dynamic model routing** — W4-08. W1-2's `model` is
  always the static `cfg.default_model`.
