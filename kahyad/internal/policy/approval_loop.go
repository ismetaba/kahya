// approval_loop.go implements the engine half of the B-1 approval loop
// (needs_approval -> human decision -> execution of exactly the approved
// bytes):
//
//   - redeemApprovedGrant: the binding Engine.Check, run again by the SAME
//     tool call after the human approved (kahyad/internal/server's
//     checkWithApprovalWait, CheckInput.RedeemPendingApprovalID), finds the
//     single-use grant Engine.Approve persisted for that call's OWN pending
//     approval - bound to exactly these (task_id, tool, scope,
//     approved_bytes_hash) - burns it, and answers ALLOW with a freshly
//     minted token the tool
//     consumes immediately (ConsumeToken is unchanged). The approved bytes
//     never pass back through the model: the parked call resumes with the
//     very bytes the human saw (HANDOFF S5 #5 WYSIWYE).
//   - PendingApprovalDecision: the read-only probe the server-side waiter
//     polls (belt and braces next to SetApprovalDecidedHook).
//   - BootstrapLevels: config-seeded first-run autonomy rows
//     (autonomy_bootstrap_levels, owner decision G01-2 option A).
package policy

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"kahya/kahyad/internal/store/sqlcgen"
)

// redeemApprovedGrant (B-1) looks for an unconsumed, unrevoked, unexpired
// grant_kind='approval' approval_tokens row bound to this EXACT
// (task_id, tool, scope, approved_bytes_hash), burns it atomically and -
// for a side-effectful class - mints a fresh grant_kind='exchanged' token
// the calling tool consumes right away. ok=false means "no usable grant -
// fall through to the ordinary ladder decision"; every failure branch is
// fail-closed (never an allow without a freshly minted token for a W-class
// action).
//
// The grant is burned with the SAME single-use UPDATE ConsumeToken uses
// (ConsumeApprovalToken: WHERE consumed_at IS NULL), directly on the store
// rather than through Engine.ConsumeToken, so failFromHash's demotion logic
// never runs: this is redemption, not a violation. A concurrent redeemer
// loses that UPDATE and falls through to needs_approval. The exchanged
// token is never itself redeemable (GetRedeemableApprovalGrant only ever
// matches grant_kind='approval').
//
// Which grant: a parked call's post-wait re-check
// (in.RedeemPendingApprovalID) may redeem ONLY the grant of its own pending
// approval (lookupOwnGrant) - never a sibling's grant for the same task and
// bytes, so a denial of THIS call cannot be bypassed by approving another.
// Every other binding Check (the model's identical retry after a timeout)
// redeems the most recent matching grant by (task_id, tool, scope, hash).
func (e *Engine) redeemApprovedGrant(ctx context.Context, in CheckInput, class ActionClass, scope, hash string, level int) (Decision, bool) {
	var row sqlcgen.ApprovalToken
	var err error
	switch {
	case in.RedeemPendingApprovalID != "":
		row, err = e.lookupOwnGrant(ctx, in, scope, hash)
	case in.TaskID == "":
		// Fail-closed backstop: with no task id a hash match would bind the
		// grant to nobody - any other task-less caller of the same bytes
		// (or a tool that forgot to forward its task id) could redeem it.
		// Such a grant is redeemable only by its own parked call above.
		return Decision{}, false
	default:
		row, err = e.store.GetRedeemableApprovalGrant(ctx, sqlcgen.GetRedeemableApprovalGrantParams{
			TaskID: in.TaskID, Tool: in.Tool, Scope: scope, ApprovedBytesHash: hash,
		})
	}
	if errors.Is(err, sql.ErrNoRows) {
		return Decision{}, false
	}
	if err != nil {
		e.ledgerRaw(ctx, in.TraceID, "approval_grant_lookup_error", map[string]any{
			"event": "approval_grant_lookup_error", "task_id": in.TaskID, "tool": in.Tool, "err": err.Error(),
		})
		return Decision{}, false
	}
	pendingID := row.PendingApprovalID.String
	burn := func() (bool, error) {
		affected, err := e.store.ConsumeApprovalToken(ctx, sqlcgen.ConsumeApprovalTokenParams{
			ConsumedAt: sql.NullString{String: rfc3339(e.nowUTC()), Valid: true}, TokenHash: row.TokenHash,
		})
		return affected == 1, err
	}
	if expiresAt, perr := time.Parse(time.RFC3339Nano, row.ExpiresAt); perr != nil || e.nowUTC().After(expiresAt) {
		// Burned so the same stale grant is reported (and looked at) once,
		// not on every later Check for these bytes. Never redeemed.
		_, _ = burn()
		e.ledgerRaw(ctx, in.TraceID, "approval_grant_expired", map[string]any{
			"event": "approval_grant_expired", "task_id": in.TaskID, "tool": in.Tool, "pending_approval_id": pendingID,
		})
		return Decision{}, false
	}
	if ActionClass(row.Class) != class {
		// The tool's class changed (policy.yaml edited + kahyad restarted)
		// since the human approved it: what they approved is not what would
		// run now. Never redeemed; left for halt/expiry.
		e.ledgerRaw(ctx, in.TraceID, "approval_grant_class_mismatch", map[string]any{
			"event": "approval_grant_class_mismatch", "task_id": in.TaskID, "tool": in.Tool,
			"pending_approval_id": pendingID, "grant_class": row.Class, "class": class,
		})
		return Decision{}, false
	}
	won, err := burn()
	if err != nil {
		e.ledgerRaw(ctx, in.TraceID, "approval_grant_redeem_error", map[string]any{
			"event": "approval_grant_redeem_error", "task_id": in.TaskID, "tool": in.Tool,
			"pending_approval_id": pendingID, "err": err.Error(),
		})
		return Decision{}, false
	}
	if !won {
		return Decision{}, false // a concurrent redeemer (or a halt) got it first
	}

	var tok string
	if class != ClassR {
		// R needs no consume step (Decision.Token's contract) - only a
		// side-effectful class gets the exchanged token.
		tok, err = e.mintToken(ctx, in.TaskID, in.TraceID, in.Tool, class, scope, hash, GrantKindExchanged, pendingID)
		if err != nil {
			// The grant is already burned: this approval is lost, and the
			// call is refused - fail-closed, never an allow without a token.
			d := Decision{Result: ResultDeny, Reason: ReasonPolicyStateError, Rule: RuleLadderV1, Class: class, Scope: scope, Level: level}
			e.ledgerDecision(ctx, in, class, scope, level, d)
			return d, true
		}
	}
	if class == ClassW1 {
		// The redeemed W1 write earns the same 5-minute safety net an
		// auto-allowed one gets (idempotent per task/tool/trace). G00 C-2:
		// G05 B-8 moves window opening to ConsumeTokenOp and removes this.
		if err := e.openUndoWindow(ctx, in.TaskID, in.Tool, in.TraceID); err != nil {
			e.ledgerRaw(ctx, in.TraceID, "undo_window_open_failed", map[string]any{
				"event": "undo_window_open_failed", "task_id": in.TaskID, "tool": in.Tool, "err": err.Error(),
			})
		}
	}
	e.ledgerRaw(ctx, in.TraceID, "approval_grant_redeemed", map[string]any{
		"event": "approval_grant_redeemed", "task_id": in.TaskID, "tool": in.Tool, "class": class,
		"scope": scope, "pending_approval_id": pendingID,
	})
	d := Decision{Result: ResultAllow, Rule: RuleLadderV1, Class: class, Scope: scope, Level: level, Token: tok, RedeemedPendingApprovalID: pendingID}
	e.ledgerDecision(ctx, in, class, scope, level, d)
	return d, true
}

