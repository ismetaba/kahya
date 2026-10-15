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
	"errors"
	"fmt"

	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/secretlane"
	"kahya/kahyad/internal/store/sqlcgen"
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
	// check (B-1) is Server.checkWithApprovalWait when built via
	// Server.FSPolicyClient (production): a needs_approval parks the tool
	// call until the human decides and an approval is redeemed. nil
	// (NewFSPolicyClient - tests) means a direct engine.Check, no waiting.
	check func(ctx context.Context, in policy.CheckInput) (policy.Decision, error)
}

// FSPolicyClient (B-1) is the production mcp/fs.PolicyClient (also reused
// by mcp/shell and mcp/osascript): the SAME adapter NewFSPolicyClient
// builds, routed through checkWithApprovalWait so an fs/shell/osascript
// call that needs approval waits for it and then executes exactly the
// approved bytes. Call after SetPolicyEngine.
func (s *Server) FSPolicyClient() mcpfs.PolicyClient {
	return enginePolicyClient{engine: s.policyEngine, denyAll: s.DenyAll, check: s.checkWithApprovalWait}
}

func (a enginePolicyClient) Check(ctx context.Context, tool, scope, taskID, traceID string, toolInput []byte) (mcpfs.PolicyDecision, error) {
	if a.denyAll() {
		return mcpfs.PolicyDecision{Result: mcpfs.PolicyResultDeny, Reason: policy.ReasonDenyAll}, nil
	}
	in := policy.CheckInput{Tool: tool, Scope: scope, TaskID: taskID, TraceID: traceID, ToolInput: toolInput}
	var d policy.Decision
	var err error
	if a.check != nil {
		d, err = a.check(ctx, in)
	} else {
		d, err = a.engine.Check(ctx, in)
	}
	return mcpfs.PolicyDecision{
		Result: d.Result, Reason: d.Reason, Class: string(d.Class),
		Token: d.Token, PendingApprovalID: d.PendingApprovalID,
	}, err
}

// ConsumeToken returns the consumed token's op id (B-8: policy.
// ConsumeResult.OpID, the approval_tokens.token_hash) - mcp/fs keys its
// per-operation undo record by it; for a W1 token the engine has already
// opened that operation's undo window.
func (a enginePolicyClient) ConsumeToken(ctx context.Context, token, tool, class, scope, taskID, traceID string, toolInput []byte) (string, error) {
	res, err := a.engine.ConsumeTokenOp(ctx, policy.ConsumeInput{
		Token: token, Tool: tool, Class: policy.ActionClass(class), Scope: scope,
		TaskID: taskID, TraceID: traceID, ToolInput: toolInput,
	})
	if err != nil {
		return "", err
	}
	return res.OpID, nil
}

// NewFSPolicyClient constructs the plain (non-waiting) mcp/fs.PolicyClient
// adapter - tests use it; production wires Server.FSPolicyClient (B-1),
// which adds the approval wait. engine must not be nil; denyAll is
// typically srv.DenyAll.
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

// fsContentClassifier adapts kahyad/internal/secretlane.ClassifyDeterministic
// to mcp/fs.ContentClassifier (W3-10 gate-test fix — see mcp/fs.Server.
// ContentClassifier's own doc comment). Deterministic-only, on purpose: no
// live Qwen/MLX dependency for this ingest point, matching
// kahyad/internal/server/task.go's own POST /v1/task prompt-classification
// call, which uses the exact same function.
type fsContentClassifier struct{}

func (fsContentClassifier) ClassifySecretLane(text string) (bool, string) {
	v := secretlane.ClassifyDeterministic(text)
	return v.SecretLane, v.Category
}

// NewFSContentClassifier constructs the mcp/fs.ContentClassifier adapter
// main.go wires fsTool.ContentClassifier with.
func NewFSContentClassifier() mcpfs.ContentClassifier {
	return fsContentClassifier{}
}

// Ledger kinds the lane escalators write (FIX-B-6 fs_read, FIX-U-3
// memory_write; English, internal). EventSecretLaneEscalated: a task's
// persisted lane was (re)written secret because its fs_read hit, or its
// memory_write wrote, secret-lane content (payload "source" says which).
// EventSecretLaneEscalateNoTask: a secret-lane hit with no task to escalate
// - only an unattributed caller (the CLI, a script, a test harness - never
// a spawned worker) can produce it, and the sensitive-read mark has
// already landed.
const (
	EventSecretLaneEscalated      = "secret_lane_escalated"
	EventSecretLaneEscalateNoTask = "secret_lane_escalate_no_task"
)

