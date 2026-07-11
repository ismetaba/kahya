// fs.go wires the W3-03 fs MCP tool set (kahya/mcp/fs: fs_read/fs_write/
// fs_delete) into this package: an in-process adapter satisfying
// mcp/fs.PolicyClient straight onto kahyad/internal/policy.Engine (mcp/fs
// cannot import kahyad/internal/policy directly - Go's internal-package
// import boundary, see mcp/fs's own package doc comment), the
// SetFSTool/SetUndoExpiryHook wiring main.go calls, and the undo-recipe
// dispatch handlePolicyUndo/handlePolicyFeedback (policy.go) invoke once
// kahyad/internal/policy.Engine.TriggerUndo has already flipped the
// window and demoted the ladder state.
package server

import (
	"context"

	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/policy"
	mcpfs "kahya/mcp/fs"
)

// fsOwnedTools are the tool names policyGateMiddleware (mcp.go) defers to
// mcp/fs's OWN gate chain for entirely, instead of running its own
// generic Check+ConsumeToken step first: mcp/fs's handlers must run their
// deny-glob check BEFORE any policy decision is even consulted (HANDOFF
// §5 safety #6: "Deny-glob check runs BEFORE approval flow"), which a
// middleware that always Check()s first, unconditionally, cannot express.
// fs_read is included too for symmetry/one code path (mcp/fs.Server.
// HandleRead calls PolicyClient.Check itself as well) even though it has
// no deny-glob step of its own to protect.
var fsOwnedTools = map[string]bool{
	"fs_read":   true,
	"fs_write":  true,
	"fs_delete": true,
}

// SetFSTool wires POST /v1/mcp's fs_read/fs_write/fs_delete tools (W3-03)
// and the undo-recipe dispatch handlePolicyUndo/handlePolicyFeedback use.
// Call before Prepare(); buildMCPHandler registers fsSrv's tools onto the
// SAME shared MCP server memory's tools use whenever fsSrv is non-nil.
func (s *Server) SetFSTool(fsSrv *mcpfs.Server) {
	s.fsServer = fsSrv
}

// enginePolicyClient adapts kahyad/internal/policy.Engine (Check/
// ConsumeToken) to mcp/fs.PolicyClient - the same two round trips a
// side-effectful MCP tool makes over the wire (POST /policy/check, POST
// /policy/consume-token), satisfied here with a direct in-process call
// since mcp/fs's Server lives in this SAME process today (mcp/fs's own
// package doc comment: "a real HTTP client can satisfy the EXACT same
// interface once a tool using this same call shape runs out-of-process").
// denyAll is consulted here too (mirroring policyGateMiddleware's own
// deny-all short-circuit) since fsOwnedTools bypasses that middleware
// entirely for fs_read/fs_write/fs_delete - a policy.yaml load failure at
// boot must still deny every fs operation, not just every OTHER tool.
type enginePolicyClient struct {
	engine  *policy.Engine
	denyAll func() bool
}

func (a enginePolicyClient) Check(ctx context.Context, tool, scope, taskID, traceID string, toolInput []byte) (mcpfs.PolicyDecision, error) {
	if a.denyAll() {
		return mcpfs.PolicyDecision{Result: mcpfs.PolicyResultDeny, Reason: policy.ReasonDenyAll}, nil
	}
	d, err := a.engine.Check(ctx, policy.CheckInput{
		Tool: tool, Scope: scope, TaskID: taskID, TraceID: traceID, ToolInput: toolInput,
	})
	return mcpfs.PolicyDecision{
		Result: d.Result, Reason: d.Reason, Class: string(d.Class),
		Token: d.Token, PendingApprovalID: d.PendingApprovalID,
	}, err
}

func (a enginePolicyClient) ConsumeToken(ctx context.Context, token, tool, class, scope, taskID, traceID string, toolInput []byte) error {
	return a.engine.ConsumeToken(ctx, policy.ConsumeInput{
		Token: token, Tool: tool, Class: policy.ActionClass(class), Scope: scope,
		TaskID: taskID, TraceID: traceID, ToolInput: toolInput,
	})
}

// NewFSPolicyClient constructs the mcp/fs.PolicyClient adapter main.go
// wires mcp/fs.New with. engine must not be nil; denyAll is typically
// srv.DenyAll (a *Server already in scope at wiring time).
func NewFSPolicyClient(engine *policy.Engine, denyAll func() bool) mcpfs.PolicyClient {
	return enginePolicyClient{engine: engine, denyAll: denyAll}
}

// fsLoggerAdapter wraps a *logx.Logger to satisfy mcp/fs.Logger: the only
// wrinkle is With, which *logx.Logger already implements with the exact
// same re-scope-to-a-trace_id semantics, but returns *logx.Logger rather
// than the mcp/fs.Logger interface type Go's structural typing requires
// for direct satisfaction (mcp/fs.Logger's own doc comment explains why).
type fsLoggerAdapter struct{ l *logx.Logger }

// NewFSLogger constructs the mcp/fs.Logger adapter main.go wires
// mcp/fs.New with.
func NewFSLogger(l *logx.Logger) mcpfs.Logger {
	return fsLoggerAdapter{l: l}
}

func (a fsLoggerAdapter) With(traceID string) mcpfs.Logger {
	return fsLoggerAdapter{l: a.l.With(traceID)}
}
func (a fsLoggerAdapter) Info(event string, args ...any)  { a.l.Info(event, args...) }
func (a fsLoggerAdapter) Warn(event string, args ...any)  { a.l.Warn(event, args...) }
func (a fsLoggerAdapter) Error(event string, args ...any) { a.l.Error(event, args...) }

// dispatchFSUndo invokes the owning recipe (mcp/fs.Server.UndoWrite/
// UndoDelete) for an undo_windows row whose tool this package recognizes,
// AFTER kahyad/internal/policy.Engine.TriggerUndo has already flipped that
// row to "triggered" and demoted the ladder state (this task's own
// ambiguity-decision note: demotion/window bookkeeping and recipe
// EXECUTION are two separate steps - TriggerUndo does the former
// unconditionally, this does the latter, best-effort, for the tools that
// have one implemented today). A tool this package does not recognize
// (e.g. memory_write, whose own undo recipe is a future task) or a nil
// fsServer is silently skipped - the ladder-state demotion already
// happened regardless, which is the safety-relevant half of `kahya undo`.
// Any recipe-execution error is logged but never surfaces as an error to
// the /policy/undo or /policy/feedback caller - the window/demotion
// half's success is what those endpoints report on.
func (s *Server) dispatchFSUndo(ctx context.Context, traceID, tool string) {
	if s.fsServer == nil {
		return
	}
	var err error
	switch tool {
	case "fs_write":
		err = s.fsServer.UndoWrite(ctx, traceID)
	case "fs_delete":
		err = s.fsServer.UndoDelete(ctx, traceID)
	default:
		return
	}
	if err != nil {
		s.log.With(traceID).Error("fs_undo_recipe_failed", "tool", tool, "err", err.Error())
	}
}