// lookupOwnGrant (B-1) returns the still-redeemable grant Engine.Approve
// minted for in.RedeemPendingApprovalID, or sql.ErrNoRows when there is none
// (not approved, already redeemed, halt-revoked). A grant whose binding is
// not exactly this call's (task_id, tool, scope, approved_bytes_hash) is
// ledgered approval_grant_binding_mismatch and never redeemed - Approve
// copies all four from the pending row this very call minted, so a
// mismatch means the row was tampered with. Expiry and class are checked
// by the caller, exactly as for a hash-matched grant.
func (e *Engine) lookupOwnGrant(ctx context.Context, in CheckInput, scope, hash string) (sqlcgen.ApprovalToken, error) {
	id := in.RedeemPendingApprovalID
	row, err := e.store.GetApprovalGrantByPendingApprovalID(ctx, sql.NullString{String: id, Valid: true})
	if err != nil {
		return sqlcgen.ApprovalToken{}, err
	}
	if row.ConsumedAt.Valid || row.RevokedAt.Valid {
		return sqlcgen.ApprovalToken{}, sql.ErrNoRows
	}
	if row.TaskID != in.TaskID || row.Tool != in.Tool || row.Scope != scope || row.ApprovedBytesHash != hash {
		e.ledgerRaw(ctx, in.TraceID, "approval_grant_binding_mismatch", map[string]any{
			"event": "approval_grant_binding_mismatch", "task_id": in.TaskID, "tool": in.Tool, "pending_approval_id": id,
		})
		return sqlcgen.ApprovalToken{}, sql.ErrNoRows
	}
	return row, nil
}

