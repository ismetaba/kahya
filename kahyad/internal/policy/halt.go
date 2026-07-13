// halt.go implements the approval half of the W6-03 emergency halt
// (HANDOFF S6 W6 flag: "bekleyen tum onaylar gecersiz kilinir"):
// InvalidateApprovalsForTask consumes every not-yet-decided
// pending_approvals row for a task AND revokes every not-yet-consumed
// approval_tokens row for that same task, in one call, so
// kahyad/internal/halt.Executor never has to know either table's shape.
//
// Token revocation is what makes the halt binding on EVERY approval
// surface at once (HANDOFF S5 one-time approval tokens): a stale CLI
// `decide`, a Hammerspoon card button, or a Telegram W2 inline button
// pressed AFTER this call all route through the SAME ConsumePendingApproval
// (for a still-undecided approval) or ConsumeToken (for an already-approved
// action whose token the tool has not yet presented) atomic
// "UPDATE ... WHERE ... IS NULL" guard - a diff approved before the halt
// must not authorize anything after it, and now cannot.
//
// This deliberately does NOT reuse Engine.Approve/Deny/ConsumeToken: those
// paths ALSO demote the autonomy ladder on a violation (wrong typed
// "onayla", hash mismatch, replay) - a halt is the user choosing to stop,
// not a tool misusing a token, so nothing here should count against the
// ladder. The two atomic UPDATEs below are surgical and side-effect-free
// beyond "this row/token can never be used again".
package policy

import (
	"context"
	"database/sql"
	"fmt"

	"kahya/kahyad/internal/store/sqlcgen"
)

// EventApprovalInvalidated is the ledger event kind InvalidateApprovalsForTask
// appends once per pending_approvals row it consumes (W6-03 deliverable:
// "Ledger event kinds: task.user_halted, approval.invalidated").
const EventApprovalInvalidated = "approval.invalidated"

// InvalidateApprovalsForTask consumes every not-yet-decided
// pending_approvals row for taskID (so a subsequent Approve/Deny call
// against any of their ids fails with ErrInvalidPendingApproval - the same
// "0 rows affected" fail-closed path a genuine replay hits) and revokes
// every not-yet-consumed approval_tokens row for taskID (so a
// side-effectful MCP tool presenting one to ConsumeToken after this call
// fails with ErrTokenInvalid). Continues past a single row's ledger/hook
// failure (best-effort - the W6-03 halt executor's own "continue past
// individual failures" contract) rather than aborting the rest of the
// task's invalidation sweep. Returns how many pending_approvals rows were
// invalidated (the token revocation count is ledgered per-row below but
// not separately returned - callers needing it can inspect the ledger).
func (e *Engine) InvalidateApprovalsForTask(ctx context.Context, traceID, taskID string) (int, error) {
	rows, err := e.store.ListUnconsumedPendingApprovalsByTask(ctx, taskID)
	if err != nil {
		return 0, fmt.Errorf("policy: list pending approvals for task %s: %w", taskID, err)
	}

	now := rfc3339(e.nowUTC())
	invalidated := 0
	for _, row := range rows {
		affected, err := e.store.ConsumePendingApproval(ctx, sqlcgen.ConsumePendingApprovalParams{
			ConsumedAt: sql.NullString{String: now, Valid: true}, ID: row.ID,
		})
		if err != nil {
			// Best-effort: log via the ledger and move on to the next row -
			// halt must never partial-abort over one row's DB hiccup.
			e.ledgerRaw(ctx, traceID, "approval_invalidate_failed", map[string]any{
				"event": "approval_invalidate_failed", "pending_approval_id": row.ID, "task_id": taskID, "err": err.Error(),
			})
			continue
		}
		if affected == 0 {
			// Lost a race to a CONCURRENT halt of the same task (double-press
			// ⌥⎋, or two POST /halt): the winner already consumed this row via
			// the atomic "WHERE consumed_at IS NULL" guard. Don't double-count,
			// double-ledger approval.invalidated, or re-fire the Telegram-edit
			// hook (W6-03 review MINOR).
			continue
		}
		invalidated++
		e.ledgerRaw(ctx, traceID, EventApprovalInvalidated, map[string]any{
			"event": EventApprovalInvalidated, "pending_approval_id": row.ID,
			"task_id": taskID, "tool": row.Tool, "class": row.Class,
		})
		if e.invalidatedHook != nil {
			e.invalidatedHook(PendingApprovalInfo{
				ID: row.ID, Tool: row.Tool, Class: ActionClass(row.Class), Scope: row.Scope,
				ToolInput: row.ToolInput, TraceID: traceID, TaskID: taskID,
			})
		}
	}

	// Revoke every not-yet-consumed one-time token for this task - covers
	// the OTHER half of "a diff approved before the halt must not
	// authorize anything after it": a token an autonomy-ladder auto-allow
	// (W1) or a prior human Approve (W2) already minted, that the
	// corresponding side-effectful MCP tool has not yet presented to
	// ConsumeToken. One UPDATE, no per-row ledger (approval_tokens rows
	// carry no human-facing surface of their own to invalidate - unlike
	// pending_approvals, there is no card/dialog to edit).
	if _, err := e.store.RevokeApprovalTokensByTask(ctx, sqlcgen.RevokeApprovalTokensByTaskParams{
		// consumed_at burns the token (so ConsumeToken's single-use guard
		// denies it); revoked_at ADDITIONALLY marks it halt-revoked so
		// failFromHash skips the autonomy-ladder demotion (W6-03 review fix:
		// a user halt is not a tool misusing a token).
		ConsumedAt: sql.NullString{String: now, Valid: true},
		RevokedAt:  sql.NullString{String: now, Valid: true},
		TaskID:     taskID,
	}); err != nil {
		e.ledgerRaw(ctx, traceID, "approval_token_revoke_failed", map[string]any{
			"event": "approval_token_revoke_failed", "task_id": taskID, "err": err.Error(),
		})
	}

	return invalidated, nil
}
