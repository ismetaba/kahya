// mcp.go mounts kahyad's memory MCP server (W12-05) at POST /v1/mcp and
// implements the binding policy gate in front of it (HANDOFF §5 ⚑
// enforcement plane: "Baglayici politika karari kahyad'da verilir" - never
// in the worker's can_use_tool early-reject callback).
//
// All THREE tool handlers (memory_search/memory_write/memory_forget) live
// in kahya/mcp/memory, compiled into this binary but outside the
// kahyad/internal/* import boundary (see that package's doc comment).
// This file is the seam: it adapts this package's own concrete
// search/indexer/ledger types to the narrow interfaces mcp/memory.Server
// depends on, wraps the resulting *mcp.Server with the interim policy
// gate (kahyad/internal/policy), and mounts it via the official MCP Go
// SDK's stateless streamable-HTTP handler.
package server

import (
	"context"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/traceid"
	"kahya/mcp/memory"
)

// SetPolicyEngine wires the W3-02 autonomy-ladder decision engine that
// policyGateMiddleware (this file) and handlePolicyCheck (task.go) both
// consult. Call before Prepare(); nil is a valid (if useless) value in
// deny-all mode, where s.denyAll always short-circuits before the engine
// is ever consulted.
func (s *Server) SetPolicyEngine(e *policy.Engine) {
	s.policyEngine = e
}

// EventLogger is the append-only ledger sink this package's own MCP
// wiring needs: the policy gate's policy_decision rows (this file) and
// /v1/memory/search's hafiza_injected rows (server.go's
// handleMemorySearch) both go through it, and it is passed straight
// through as the memory.Ledger mcp/memory.Server's memory_write/
// memory_forget tools write memory_write/memory_forget events with -
// *kahyad/internal/store.Store already has exactly this method shape, so
// it satisfies both interfaces with no adapter at all.
type EventLogger interface {
	LogEvent(ctx context.Context, traceID, kind string, payload map[string]any) error
}

// traceHeader is the HTTP header kahya-mcp's bridge (kahyad/cmd/kahya-mcp)
// propagates a task's trace_id through as (mirrors mcp/memory's own
// unexported traceHeader constant - kept in sync by hand since the two
// packages intentionally don't share code across the internal boundary).
const traceHeader = "X-Kahya-Trace-Id"

// taskHeader is the HTTP header a future bridge MAY propagate a task's
// task_id through as (kahyad/cmd/kahya-mcp does not send this today - its
// own package doc comment only names KAHYA_TRACE_ID). policyGateMiddleware
// below reads it defensively (BLOCKER 1+2 fix): when present, it lets
// policy.Engine's SessionResolver resolve by the exact task_id primary key
// instead of only ever falling back to trace_id; when absent (today's
// actual bridge behavior), TaskID is simply left empty on CheckInput and
// resolution proceeds via TraceID alone - kahyad/internal/spawn sets the
// worker's KAHYA_TRACE_ID env to this exact task's own trace_id, so a
// trace_id-only resolution already finds the right tasks row.
const taskHeader = "X-Kahya-Task-Id"

// SetEventLogger wires the append-only events ledger used by (a) the
// /v1/mcp policy gate's policy_decision rows, (b) /v1/memory/search's
// for_injection=true hafiza_injected rows, and (c) - passed straight
// through - mcp/memory.Server's own memory_write/memory_forget rows. Call
// before Prepare(); every one of those ledgering paths is a best-effort
// side channel (a nil/unset logger just skips ledgering, matching
// SetSearcher/SetReindexer's "unwired dependency" posture elsewhere in
// this package) rather than failing the request it's attached to.
func (s *Server) SetEventLogger(logger EventLogger) {
	s.eventLogger = logger
}

// SetMCPMemory wires POST /v1/mcp's memory tools (W12-05): memoryDir is
// cfg.memory_dir (mcp/memory.Server confines every memory_write/
// memory_forget path argument to it), idx is the single-file incremental
// reindex hook those two tools call after touching a file on disk. Call
// before Prepare(); /v1/mcp answers 503 until this AND SetSearcher have
// both been called (memory_search reuses the same Searcher
// /v1/memory/search does - see mcpSearchAdapter).
func (s *Server) SetMCPMemory(memoryDir string, idx memory.Indexer) {
	s.mcpMemoryDir = memoryDir
	s.mcpIndexer = idx
}