// Lane-escalation sources (the ledger payload "source" and the
// secretlane.Verdict.Reason the escalation persists).
const (
	laneSourceFSRead      = "fs_read"
	laneSourceMemoryWrite = "memory_write"
)

// laneEscalator adapts kahyad/internal/secretlane.EscalateVerdict to
// mcp/fs.LaneEscalator (FIX-B-6) and mcp/memory.LaneEscalator (FIX-U-3) -
// one implementation, source naming the ingestion point.
type laneEscalator struct {
	lanes  *SecretLaneStoreAdapter
	ledger EventLogger // may be nil
	source string
}

// NewFSLaneEscalator constructs the mcp/fs.LaneEscalator adapter main.go
// wires fsTool.LaneEscalator with: q is *store.Store.Queries, ledger the
// store itself.
func NewFSLaneEscalator(q TaskLaneQueries, ledger EventLogger) mcpfs.LaneEscalator {
	return laneEscalator{lanes: NewSecretLaneStoreAdapter(q), ledger: ledger, source: laneSourceFSRead}
}

// EscalateSecretLane escalates the CALLING task's persisted lane to secret.
// Which task that is comes from the caller's UDS peer attribution (G03 /
// B-14, G00 C-9 - callerAttributionFromContext, the same identity
// pinBodyIdentity pins body ids to), never from a declaration: for a live
// worker taskID is already the pinned header, and should a direct in-process
// caller hand in anything else, the attributed task still wins. Only an
// unattributed caller - which withControlAuth serves on /v1/mcp only with
// the control secret - keeps its declared taskID (no trace_id lookup: a
// spawned worker is always attributable, so there is nothing to guess).
//
// Fail-closed: an attributed live worker whose tasks row cannot be found,
// or any read/write error, is an error - mcp/fs then refuses the read and
// mcp/memory the write. An unattributed caller with no task id, or one
// naming a task that does not exist, has no task proxy for the bytes to
// leave through: ledgered as EventSecretLaneEscalateNoTask, nil.
func (e laneEscalator) EscalateSecretLane(ctx context.Context, taskID, traceID, category string) error {
	attribution := "declared"
	if att, ok := callerAttributionFromContext(ctx); ok && att.TaskID != "" {
		taskID, attribution = att.TaskID, "live_worker"
	}
	if taskID == "" {
		e.ledgerRaw(ctx, traceID, EventSecretLaneEscalateNoTask, map[string]any{
			"event": EventSecretLaneEscalateNoTask, "category": category, "reason": "no_task_id", "source": e.source,
		})
		return nil
	}
	_, _, found, err := e.lanes.GetTaskLane(ctx, taskID)
	if err != nil {
		return fmt.Errorf("%s lane escalate: read lane for task %s: %w", e.source, taskID, err)
	}
	if !found {
		if attribution == "live_worker" {
			return fmt.Errorf("%s lane escalate: live worker's task %s has no tasks row", e.source, taskID)
		}
		e.ledgerRaw(ctx, traceID, EventSecretLaneEscalateNoTask, map[string]any{
			"event": EventSecretLaneEscalateNoTask, "task_id": taskID, "category": category, "reason": "no_task_row", "source": e.source,
		})
		return nil
	}
	lane, cat, err := secretlane.EscalateVerdict(ctx, e.lanes, taskID, traceID,
		secretlane.Verdict{SecretLane: true, Category: category, Reason: e.source}, nil)
	if err != nil {
		return err
	}
	e.ledgerRaw(ctx, traceID, EventSecretLaneEscalated, map[string]any{
		"event": EventSecretLaneEscalated, "task_id": taskID, "lane": lane, "category": cat,
		"source": e.source, "attribution": attribution,
	})
	return nil
}

func (e laneEscalator) ledgerRaw(ctx context.Context, traceID, kind string, payload map[string]any) {
	if e.ledger == nil {
		return
	}
	_ = e.ledger.LogEvent(ctx, traceID, kind, payload)
}

func (a fsLoggerAdapter) With(traceID string) mcpfs.Logger {
	return fsLoggerAdapter{l: a.l.With(traceID)}
}
func (a fsLoggerAdapter) Info(event string, args ...any)  { a.l.Info(event, args...) }
func (a fsLoggerAdapter) Warn(event string, args ...any)  { a.l.Warn(event, args...) }
func (a fsLoggerAdapter) Error(event string, args ...any) { a.l.Error(event, args...) }