// PendingApprovalDecision (B-1) reports whether id has been decided and how
// (PendingDecisionApproved | PendingDecisionDenied |
// PendingDecisionInvalidated). decided=false for a live, undecided row;
// sql.ErrNoRows for an unknown id. Read-only.
//
// RACE: Approve burns the pending row (decision='approved') and only THEN
// mints the grant. brain.db runs on ONE connection (store/db.go
// SetMaxOpenConns(1)), so a waiter probe can interleave between those two
// statements and see decision='approved' with no grant yet - its re-check
// would then find nothing to redeem. 'approved' is therefore reported as
// decided ONLY once the grant row exists (GetApprovalGrantByPendingApprovalID,
// any consumed/revoked state). The hook path (fireApprovalDecided, fired
// AFTER mintToken) has no such window. A row left 'approved' with no grant
// forever (mintToken failed after the burn - a DB error) reads as
// undecided: the waiter times out - fail-closed, nothing executes.
func (e *Engine) PendingApprovalDecision(ctx context.Context, id string) (decided bool, decision string, err error) {
	pa, err := e.store.GetPendingApproval(ctx, id)
	if err != nil {
		return false, "", err
	}
	if !pa.ConsumedAt.Valid {
		return false, "", nil
	}
	if pa.Decision.String == PendingDecisionApproved {
		if _, gerr := e.store.GetApprovalGrantByPendingApprovalID(ctx, sql.NullString{String: id, Valid: true}); gerr != nil {
			return false, "", nil // grant mint still in flight (or failed): not decided yet
		}
	}
	return true, pa.Decision.String, nil
}

// BootstrapLevels (B-1, owner decision G01-2 option A) seeds autonomy_state
// rows from config (config.Config.AutonomyBootstrapLevels, key
// "tool/class/scope" -> level) ONLY where no row exists yet. An existing
// row - including a demotion back to 0 after a denial - is never touched,
// so this is equivalent to the user having run `kahya autonomy promote`
// once per key on first boot, never a standing override of the ladder. A
// malformed key, an unknown tool, a class that is not the tool's own
// policy.yaml class, or an out-of-range level is ledgered
// (autonomy_bootstrap_rejected) and skipped - never fatal to boot, never a
// permissive fallback. Every seeded row is ledgered (autonomy_bootstrapped).
// W3 is unaffected by any level (autoLevelFor never auto-allows it).
func (e *Engine) BootstrapLevels(ctx context.Context, traceID string, levels map[string]int) {
	for key, level := range levels {
		reject := func(reason string) {
			e.ledgerRaw(ctx, traceID, "autonomy_bootstrap_rejected", map[string]any{
				"event": "autonomy_bootstrap_rejected", "key": key, "level": level, "reason": reason,
			})
		}
		parts := strings.Split(key, "/")
		if len(parts) != 3 {
			reject("key must be tool/class/scope")
			continue
		}
		tool, class, scope := Canonicalize(parts[0]), ActionClass(parts[1]), normalizeScope(parts[2])
		rule, ok := e.policy.ToolsByName[tool]
		if !ok {
			reject("unknown tool")
			continue
		}
		if !validClasses[class] || level < L0 || level > L4 {
			reject("invalid class or level")
			continue
		}
		if rule.Class != class {
			reject("class does not match policy.yaml")
			continue
		}
		if _, err := e.store.GetAutonomyState(ctx, sqlcgen.GetAutonomyStateParams{Tool: tool, Class: string(class), Scope: scope}); err == nil {
			continue // row exists - never override
		} else if !errors.Is(err, sql.ErrNoRows) {
			reject("state read error: " + err.Error())
			continue
		}
		if err := e.saveState(ctx, tool, class, scope, level, 0); err != nil {
			reject("state write error: " + err.Error())
			continue
		}
		e.ledgerRaw(ctx, traceID, "autonomy_bootstrapped", map[string]any{
			"event": "autonomy_bootstrapped", "tool": tool, "class": class, "scope": scope, "level": level,
		})
	}
}
