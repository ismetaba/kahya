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
// 6): it runs before ANY "tools/call" method is dispatched to a tool
// handler, canonicalizes the tool name (stripping an SDK-style
// "mcp__<server>__" prefix - kahyad/internal/policy.Check already does
// this internally too, so a caller that forgot to canonicalize still gets
// the right answer), and either lets the call through (policy.Check
// allowed it - currently only memory_search) or short-circuits it with a
// CallToolResult carrying IsError:true and the Turkish deny reason as its
// text content (an MCP "tool error", not a JSON-RPC protocol-level error -
// the caller/model sees exactly why the call failed). EVERY decision
// (allow or deny) is ledgered as a policy_decision event, matching
// W12-07's /policy/check endpoint's own "one ledger insert per decision"
// behavior - one interim table, two mount points, one ledgering
// convention.
//
// This middleware is installed on the *mcp.Server itself
// (AddReceivingMiddleware), which is invoked by the MCP SDK's dispatch
// loop regardless of transport - so a compromised worker POSTing
// /v1/mcp directly (bypassing kahya-mcp's bridge and can_use_tool
// entirely) is STILL denied here. can_use_tool (W12-09) is a separate,
// non-binding, worker-side early-reject layer.
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

			// W3-01: deny-all mode overrides the interim table entirely -
			// even memory_search - whenever policy.yaml failed to
			// load/validate at boot (see
			// kahyad/internal/server.Server.SetDenyAll's doc comment).
			var decision policy.Decision
			if s.denyAll {
				decision = policy.Decision{Allow: false, Reason: policy.ReasonDenyAll, Rule: policy.RuleDenyAllV1}
			} else {
				decision = policy.Check(rawName)
			}

			traceID := ""
			if callReq.Extra != nil && callReq.Extra.Header != nil {
				traceID = callReq.Extra.Header.Get(traceHeader)
			}
			if traceID == "" {
				traceID = traceid.New()
			}

			if s.eventLogger != nil {
				payload := map[string]any{
					"tool":     policy.Canonicalize(rawName),
					"raw_tool": rawName,
					"decision": allowDenyString(decision.Allow),
					"rule":     decision.Rule,
				}
				if decision.Reason != "" {
					payload["reason"] = decision.Reason
				}
				if err := s.eventLogger.LogEvent(ctx, traceID, "policy_decision", payload); err != nil {
					s.log.With(traceID).Warn("policy_decision_ledger_error", "err", err.Error())
				}
			}

			if !decision.Allow {
				s.log.With(traceID).Info("mcp_tool_denied", "tool", policy.Canonicalize(rawName), "reason", decision.Reason)
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: decision.Reason}},
				}, nil
			}
			return next(ctx, method, req)
		}
	}
}

func allowDenyString(allow bool) string {
	if allow {
		return "allow"
	}
	return "deny"
}
