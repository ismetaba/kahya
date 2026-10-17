// receipts.go wires the W4-02 receipt lifecycle (kahyad/internal/task.
// Receipts) into every REAL side-effectful tool (FIX-I-4, the W4 receipt
// gap):
//
//   - fs_write/fs_delete (mcp/fs), shell_docker/shell_host (mcp/shell) and
//     applescript_run/jxa_run/shortcuts_run (mcp/osascript) run their own
//     gate chains; they reach Receipts through the one in-process adapter
//     below (mcp/fs.ReceiptExecutor - the shell/osascript packages alias
//     it), which maps Replay to Receipts.Replay and Execute to
//     Receipts.ExecuteDetached;
//   - every other tool mounted on /v1/mcp (memory_write/memory_forget, the
//     dev-only w2_slow_stub) goes through policyGateMiddleware's generic
//     path, which replays an identical call an EARLIER worker run completed
//     BEFORE the policy check and runs a non-R call's handler inside
//     ExecuteDetached (mcpReplayStoredResult / mcpExecuteWithReceipt).
//
// FIX-FINAL receipt-replay-per-run: a receipt is replayed only for a later
// (resumed) run of the task - at most once per run - and never inside the
// live run that wrote it, where an identical call executes again. The run
// is kahyad-side state (tasks.worker_run, pinned per tools/call by
// policyGateMiddleware's task.WithRunPin), never a worker value.
//
// kahyad stays brain.db's only writer: the mcp/* packages never touch the
// DB, they call back into this process through the adapter.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"kahya/kahyad/internal/task"
	mcpfs "kahya/mcp/fs"
)

// SetReceipts wires the receipt lifecycle the generic /v1/mcp path uses for
// every non-R tool (FIX-I-4) and that ReceiptExecutor adapts for the
// fs/shell/osascript tools. Call before Prepare(). nil = no receipts (tests
// that do not exercise them).
func (s *Server) SetReceipts(r *task.Receipts) {
	s.receipts = r
}

// ReceiptExecutor returns the mcp/fs.ReceiptExecutor adapter onto the
// receipts wired by SetReceipts (nil interface when none are), consulting
// the server's deny-all state like FSPolicyClient does. Call after
// SetReceipts.
func (s *Server) ReceiptExecutor() mcpfs.ReceiptExecutor {
	if s.receipts == nil {
		return nil
	}
	return NewReceiptExecutor(s.receipts, s.DenyAll)
}

// NewReceiptExecutor adapts *task.Receipts to mcp/fs.ReceiptExecutor:
// Replay -> Receipts.Replay, Execute -> Receipts.ExecuteDetached. denyAll may
// be nil; when it reports true (policy.yaml failed to load at boot) Replay
// answers a miss, so the tool falls through to its policy check, which
// denies - deny-all mode serves nothing, not even a stored result.
func NewReceiptExecutor(r *task.Receipts, denyAll func() bool) mcpfs.ReceiptExecutor {
	return receiptExecutorAdapter{r: r, denyAll: denyAll}
}

type receiptExecutorAdapter struct {
	r       *task.Receipts
	denyAll func() bool
}

func toExecuteInput(in mcpfs.ReceiptInput) task.ExecuteInput {
	return task.ExecuteInput{
		TaskID: in.TaskID, TraceID: in.TraceID, ToolName: in.Tool,
		Class: task.ActionClass(in.Class), ArgsHash: in.ArgsHash, ApprovalToken: in.ApprovalToken,
	}
}

func (a receiptExecutorAdapter) Replay(ctx context.Context, in mcpfs.ReceiptInput) (json.RawMessage, bool, error) {
	if a.denyAll != nil && a.denyAll() {
		return nil, false, nil
	}
	return a.r.Replay(ctx, toExecuteInput(in))
}

func (a receiptExecutorAdapter) Execute(ctx context.Context, in mcpfs.ReceiptInput, effect func(context.Context) (json.RawMessage, error)) (json.RawMessage, bool, error) {
	return a.r.ExecuteDetached(ctx, toExecuteInput(in), task.DetachedEffectFunc(effect))
}