// mcpSearchAdapter adapts this package's own Searcher interface (already
// wired via SetSearcher for /v1/memory/search - kahyad/internal/search.Hit
// results) to mcp/memory.Searcher (mcp/memory.Hit results), so
// memory_search reuses the EXACT SAME ranking call /v1/memory/search does
// without mcp/memory needing to import kahyad/internal/search (which it
// cannot - see that package's doc comment on the import boundary).
type mcpSearchAdapter struct {
	inner Searcher
}

func (a mcpSearchAdapter) Search(ctx context.Context, traceID, query string, k int) ([]memory.Hit, error) {
	hits, err := a.inner.Search(ctx, traceID, query, k)
	if err != nil {
		return nil, err
	}
	out := make([]memory.Hit, len(hits))
	for i, h := range hits {
		out[i] = memory.Hit{ChunkID: h.ChunkID, Path: h.Path, Seq: h.Seq, Text: h.Text, Score: h.Score, SourceTier: h.SourceTier}
	}
	return out, nil
}

// buildMCPHandler constructs the /v1/mcp route's http.Handler once (called
// from Prepare()): if the memory tools have not been fully wired
// (SetSearcher and SetMCPMemory), it answers 503 like every other
// not-yet-wired route in this package; otherwise it builds mcp/memory's
// three tools, installs the binding policy gate (policyGateMiddleware) as
// MCP receiving middleware - so it runs on EVERY tools/call before any
// tool handler executes, regardless of transport - and mounts the result
// via the pinned official MCP Go SDK's stateless, JSON-response
// streamable-HTTP handler (see the package doc for why: kahyad's /v1/mcp
// is a single-shot request/response protocol over kahya-mcp's per-message
// relay, never a long-lived SSE session, so Stateless+JSONResponse is the
// correct - and simplest correct - configuration; a stateless session
// also means a bare tools/call with no prior initialize on that "session"
// is accepted, which is exactly what the dumb bridge sends).
func (s *Server) buildMCPHandler() http.Handler {
	if s.search == nil || s.mcpIndexer == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSONError(w, http.StatusServiceUnavailable, "mcp not available")
		})
	}

	memSrv := memory.New(s.mcpMemoryDir, mcpSearchAdapter{s.search}, s.mcpIndexer, s.eventLogger, s.log)
	mcpServer := memSrv.MCPServer(nil)
	// W3-03: fs_read/fs_write/fs_delete, registered onto this SAME shared
	// server (one MCP session/tool surface for the worker) - see
	// fs.go's SetFSTool doc comment and fsOwnedTools, which
	// policyGateMiddleware below defers to entirely for these three tool
	// names.
	if s.fsServer != nil {
		s.fsServer.RegisterTools(mcpServer)
	}
	// W3-04: shell_docker/shell_host, registered onto this SAME shared
	// server - see shell.go's SetShellTool doc comment and shellOwnedTools,
	// which policyGateMiddleware below defers to entirely for these two
	// tool names (mirrors fsOwnedTools' identical bypass for fs_*).
	if s.shellServer != nil {
		s.shellServer.RegisterTools(mcpServer)
	}
	// W3-09: applescript_run/jxa_run/shortcuts_run, registered onto this
	// SAME shared server - see osascript.go's SetOsascriptTool doc comment
	// and osascriptOwnedTools, which policyGateMiddleware below defers to
	// entirely for these three tool names (mirrors fsOwnedTools'/
	// shellOwnedTools' identical bypass).
	if s.osascriptServer != nil {
		s.osascriptServer.RegisterTools(mcpServer)
	}
	// W4-07: w2_slow_stub, registered onto this SAME shared server ONLY
	// when SetDevStub has been called (main.go: only under KAHYA_ENV=dev) -
	// see devstub.go's own package doc comment. Unlike fs/shell/osascript,
	// this tool has no separate gate chain - it is NOT added to
	// fsOwnedTools/shellOwnedTools/osascriptOwnedTools, so it goes through
	// policyGateMiddleware's generic Check+ConsumeToken path below.
	if s.devStubReceipts != nil {
		s.registerDevStubTool(mcpServer)
	}
	mcpServer.AddReceivingMiddleware(s.policyGateMiddleware())

	streamable := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return mcpServer },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
	)
	return s.ensureTraceHeader(streamable)
}