// undoRecipe* are the per-operation outcomes policyUndoOp.Recipe carries
// (policy.go, B-8).
const (
	undoRecipeExecuted    = "executed"    // recipe ran, bytes restored
	undoRecipeFailed      = "failed"      // recipe ran and errored (logged + surfaced)
	undoRecipeNoRecord    = "no_record"   // window exists but the tool has no record (kahyad restarted mid-window, the op never completed, or a pre-0022 row)
	undoRecipeUnsupported = "unsupported" // tool has no undo recipe (memory_write/memory_forget today) - the ladder demotion still applied
)

// dispatchUndoRows executes the owning recipe of every row
// Engine.TriggerUndo triggered, in exactly that order (newest first - B-8
// LIFO), as ONE fail-closed chain (B-8 review, mcp/fs.UndoChain): once an
// fs operation's recipe fails on a path, no older operation on that path
// runs in this call - it is reported failed (mcp/fs.ErrUndoBlocked) and the
// CLI exits 1 (G05-3) - so a refused newer recipe can never be bypassed by
// an older one writing its pre-image over bytes it did not produce. Every
// fs recipe additionally checks the target is still exactly what its own
// operation left (mcp/fs.ErrTargetChanged), which also covers a newer row
// TriggerUndo could not flip (it is not in rows at all). POST /policy/undo
// and POST /policy/feedback kind=undo both dispatch through here.
func (s *Server) dispatchUndoRows(ctx context.Context, traceID string, rows []sqlcgen.UndoWindow) []policyUndoOp {
	var chain *mcpfs.UndoChain
	if fsSrv := s.fsServer; fsSrv != nil {
		chain = fsSrv.NewUndoChain()
	}
	ops := make([]policyUndoOp, 0, len(rows))
	for _, row := range rows {
		ops = append(ops, s.dispatchFSUndo(ctx, traceID, row, chain))
	}
	return ops
}

// dispatchFSUndo executes the owning recipe (mcp/fs.UndoChain.UndoOp) for
// ONE triggered undo_windows row (B-8: one call per row Engine.TriggerUndo
// returned, newest first, all through dispatchUndoRows' one chain), AFTER
// TriggerUndo has already flipped it to "triggered" and demoted the ladder
// state (W3-03's ambiguity-decision note: demotion/window bookkeeping and
// recipe EXECUTION are two separate steps). A tool this package has no
// recipe for (memory_write, whose own undo recipe is a future task) or a
// nil fs tool (chain) reports "unsupported" - the demotion already happened
// regardless, which is the safety-relevant half of `kahya undo`. A missing
// record is "no_record" and is never guessed at (fail-closed: nothing is
// restored). Never returns an error: the window/demotion half is what
// /policy/undo answers ok on; the recipe outcome is surfaced per op in the
// response and logged under traceID.
func (s *Server) dispatchFSUndo(ctx context.Context, traceID string, row sqlcgen.UndoWindow, chain *mcpfs.UndoChain) policyUndoOp {
	op := policyUndoOp{OpID: row.OpID, Tool: row.Tool, TaskID: row.TaskID, Recipe: undoRecipeUnsupported}
	if chain == nil || (row.Tool != "fs_write" && row.Tool != "fs_delete") {
		return op
	}
	res, err := chain.UndoOp(ctx, traceID, row.OpID)
	switch {
	case errors.Is(err, mcpfs.ErrNoUndoRecord):
		op.Recipe, op.Error = undoRecipeNoRecord, err.Error()
		s.log.With(traceID).Warn("fs_undo_no_record", "tool", row.Tool, "op_id", row.OpID, "task_id", row.TaskID)
	case err != nil:
		op.Recipe, op.Error = undoRecipeFailed, err.Error()
		op.Path = res.CanonicalPath
		s.log.With(traceID).Error("fs_undo_recipe_failed", "tool", row.Tool, "op_id", row.OpID, "task_id", row.TaskID,
			"blocked", errors.Is(err, mcpfs.ErrUndoBlocked), "target_changed", errors.Is(err, mcpfs.ErrTargetChanged), "err", err.Error())
	default:
		op.Recipe, op.Path = undoRecipeExecuted, res.CanonicalPath
	}
	return op
}