// canonicalToolArgs is the generic /v1/mcp path's receipt-key input: the
// call's JSON arguments re-encoded canonically (object keys sorted, no
// insignificant whitespace, numbers verbatim). A resumed model re-issuing
// the same call may serialize it with another key order or spacing; the
// tool decodes both to the same input, so both must hit the same receipt.
// Bytes that are not valid JSON are used as they are (the tool will reject
// them anyway).
func canonicalToolArgs(raw []byte) []byte {
	if len(bytes.TrimSpace(raw)) == 0 {
		return []byte("null")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil || dec.More() {
		return raw
	}
	out, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return out
}

// mcpReplayStoredResult answers a generic-path tool call from a stored
// receipt (FIX-I-4): hit=false on a miss; an error means the lookup (or the
// stored result) could not be read - the caller refuses the call.
func (s *Server) mcpReplayStoredResult(ctx context.Context, in task.ExecuteInput) (mcp.Result, bool, error) {
	raw, hit, err := s.receipts.Replay(ctx, in)
	if err != nil || !hit {
		return nil, false, err
	}
	var out mcp.CallToolResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, false, err
	}
	s.log.With(in.TraceID).Info("mcp_tool_replayed", "tool", in.ToolName, "task_id", in.TaskID)
	return withReplayNote(&out), true, nil
}

// withReplayNote appends the model-facing mcp/fs.MsgReplayedFromEarlierRun
// to a generic-path result answered from an earlier run's receipt
// (FIX-FINAL receipt-replay-per-run) - the stored content stays first and
// unchanged; the note tells the resumed model the call did not run again.
func withReplayNote(out *mcp.CallToolResult) *mcp.CallToolResult {
	out.Content = append(out.Content, &mcp.TextContent{Text: mcpfs.MsgReplayedFromEarlierRun})
	return out
}

// errToolResultIsError marks a handler that returned an MCP tool error
// (CallToolResult.IsError): its effect did not complete, so no receipt is
// written (the row goes 'failed') and the tool's own error result is passed
// through unchanged.
var errToolResultIsError = errors.New("server: tool returned an error result")

// mcpExecuteWithReceipt runs a generic-path tool handler inside
// Receipts.ExecuteDetached (FIX-I-4) on a context detached from the MCP
// request's cancellation (the W4-07 posture: a worker killed mid-call must
// not abandon a half-done effect). The handler's successful CallToolResult
// is the receipt; a tool error result is returned as is with no receipt; a
// replay hit (an identical call of an earlier run completed meanwhile)
// returns the stored result.
func (s *Server) mcpExecuteWithReceipt(ctx context.Context, in task.ExecuteInput, run func(context.Context) (mcp.Result, error)) (mcp.Result, error) {
	var toolErrResult, fresh mcp.Result
	raw, replayed, err := s.receipts.ExecuteDetached(context.WithoutCancel(ctx), in, func(ectx context.Context) (json.RawMessage, error) {
		res, err := run(ectx)
		if err != nil {
			return nil, err
		}
		if ctr, ok := res.(*mcp.CallToolResult); ok && ctr.IsError {
			toolErrResult = res
			return nil, errToolResultIsError
		}
		fresh = res
		return json.Marshal(res)
	})
	if toolErrResult != nil {
		return toolErrResult, nil
	}
	if err != nil {
		s.log.With(in.TraceID).Error("mcp_tool_receipt_failed", "tool", in.ToolName, "task_id", in.TaskID, "err", err.Error())
		return nil, err
	}
	if !replayed {
		return fresh, nil
	}
	var out mcp.CallToolResult
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		return nil, uerr
	}
	s.log.With(in.TraceID).Info("mcp_tool_replayed", "tool", in.ToolName, "task_id", in.TaskID)
	return withReplayNote(&out), nil
}
