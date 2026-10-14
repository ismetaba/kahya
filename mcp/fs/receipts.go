// receipts.go is the W4-02 idempotent-receipt seam every side-effectful tool
// in this package - and, through type aliases, mcp/shell and mcp/osascript -
// runs its effect through (FIX-I-4, the W4 receipt gap).
//
// Order inside a tool, after its mechanical pre-policy checks (deny glob,
// scan, argv validator, ...):
//
//  1. ReplayReceipt BEFORE Policy.Check: a durable receipt for this exact
//     (task, tool, canonical args hash) means a resumed session is
//     re-issuing a call that already completed - the stored result is
//     returned, no policy decision is taken, no approval is asked for again,
//     no token is minted or consumed, and nothing executes.
//  2. Policy.Check, Policy.ConsumeToken (unchanged).
//  3. ExecuteWithReceipt runs the effect closure inside kahyad's
//     tool_calls lifecycle: intent -> executing (the pre-op journal) ->
//     effect -> receipt (the post-op record). The effect runs on a context
//     detached from the MCP request's cancellation (a worker or bridge that
//     dies mid-call must not abandon a half-done effect; its receipt must
//     still land), and holds no brain.db connection while it runs.
//
// The args hash is sha256 over the SAME canonical tool-input bytes the
// approval token is bound to (WYSIWYE, HANDOFF §5 safety #5) or, where a
// tool binds its token to less than its full input, over a canonical
// envelope of the full input - a replay only ever answers the identical
// call. kahyad is still the only writer of brain.db: this package never
// touches the DB, kahyad/internal/server adapts *task.Receipts to
// ReceiptExecutor in-process.
package fs

import (
	"context"
	"encoding/json"
	"fmt"
)

// ReceiptInput mirrors kahyad/internal/task.ExecuteInput without importing
// it (mcp/* lives outside kahyad/internal and cannot).
type ReceiptInput struct {
	TaskID        string
	TraceID       string
	Tool          string
	Class         string // PolicyDecision.Class: W1 | W2 | W3 (never R)
	ArgsHash      string
	ApprovalToken string
}

// ReceiptExecutor is kahyad's receipt lifecycle as this package sees it.
// kahyad/internal/server.NewReceiptExecutor adapts *task.Receipts
// (Replay + ExecuteDetached). nil (tests, standalone use) = no receipts: the
// effect runs directly, exactly as before FIX-I-4.
type ReceiptExecutor interface {
	// Replay returns a stored receipt's result for in's exact key, claiming
	// nothing on a miss.
	Replay(ctx context.Context, in ReceiptInput) (result json.RawMessage, hit bool, err error)
	// Execute runs effect inside the intent -> executing -> receipt
	// lifecycle; replayed is true when an identical call's receipt answered
	// instead (effect not run).
	Execute(ctx context.Context, in ReceiptInput, effect func(ctx context.Context) (json.RawMessage, error)) (result json.RawMessage, replayed bool, err error)
}

// ReceiptArgsHash is the receipt key's args hash: sha256(hex) of the
// canonical tool-input bytes.
func ReceiptArgsHash(canonical []byte) string { return sha256Hex(canonical) }

// receiptsActive reports whether a call goes through the receipt lifecycle:
// a wired executor and a task to key it on (a direct Go caller or test with
// no task id keeps the pre-FIX-I-4 direct path).
func receiptsActive(rex ReceiptExecutor, taskID string) bool {
	return rex != nil && taskID != ""
}

// ReplayReceipt is step 1 of this file's order: when receipts are active
// and a durable receipt exists for in, it is decoded into out and hit is
// true - the caller returns out without a policy decision. A lookup or
// decode error is returned (fail-closed: the caller must not execute).
func ReplayReceipt(ctx context.Context, rex ReceiptExecutor, in ReceiptInput, out any) (hit bool, err error) {
	if !receiptsActive(rex, in.TaskID) {
		return false, nil
	}
	raw, hit, err := rex.Replay(ctx, in)
	if err != nil || !hit {
		return false, err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return false, fmt.Errorf("decode stored receipt: %w", err)
	}
	return true, nil
}

// ExecuteWithReceipt is step 3: effect runs inside rex's lifecycle (on a
// context detached from ctx's cancellation) when receipts are active, or
// directly on ctx otherwise. The returned bytes are the effect's own result
// or, when an identical call's receipt answered instead, the stored one
// (replayed true). With receipts active, an error may come back TOGETHER
// with a result: the effect ran but its receipt could not be persisted -
// callers report the error (fail-closed) and must not retry the effect.
func ExecuteWithReceipt(ctx context.Context, rex ReceiptExecutor, in ReceiptInput, effect func(ctx context.Context) (json.RawMessage, error)) (raw json.RawMessage, replayed bool, err error) {
	if !receiptsActive(rex, in.TaskID) {
		raw, err = effect(ctx)
		return raw, false, err
	}
	return rex.Execute(context.WithoutCancel(ctx), in, effect)
}