// ensureTraceHeader guarantees the request reaching the MCP handler always
// carries a non-empty X-Kahya-Trace-Id header, even when the caller (or a
// bare curl in the acceptance-test sense) omitted it: withTraceLogging
// (wrapping the whole mux in Prepare) has already resolved/minted one into
// the request's context by this point (traceIDFromContext never returns
// "" after that middleware), so this just projects that SAME id onto the
// header the MCP SDK's RequestExtra.Header exposes to
// mcp/memory.traceIDFromRequest and this file's policyGateMiddleware -
// one trace_id per request, never two independently-minted ones.
func (s *Server) ensureTraceHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(traceHeader) == "" {
			r.Header.Set(traceHeader, traceIDFromContext(r))
		}
		next.ServeHTTP(w, r)
	})
}

// policyGateMiddleware is THE binding boundary (HANDOFF §5 ⚑, W12-05 step
// 6 / W3-02): it runs before ANY "tools/call" method is dispatched to a
// tool handler, canonicalizes the tool name (stripping an SDK-style
// "mcp__<server>__" prefix - kahyad/internal/policy.Engine.Check does this
// internally too, so a caller that forgot to canonicalize still gets the
// right answer), consults the W3-02 autonomy-ladder engine, and either
// lets the call through (ResultAllow - consuming the freshly-minted token
// itself, in-process, for any side-effectful class before doing so - see
// below) or short-circuits it with a CallToolResult carrying IsError:true
// and the Turkish deny/needs-approval reason as its text content (an MCP
// "tool error", not a JSON-RPC protocol-level error - the caller/model
// sees exactly why the call failed). EVERY decision is ledgered as a
// policy_decision event (kahyad/internal/policy/engine.go's own
// ledgerDecision call inside Engine.Check), matching /policy/check's
// "one ledger insert per decision" behavior - one engine, two mount
// points, one ledgering convention.
//
// BLOCKER 1+2 fix (post-security-review): this call never sets
// CheckInput.SessionID at all - /v1/mcp carries no session_id on the wire
// - which used to mean Engine.Check's old `in.SessionID != ""` guard
// silently SKIPPED the W4-03 taint check on every single /v1/mcp call,
// tainted session or not (a fail-open hole a tainted session's
// memory_write/memory_forget could walk straight through). Check now
// resolves the session identity SERVER-SIDE from TraceID (and TaskID,
// when a future bridge sends the X-Kahya-Task-Id header - see taskHeader's
// doc comment) via its own SessionResolver, so the taint check runs here
// exactly as it does on POST /policy/check - see policy.CheckInput and
// policy.SessionResolver's own doc comments for the full mechanism.
//
// Consuming the token in-process, right here, rather than requiring a
// second HTTP round-trip to POST /policy/consume-token, is deliberate:
// this middleware itself IS the "side-effectful tool"'s enforcement
// point for every tool mounted on /v1/mcp (memory_write/memory_forget
// today) - kahyad owns both the decision and the execution of these
// tools in the same process, so there is no separate out-of-process
// caller that needs the HTTP endpoint for this pair specifically (W3-03/
// 04's fs/shell tools, running as SEPARATE processes, are the ones that
// actually need to call POST /policy/consume-token over the wire).
//
// This middleware is installed on the *mcp.Server itself
// (AddReceivingMiddleware), which is invoked by the MCP SDK's dispatch
// loop regardless of transport - so a compromised worker POSTing
// /v1/mcp directly (bypassing kahya-mcp's bridge and can_use_tool
// entirely) is STILL denied here. can_use_tool (W12-09) is a separate,
// non-binding, worker-side early-reject layer.
//
// KNOWN, ACCEPTED DEFENSE-IN-DEPTH LIMITATION (W4-03 session taint):
// this middleware does NOT thread a SessionID into Engine.Check, so the
// W4-03 taint gate (Engine.Check's "tainted session => only R-class"
// hook, keyed on CheckInput.SessionID) is SKIPPED on the /v1/mcp route -
// Check treats an empty SessionID as "no session concept applies" and
// falls straight through to the ladder. This is deliberate, not an
// oversight, and specifically NOT "just not wired yet": there is no
// non-forgeable session_id available on this route to thread.
//   - No tool mounted here carries a session_id argument in its schema
//     (memory_search/write/forget - mcp/memory; fs_* - mcp/fs; shell_* -
//     mcp/shell; applescript/jxa/shortcuts - mcp/osascript), and the only
//     per-request identity kahya-mcp's bridge propagates is the trace_id
//     header (X-Kahya-Trace-Id), sourced from the worker's own
//     KAHYA_TRACE_ID env - i.e. worker-supplied.
//   - The adversary this middleware exists to stop is a COMPROMISED
//     worker (RCE-level, not a merely prompt-injected model) crafting
//     arbitrary POSTs to the shared control socket. ANY session identity
//     that adversary supplies - a tool arg, or a header the bridge/worker
//     sets - is attacker-chosen: it would omit it (taint skipped) or name
//     a known-clean session (taint passes). A worker-supplied session_id
//     here therefore buys no security against this adversary; wiring one
//     would be security theater that reads as protection it does not give.
//   - Taint fundamentally defends against a prompt-injected MODEL, whose
//     tool calls flow through the ORDINARY path (can_use_tool -> POST
//     /policy/check -> handlePolicyCheck), which DOES pass the worker's own
//     session_id into this same Engine.Check taint hook - that is where the
//     HANDOFF §5 safety #2 invariant binds. This middleware backstops a
//     strictly stronger adversary, for whom taint keyed on a forgeable
//     session adds nothing, but for whom the controls kahyad evaluates
//     authoritatively (from the tool name + its own autonomy_state + the
//     call's args, never from any worker-supplied session/trace value) DO
//     still bind: class W3 never auto-allows and always needs written LOCAL
//     approval (Check -> needs_approval -> deny() here), so mail_send etc.
//     stays unreachable taint or no taint; the autonomy ladder still gates
//     every W1/W2 call by kahyad-side state, so only user-promoted tools
//     auto-allow; fs_*/shell_*/osascript_* run their OWN deny-glob/
//     digest-pin/egress/arg-validator chain (the fsOwnedTools/shellOwnedTools/
//     osascriptOwnedTools bypass above) regardless - and share this EXACT
//     same no-session_id limitation via enginePolicyClient (fs.go), for the
//     same reason; deny-all still fails closed at boot; every decision is
//     ledgered; an auto-allowed W1 still opens a 5-minute undo window.
//   - The residual risk taint would otherwise cover on this path is thus
//     narrow: an RCE-compromised worker, whose session is tainted, using
//     this bypass (not the ordinary path) to invoke a W1/W2 tool the user
//     ALREADY promoted to auto-allow - which is still ledgered and (for W1)
//     undoable.
//   - A SOUND fix needs an UNFORGEABLE binding between the incoming /v1/mcp
//     request and a specific session: a dedicated authenticated per-task
//     channel, or a kahyad-minted per-task secret this middleware MANDATES
//     (deny on absence) and resolves server-side to that task's captured
//     session_id. All workers share one control socket today with no
//     per-connection identity, so that is a larger, separate change, tracked
//     in tasks/w4-durability/W4-03-taint-reader-actor.md's "Deviations".
//     TestMCPGateTaintCheckSkippedNoSessionOnThisPath (mcp_test.go) pins
//     this documented behavior.
func (s *Server) policyGateMiddleware() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/call" {
				return next(ctx, method, req)
			}
			callReq, ok := req.(*mcp.CallToolRequest)
			if !ok || callReq.Params == nil {
				return next(ctx, method, req)
			}

			rawName := callReq.Params.Name
			canonName := policy.Canonicalize(rawName)

			traceID := ""
			taskID := ""
			if callReq.Extra != nil && callReq.Extra.Header != nil {
				traceID = callReq.Extra.Header.Get(traceHeader)
				// BLOCKER 1+2 fix: propagated when a future bridge sends
				// it (taskHeader's own doc comment - kahya-mcp does not
				// today); left "" otherwise, so policy.Engine's
				// SessionResolver falls back to resolving by traceID
				// alone, which is already sufficient (kahyad/internal/
				// spawn sets the worker's KAHYA_TRACE_ID env to this
				// exact task's own trace_id).
				taskID = callReq.Extra.Header.Get(taskHeader)
			}
			if traceID == "" {
				traceID = traceid.New()
			}
			deny := func(reason string) (mcp.Result, error) {
				s.log.With(traceID).Info("mcp_tool_denied", "tool", canonName, "reason", reason)
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: reason}}}, nil
			}

			// W3-03/W3-04: fs_read/fs_write/fs_delete and shell_docker/
			// shell_host each run their OWN full gate chain (deny-glob/
			// arg-validator/digest-pin/network checks BEFORE any policy
			// decision, then Check, then ConsumeToken - see mcp/fs's and
			// mcp/shell's package doc comments and fs.go's fsOwnedTools /
			// shell.go's shellOwnedTools) instead of this middleware's
			// generic Check+auto-consume step: a middleware that always
			// Check()s first, unconditionally, cannot express "a
			// fs_write_deny_globs hit denies immediately, with no
			// approval able to override it, before the ladder is ever
			// consulted". This bypass runs BEFORE the deny-all check
			// below too - mcp/fs's own PolicyClient adapter
			// (enginePolicyClient in fs.go), reused directly by mcp/shell
			// (shell.go's doc comment), independently re-checks s.denyAll
			// and fails closed identically, so a policy.yaml load failure
			// at boot still denies every fs/shell operation.
			if fsOwnedTools[canonName] || shellOwnedTools[canonName] || osascriptOwnedTools[canonName] {
				return next(ctx, method, req)
			}

			// W3-01: deny-all mode overrides the ladder engine entirely - even
			// memory_search - whenever policy.yaml failed to load/validate at
			// boot (see kahyad/internal/server.Server.SetDenyAll's doc
			// comment). This is ledgered by hand (rather than through
			// Engine.Check, which is never called in this mode) so a
			// deny-all decision stays visible in the same policy_decision
			// ledger stream.
			if s.denyAll {
				if s.eventLogger != nil {
					payload := map[string]any{
						"event": "policy_decision", "tool": canonName, "raw_tool": rawName,
						"decision": "deny", "rule": policy.RuleDenyAllV1, "reason": policy.ReasonDenyAll,
					}
					if err := s.eventLogger.LogEvent(ctx, traceID, "policy_decision", payload); err != nil {
						s.log.With(traceID).Warn("policy_decision_ledger_error", "err", err.Error())
					}
				}
				return deny(policy.ReasonDenyAll)
			}
			if s.policyEngine == nil {
				s.log.With(traceID).Error("mcp_policy_engine_unavailable", "tool", canonName)
				return deny(policy.ReasonPolicyStateError)
			}

			var argBytes []byte
			if callReq.Params.Arguments != nil {
				argBytes = []byte(callReq.Params.Arguments)
			}
			decision, _ := s.policyEngine.Check(ctx, policy.CheckInput{
				Tool: rawName, TraceID: traceID, TaskID: taskID, ToolInput: argBytes,
			})
			if decision.Result != policy.ResultAllow {
				return deny(decision.Reason)
			}

			// Side-effectful classes (W1/W2 - never R, never W3, which never
			// reaches ResultAllow at all) must present the freshly-minted
			// token back before executing (HANDOFF §5 enforcement plane).
			//
			// W4-07 fix (defect the acceptance gate surfaced): this call used
			// to omit TaskID entirely. Engine.ConsumeToken compares the
			// token's REAL bound task_id (row.TaskID, recovered from
			// approval_tokens - set at mint time from Check's OWN
			// CheckInput.TaskID, taskID's own value from the SAME request)
			// against ConsumeInput.TaskID and fails closed
			// (ErrTokenInvalid/"context_mismatch") on any mismatch. As long as
			// taskID was ALWAYS "" on both sides (kahya-mcp never forwarded
			// X-Kahya-Task-Id - see taskHeader's own doc comment history),
			// "" == "" always matched, silently masking this omission. Once
			// the bridge started forwarding a REAL task_id (W4-07's own fix,
			// needed for w2_slow_stub/Receipts.Execute to key tool_calls
			// correctly), row.TaskID became non-empty while this call's
			// in.TaskID stayed "" - a guaranteed mismatch that denied EVERY
			// side-effectful /v1/mcp call (memory_write/memory_forget
			// included, not just the new dev-only tool). Passing taskID here
			// closes that gap.
			if decision.Class != policy.ClassR {
				if err := s.policyEngine.ConsumeToken(ctx, policy.ConsumeInput{
					Token: decision.Token, Tool: canonName, Class: decision.Class, Scope: decision.Scope,
					TaskID: taskID, TraceID: traceID, ToolInput: argBytes,
				}); err != nil {
					s.log.With(traceID).Error("mcp_consume_token_failed", "tool", canonName, "err", err.Error())
					return deny(policy.ReasonPolicyStateError)
				}
			}
			return next(ctx, method, req)
		}
	}
}
