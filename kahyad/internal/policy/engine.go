// engine.go implements the W3-02 binding policy decision: given a tool
// name, a ladder scope, and a task/trace correlation pair, it answers
// ALLOW / NEEDS_APPROVAL / DENY per the autonomy ladder (HANDOFF S4 ladder
// flag), tracks per-(tool,class,scope) promotion/demotion state, and opens
// the W1 5-minute undo window on an auto-allowed undoable write. This
// REPLACES W12-07's interim static allow/deny table (formerly interim.go,
// now retired) as the decision logic task.go's POST /policy/check and
// mcp.go's policyGateMiddleware consult.
//
// HARD-CODED SAFETY INVARIANTS (Go, not config - CLAUDE.md/HANDOFF S5):
//   - class W3 NEVER returns ALLOW, at any level, ever (autoLevelFor
//     returns ok=false for ClassW3 - there is no threshold it could ever
//     meet). A W3 approval additionally requires surface="local"
//     (enforced in Approve below) - Telegram can notify, never approve.
//   - The decision's only inputs are the loaded Policy (tool -> class,
//     W3-01), autonomy_state (this file), the caller-supplied scope, and
//     task/trace correlation ids. No memory/index/search package is
//     imported by this package - see import_graph_test.go, which asserts
//     that mechanically rather than trusting a code review to notice a
//     future import - so no fact/embedding/hafiza content can ever reach
//     a decision (HANDOFF S5 product principle: memory never lowers a
//     permission).
package policy

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"kahya/kahyad/internal/store/sqlcgen"
)

// Canonicalize strips an SDK-style "mcp__<server>__<tool>" prefix down to
// the bare tool name (e.g. "mcp__kahya_memory__memory_write" ->
// "memory_write"), matching how Claude's tool-use layer names MCP-routed
// tools to the model. A name not shaped like that prefix (no leading
// "mcp__", or no second "__" after it) is returned unchanged - never
// guessed at. Moved here from the now-retired interim.go (W12-07); Check
// below calls this itself (defense in depth), so a caller that already
// canonicalized before calling pays no extra cost and gets no different
// an answer.
func Canonicalize(name string) string {
	const prefix = "mcp__"
	if !strings.HasPrefix(name, prefix) {
		return name
	}
	rest := name[len(prefix):]
	idx := strings.Index(rest, "__")
	if idx < 0 {
		return name
	}
	return rest[idx+2:]
}

// Ladder levels (HANDOFF S4: L0 Gozlemci .. L4 Kahya).
const (
	L0 = 0
	L1 = 1
	L2 = 2
	L3 = 3
	L4 = 4
)

// RuleLadderV1 identifies decisions produced by this file's ladder engine,
// distinct from RuleDenyAllV1 so a decision's provenance stays visible in
// the ledger/response.
const RuleLadderV1 = "ladder-v1"

// Decision.Result values (HANDOFF S4 ladder: "Sonuc ALLOW/NEEDS_APPROVAL/
// DENY olur"). Kept lower-case to match the retired W12-07 interim
// table's allow/deny wire convention - NEEDS_APPROVAL is the one new
// value this file adds.
const (
	ResultAllow         = "allow"
	ResultNeedsApproval = "needs_approval"
	ResultDeny          = "deny"
)

// Turkish user-facing deny/approval reasons (CLAUDE.md language policy).
const (
	ReasonUnknownTool      = "Tanınmayan araç reddedildi (fail-closed)."
	ReasonNeedsApproval    = "Bu eylem için onay gerekiyor (otonomi seviyesi yetersiz)."
	ReasonW3AlwaysApproval = "W3 sınıfı eylemler her zaman yazılı yerel onay gerektirir."
	ReasonPolicyStateError = "Politika durumu okunamadı; güvenlik gereği reddedildi (fail-closed)."
)

// defaultScope is substituted whenever a caller supplies an empty scope -
// the same default loader.go's normalize step gives an unset
// ToolRule.ScopeKey, so a tool that never declared its own scope dimension
// earns/spends autonomy under one single, predictable scope value.
const defaultScope = "global"

// undoWindowDuration is the W1 5-minute undo grace period's PRODUCTION
// DEFAULT (HANDOFF S4 ladder: "L2 | Eslikci | R, W1 (5-dk geri-alma +
// defter)"). Engine's own window length is configurable (Engine.
// undoWindowDuration / SetUndoWindowDuration below, threaded from
// config.Config.UndoWindowSeconds by main.go) so a test can inject a
// short window end-to-end (MINOR fix: exercising SweepExpiredUndoWindows
// -> SetUndoExpiryHook -> the owning tool's own purge, rather than only
// ever calling that purge directly) — NewEngine still defaults to this
// exact 5-minute constant, so every caller that never calls
// SetUndoWindowDuration keeps today's production behavior unchanged.
const undoWindowDuration = 5 * time.Minute

// promotionThreshold is the "20 ardisik onay + 0 red" terfi-ONERI
// threshold (HANDOFF S4: promotion is only ever SUGGESTED here - the
// level itself never changes except via Engine.Promote, the CLI's
// user-invoked `kahya autonomy promote`).
const promotionThreshold = 20

// autoLevelFor returns the minimum ladder level at which class is
// automatic, per the HANDOFF S4 ladder table: L1 auto-R, L2 adds auto-W1
// (+ 5-min undo/ledger), L3 adds auto-W2 (from the triple's own earned
// level - reaching L3 on a (tool,W2,scope) row IS that row's entry in the
// "earned allowlist"), L4 changes nothing further for R/W1/W2. ok is
// false for ClassW3 (and any unrecognized class): W3 NEVER auto-allows,
// at any level - this is the one hard-coded-in-Go branch nothing in this
// package's config-shaped inputs can override.
func autoLevelFor(class ActionClass) (level int, ok bool) {
	switch class {
	case ClassR:
		return L1, true
	case ClassW1:
		return L2, true
	case ClassW2:
		return L3, true
	default:
		return 0, false
	}
}

// normalizeScope mirrors loader.go's ScopeKey default: an empty
// caller-supplied scope becomes "global", never a bare "".
func normalizeScope(scope string) string {
	if scope == "" {
		return defaultScope
	}
	return scope
}

// Store is the narrow autonomy-ladder persistence surface Engine needs.
// *sqlcgen.Queries (via *store.Store) satisfies it directly, with no
// adapter - the same pattern server.TaskStore already uses for the tasks
// table.
type Store interface {
	GetAutonomyState(ctx context.Context, arg sqlcgen.GetAutonomyStateParams) (sqlcgen.AutonomyState, error)
	ListAutonomyState(ctx context.Context) ([]sqlcgen.AutonomyState, error)
	InsertAutonomyState(ctx context.Context, arg sqlcgen.InsertAutonomyStateParams) error
	UpdateAutonomyState(ctx context.Context, arg sqlcgen.UpdateAutonomyStateParams) (int64, error)

	InsertApprovalToken(ctx context.Context, arg sqlcgen.InsertApprovalTokenParams) error
	GetApprovalToken(ctx context.Context, tokenHash string) (sqlcgen.ApprovalToken, error)
	ConsumeApprovalToken(ctx context.Context, arg sqlcgen.ConsumeApprovalTokenParams) (int64, error)

	// Pending-approval persistence (post-security-review amendment): a
	// NEEDS_APPROVAL decision's pending_approval_id is now a server-issued,
	// single-use, DB-tracked reference - never an unsigned caller-decodable
	// blob. See mintPendingApproval/Approve/Deny below.
	InsertPendingApproval(ctx context.Context, arg sqlcgen.InsertPendingApprovalParams) error
	GetPendingApproval(ctx context.Context, id string) (sqlcgen.PendingApproval, error)
	ConsumePendingApproval(ctx context.Context, arg sqlcgen.ConsumePendingApprovalParams) (int64, error)
	// ListUnconsumedPendingApprovals backs `kahya approvals` (W3-06):
	// every not-yet-consumed pending_approvals row, oldest first. Expiry
	// is filtered in Go (ListPendingApprovals below), not SQL - see that
	// query's own doc comment in queries.sql.
	ListUnconsumedPendingApprovals(ctx context.Context) ([]sqlcgen.PendingApproval, error)

	InsertUndoWindow(ctx context.Context, arg sqlcgen.InsertUndoWindowParams) (sqlcgen.UndoWindow, error)
	GetOpenUndoWindowByTrace(ctx context.Context, traceID string) (sqlcgen.UndoWindow, error)
	GetOpenUndoWindowByTaskToolTrace(ctx context.Context, arg sqlcgen.GetOpenUndoWindowByTaskToolTraceParams) (sqlcgen.UndoWindow, error)
	ListOpenUndoWindows(ctx context.Context) ([]sqlcgen.UndoWindow, error)
	SetUndoWindowState(ctx context.Context, arg sqlcgen.SetUndoWindowStateParams) error
}

var _ Store = (*sqlcgen.Queries)(nil)

// Ledger is the append-only events sink every decision/promotion/demotion
// writes to (HANDOFF S5 safety #4). *store.Store already has exactly this
// method shape (store.Store.LogEvent), so it satisfies this with no
// adapter - mirroring kahyad/internal/server.EventLogger's own shape one
// package over.
type Ledger interface {
	LogEvent(ctx context.Context, traceID, kind string, payload map[string]any) error
}

// Engine is the W3-02 ladder decision engine: one per kahyad process,
// sharing the single *store.Store the rest of the daemon uses.
type Engine struct {
	policy Policy
	store  Store
	ledger Ledger
	// now is time.Now by default; tests substitute a fixed/advancing clock
	// so undo-window/token-expiry logic never needs a real sleep.
	now func() time.Time
	// undoExpiryHook is W3-03's purge seam (SetUndoExpiryHook): called
	// once per undo_windows row this Engine itself flips to "expired"
	// (both SweepExpiredUndoWindows' background sweep and TriggerUndo's
	// own lazy expiry-on-trigger-attempt branch), AFTER the row is
	// already durably marked expired and undo_window_expired is already
	// ledgered. nil by default (every pre-W3-03 caller/test keeps working
	// unchanged) - a set hook lets the owning tool (mcp/fs.Server.
	// PurgeExpired) delete its own fallback pre-image copy without this
	// package needing to know anything about what that tool stores or
	// where.
	undoExpiryHook func(traceID, taskID, tool string)
	// undoWindowDuration is openUndoWindow's own configurable window
	// length (MINOR fix: config.Config.UndoWindowSeconds, defaulted here
	// to the package-level undoWindowDuration const by NewEngine) - see
	// SetUndoWindowDuration's doc comment.
	undoWindowDuration time.Duration
	// pendingApprovalHook is W3-07's subscription seam (SetPendingApprovalHook):
	// called once per freshly-minted pending_approvals row (mintPendingApproval,
	// AFTER the row is already durably persisted), so the Telegram bot can
	// deliver a W1/W2 approval card or a W3 notify-only message without this
	// package needing to know Telegram exists - the exact same "owning
	// surface subscribes via a nil-by-default hook" pattern undoExpiryHook
	// already established for W3-03. nil by default (every pre-W3-07
	// caller/test keeps working unchanged). Called SYNCHRONOUSLY from
	// mintPendingApproval, which runs inside Check/Approve's own request
	// path - a real caller MUST keep this fast (fire off a goroutine for
	// any actual network send) so a slow hook can never delay a policy
	// decision.
	pendingApprovalHook func(PendingApprovalInfo)
}

// NewEngine constructs an Engine. pol is W3-01's loaded tool registry (the
// engine's only source of tool->class metadata - never trust a caller-
// supplied class). store/ledger may not be nil in production; tests pass
// fakes or a real temp *store.Store (kahyad/internal/store).
func NewEngine(pol Policy, store Store, ledger Ledger) *Engine {
	return &Engine{policy: pol, store: store, ledger: ledger, now: time.Now, undoWindowDuration: undoWindowDuration}
}

// SetUndoWindowDuration overrides Engine's undo-window length (MINOR fix:
// config.Config.UndoWindowSeconds, defaulted to the package-level
// undoWindowDuration const of 5 minutes - see main.go's wiring). Every
// window opened by a Check/Approve call AFTER this is called uses d;
// windows already open keep their originally-recorded deadline. Tests
// use this to inject a short window so purge-on-expiry can be exercised
// end-to-end through the real SweepExpiredUndoWindows -> SetUndoExpiryHook
// path instead of only ever calling the owning tool's purge directly.
func (e *Engine) SetUndoWindowDuration(d time.Duration) { e.undoWindowDuration = d }

// SetUndoExpiryHook registers fn to be called whenever this Engine flips
// an undo_windows row to "expired" (W3-03 task spec: "Purge fallback
// pre-image copies when the 5-minute window expires — hook the
// undo_window_expired event"). Call before the undo sweeper goroutine
// starts (main.go); nil (the default) means no hook runs.
func (e *Engine) SetUndoExpiryHook(fn func(traceID, taskID, tool string)) {
	e.undoExpiryHook = fn
}

// SetPendingApprovalHook registers fn to be called once for every freshly
// minted pending_approvals row (see pendingApprovalHook's own doc comment
// on the Engine struct). Call before any Check/Approve traffic starts
// flowing; nil (the default) means no hook runs, so every pre-W3-07
// caller/test is unaffected.
func (e *Engine) SetPendingApprovalHook(fn func(PendingApprovalInfo)) {
	e.pendingApprovalHook = fn
}

// fireUndoExpiryHook calls the registered hook, if any - a small helper so
// SweepExpiredUndoWindows and TriggerUndo's own expiry branch share
// exactly one nil-check instead of duplicating it.
func (e *Engine) fireUndoExpiryHook(traceID, taskID, tool string) {
	if e.undoExpiryHook != nil {
		e.undoExpiryHook(traceID, taskID, tool)
	}
}

// SetClock overrides Engine's clock (tests only).
func (e *Engine) SetClock(now func() time.Time) { e.now = now }

// nowUTC is the one place Engine reads the clock, always normalized to UTC
// (matching every other timestamp convention in this codebase).
func (e *Engine) nowUTC() time.Time { return e.now().UTC() }

func rfc3339(t time.Time) string { return t.Format(time.RFC3339Nano) }

// CheckInput is Engine.Check's only input shape (HANDOFF S5 product
// principle: "the decision function's inputs are ONLY policy
// registration, autonomy_state, class, scope, taint/session flags" - Scope
// is the one caller-supplied value; Class is NEVER accepted from a
// caller, only ever resolved from the loaded Policy). ToolInput is hashed
// (sha256) to bind a freshly-minted token/pending-approval to these exact
// bytes (HANDOFF S5 safety #5 WYSIWYE, until W3-06 lands the real
// normalize+hash pipeline - see approvedBytesHash's doc comment).
type CheckInput struct {
	Tool      string
	Scope     string
	TaskID    string
	TraceID   string
	ToolInput []byte
}

// Decision is one Engine.Check outcome.
type Decision struct {
	Result string // ResultAllow | ResultNeedsApproval | ResultDeny
	Reason string // Turkish, non-empty on Deny/NeedsApproval
	Rule   string
	Class  ActionClass
	Scope  string
	Level  int
	// PendingApprovalID is set iff Result == ResultNeedsApproval: an
	// opaque, self-describing reference an approval surface passes back to
	// Engine.Approve/Deny (kahyad's POST /policy/feedback).
	PendingApprovalID string
	// Token is set iff Result == ResultAllow AND the tool's class is
	// side-effectful (W1/W2 - never R, which needs no consume-token step,
	// and never W3, which never reaches ResultAllow at all). The caller
	// (a side-effectful MCP tool) MUST present this to
	// Engine.ConsumeToken/POST /policy/consume-token immediately before
	// executing.
	Token string
}

// pendingApprovalIDBytesLen is the pending_approval_id's raw (pre-hex)
// length: 32 random bytes, the same size/entropy convention tokens.go's
// tokenBytesLen already uses for one-time approval tokens.
const pendingApprovalIDBytesLen = 32

// pendingApprovalTTL is the NEEDS_APPROVAL pending-approval id's
// time-to-live (this task's spec, verbatim: "TTL 10 dakika" - mirroring
// tokens.go's tokenTTL).
const pendingApprovalTTL = 10 * time.Minute

// ErrInvalidPendingApproval is returned by Approve/Deny for every failure
// branch (forged/unrecognized id, expired, or already-consumed) - callers
// must treat all of these identically: fail-closed, no token minted, no
// bookkeeping performed.
var ErrInvalidPendingApproval = errors.New("policy: invalid or unrecognized pending_approval_id")

// mintPendingApproval is the server-side truth behind a NEEDS_APPROVAL
// decision's pending_approval_id (post-security-review amendment - this
// USED TO BE an unsigned base64(json) blob a caller could hand-craft to
// mint a token for ANY tool/class/scope; it is now 32 random bytes bound
// to a pending_approvals row holding the RESOLVED (tool, class, scope,
// task_id, trace_id, approved_bytes_hash) Check itself just computed - the
// id is opaque and self-describing to nobody but this row). Approve/Deny
// below look this row up by id and never trust anything decoded from the
// id string itself. toolInput (W3-06) is persisted alongside the hash so
// `kahya approvals`/`kahya approve <id>` can render the real byte-exact
// WYSIWYE diff, not just the one-way hash.
func (e *Engine) mintPendingApproval(ctx context.Context, taskID, traceID, tool string, class ActionClass, scope, hash string, toolInput []byte) (string, error) {
	raw := make([]byte, pendingApprovalIDBytesLen)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	id := hex.EncodeToString(raw)

	now := e.nowUTC()
	if err := e.store.InsertPendingApproval(ctx, sqlcgen.InsertPendingApprovalParams{
		ID:                id,
		TaskID:            taskID,
		TraceID:           traceID,
		Tool:              tool,
		Class:             string(class),
		Scope:             scope,
		ApprovedBytesHash: hash,
		ToolInput:         toolInput,
		MintedAt:          rfc3339(now),
		ExpiresAt:         rfc3339(now.Add(pendingApprovalTTL)),
	}); err != nil {
		return "", err
	}
	if e.pendingApprovalHook != nil {
		e.pendingApprovalHook(PendingApprovalInfo{
			ID: id, Tool: tool, Class: class, Scope: scope,
			ToolInput: toolInput, MintedAt: now,
			TraceID: traceID, TaskID: taskID,
		})
	}
	return id, nil
}

// getValidPendingApproval fetches (WITHOUT consuming) a pending_approvals
// row by id, rejecting a missing, expired, or already-consumed one with
// ErrInvalidPendingApproval. This is a read-only peek, deliberately kept
// separate from consumePendingApproval below, so Approve can enforce the
// W3 surface="local" rule BEFORE burning the id: a Telegram surface
// rejected on a W3 pending approval must leave that SAME id usable for a
// later local approval, not consume it on the failed attempt.
func (e *Engine) getValidPendingApproval(ctx context.Context, id string) (sqlcgen.PendingApproval, error) {
	pa, err := e.store.GetPendingApproval(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return sqlcgen.PendingApproval{}, ErrInvalidPendingApproval
	}
	if err != nil {
		return sqlcgen.PendingApproval{}, err
	}
	if pa.ConsumedAt.Valid {
		return sqlcgen.PendingApproval{}, ErrInvalidPendingApproval
	}
	if expiresAt, perr := time.Parse(time.RFC3339Nano, pa.ExpiresAt); perr != nil || e.nowUTC().After(expiresAt) {
		return sqlcgen.PendingApproval{}, ErrInvalidPendingApproval
	}
	return pa, nil
}

// consumePendingApproval atomically single-use-burns a pending_approvals
// row already validated by getValidPendingApproval above (BLOCKER 2:
// Approve/Deny must never act twice on the same pending_approval_id).
// Only the FIRST caller to reach this for a given id ever succeeds; a
// second (or racing concurrent) call affects 0 rows and is rejected -
// before any token is minted or any bookkeeping happens.
func (e *Engine) consumePendingApproval(ctx context.Context, id string) error {
	affected, err := e.store.ConsumePendingApproval(ctx, sqlcgen.ConsumePendingApprovalParams{
		ConsumedAt: sql.NullString{String: rfc3339(e.nowUTC()), Valid: true},
		ID:         id,
	})
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrInvalidPendingApproval
	}
	return nil
}

// Check is the binding policy decision (HANDOFF S5 enforcement plane:
// "Baglayici politika karari kahyad'da verilir"). Every call writes
// exactly one events row (kind="policy_decision") regardless of outcome.
func (e *Engine) Check(ctx context.Context, in CheckInput) (Decision, error) {
	in.Tool = Canonicalize(in.Tool)
	scope := normalizeScope(in.Scope)

	tool, ok := e.policy.ToolsByName[in.Tool]
	if !ok {
		d := Decision{Result: ResultDeny, Reason: ReasonUnknownTool, Rule: RuleLadderV1, Scope: scope}
		e.ledgerDecision(ctx, in, "", scope, 0, d)
		return d, nil
	}
	class := tool.Class
	hash := approvedBytesHash(in.ToolInput)

	state, err := e.loadState(ctx, in.Tool, class, scope)
	if err != nil {
		d := Decision{Result: ResultDeny, Reason: ReasonPolicyStateError, Rule: RuleLadderV1, Class: class, Scope: scope}
		e.ledgerDecision(ctx, in, class, scope, 0, d)
		return d, err
	}
	level := int(state.Level)

	// HARD-CODED: W3 never auto-allows, at any level, ever - see this
	// file's package doc comment and autoLevelFor's.
	if class == ClassW3 {
		return e.needsApproval(ctx, in, class, scope, level, hash, ReasonW3AlwaysApproval)
	}

	threshold, _ := autoLevelFor(class)
	if level >= threshold {
		var token string
		if class != ClassR {
			tok, err := e.mintToken(ctx, in.TaskID, in.TraceID, in.Tool, class, scope, hash)
			if err != nil {
				d := Decision{Result: ResultDeny, Reason: ReasonPolicyStateError, Rule: RuleLadderV1, Class: class, Scope: scope, Level: level}
				e.ledgerDecision(ctx, in, class, scope, level, d)
				return d, err
			}
			token = tok
			if class == ClassW1 {
				// Best-effort: a failure here must not retroactively flip an
				// already-decided ALLOW to deny (the token above is already
				// minted and valid) - it is ledgered for visibility instead.
				if err := e.openUndoWindow(ctx, in.TaskID, in.Tool, in.TraceID); err != nil {
					e.ledgerRaw(ctx, in.TraceID, "undo_window_open_failed", map[string]any{
						"event": "undo_window_open_failed", "task_id": in.TaskID, "tool": in.Tool, "err": err.Error(),
					})
				}
			}
		}
		d := Decision{Result: ResultAllow, Rule: RuleLadderV1, Class: class, Scope: scope, Level: level, Token: token}
		e.ledgerDecision(ctx, in, class, scope, level, d)
		return d, nil
	}

	return e.needsApproval(ctx, in, class, scope, level, hash, ReasonNeedsApproval)
}

// needsApproval mints the server-side pending_approvals row backing a
// NEEDS_APPROVAL decision (BLOCKER 1/2: pending_approval_id is server-
// issued and DB-tracked, never an unsigned caller-decodable blob) and
// returns the resulting Decision. A mint failure itself fails closed
// (DENY) - a NEEDS_APPROVAL decision with no corresponding row would be
// approvable by nothing, which is a worse failure mode than an honest
// deny.
func (e *Engine) needsApproval(ctx context.Context, in CheckInput, class ActionClass, scope string, level int, hash, reason string) (Decision, error) {
	id, err := e.mintPendingApproval(ctx, in.TaskID, in.TraceID, in.Tool, class, scope, hash, in.ToolInput)
	if err != nil {
		d := Decision{Result: ResultDeny, Reason: ReasonPolicyStateError, Rule: RuleLadderV1, Class: class, Scope: scope, Level: level}
		e.ledgerDecision(ctx, in, class, scope, level, d)
		return d, err
	}
	d := Decision{Result: ResultNeedsApproval, Reason: reason, Rule: RuleLadderV1, Class: class, Scope: scope, Level: level, PendingApprovalID: id}
	e.ledgerDecision(ctx, in, class, scope, level, d)
	return d, nil
}

// ledgerDecision writes the ONE events row every Check call produces
// (HANDOFF S5 safety #4 / this task's acceptance criteria: {event:
// "policy_decision", tool, class, scope, level, decision, trace_id,
// task_id}). The "event" key is included INSIDE payload (redundant with
// the kind param) specifically so `json_extract(payload,'$.event')`
// resolves directly, matching this task's own acceptance-criteria SQL
// verbatim.
func (e *Engine) ledgerDecision(ctx context.Context, in CheckInput, class ActionClass, scope string, level int, d Decision) {
	if e.ledger == nil {
		return
	}
	payload := map[string]any{
		"event":    "policy_decision",
		"tool":     in.Tool,
		"class":    class,
		"scope":    scope,
		"level":    level,
		"decision": d.Result,
		"task_id":  in.TaskID,
	}
	if d.Reason != "" {
		payload["reason"] = d.Reason
	}
	_ = e.ledger.LogEvent(ctx, in.TraceID, "policy_decision", payload)
}

// ledgerRaw is a small helper for the handful of non-policy_decision
// ledger events this file emits (undo_window_opened/expired, demoted,
// promotion_suggested, policy_feedback_*) - every payload already carries
// its own "event" key (see ledgerDecision's doc comment for why).
func (e *Engine) ledgerRaw(ctx context.Context, traceID, kind string, payload map[string]any) {
	if e.ledger == nil {
		return
	}
	_ = e.ledger.LogEvent(ctx, traceID, kind, payload)
}

// loadState reads the (tool,class,scope) autonomy_state row, defaulting to
// a zero-value (L0, 0 approvals) row when none exists yet - HANDOFF S4:
// "missing row => L0".
func (e *Engine) loadState(ctx context.Context, tool string, class ActionClass, scope string) (sqlcgen.AutonomyState, error) {
	row, err := e.store.GetAutonomyState(ctx, sqlcgen.GetAutonomyStateParams{Tool: tool, Class: string(class), Scope: scope})
	if errors.Is(err, sql.ErrNoRows) {
		return sqlcgen.AutonomyState{Tool: tool, Class: string(class), Scope: scope}, nil
	}
	return row, err
}

// saveState is an application-level upsert (UpdateAutonomyState first; on
// 0 rows affected, InsertAutonomyState) - the same pattern
// UpdateEpisodeContent/GetEpisodeByPath already use elsewhere in this
// codebase, avoiding any reliance on sqlite ON CONFLICT syntax in the
// generated sqlc query.
func (e *Engine) saveState(ctx context.Context, tool string, class ActionClass, scope string, level, approvals int) error {
	now := rfc3339(e.nowUTC())
	n, err := e.store.UpdateAutonomyState(ctx, sqlcgen.UpdateAutonomyStateParams{
		Level: int64(level), ConsecutiveApprovals: int64(approvals), UpdatedAt: now,
		Tool: tool, Class: string(class), Scope: scope,
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return e.store.InsertAutonomyState(ctx, sqlcgen.InsertAutonomyStateParams{
			Tool: tool, Class: string(class), Scope: scope,
			Level: int64(level), ConsecutiveApprovals: int64(approvals), UpdatedAt: now,
		})
	}
	return nil
}

// demote implements HANDOFF S4's "Tenzil: red / geri-alma / guvenlik
// ihlali -> bir seviye duser" - floor L0, counter always reset to 0
// regardless of the floor being hit, and a "demoted" ledger event is
// always written (even a no-op demotion at L0 is evidence a violation
// happened).
func (e *Engine) demote(ctx context.Context, tool string, class ActionClass, scope, traceID, reason string) {
	state, err := e.loadState(ctx, tool, class, scope)
	if err != nil {
		return // fail-closed elsewhere already denied the triggering action
	}
	from := int(state.Level)
	to := from - 1
	if to < 0 {
		to = 0
	}
	if err := e.saveState(ctx, tool, class, scope, to, 0); err != nil {
		return
	}
	e.ledgerRaw(ctx, traceID, "demoted", map[string]any{
		"event": "demoted", "tool": tool, "class": class, "scope": scope,
		"from_level": from, "to_level": to, "reason": reason,
	})
}

// openUndoWindow inserts an undo_windows row with a 5-minute deadline and
// ledgers undo_window_opened (HANDOFF S4 ladder L2 row). MINOR 5 fix:
// idempotent open - a retried Check/Approve call for the same (task_id,
// tool, trace_id) reuses the existing OPEN window instead of opening a
// second one, so a retry never leaves multiple simultaneously-open undo
// windows for the same action.
func (e *Engine) openUndoWindow(ctx context.Context, taskID, tool, traceID string) error {
	if _, err := e.store.GetOpenUndoWindowByTaskToolTrace(ctx, sqlcgen.GetOpenUndoWindowByTaskToolTraceParams{
		TaskID: taskID, Tool: tool, TraceID: traceID,
	}); err == nil {
		return nil // already open - nothing to do
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	now := e.nowUTC()
	row, err := e.store.InsertUndoWindow(ctx, sqlcgen.InsertUndoWindowParams{
		TaskID: taskID, Tool: tool, TraceID: traceID,
		OpenedAt: rfc3339(now), Deadline: rfc3339(now.Add(e.undoWindowDuration)),
	})
	if err != nil {
		return err
	}
	e.ledgerRaw(ctx, traceID, "undo_window_opened", map[string]any{
		"event": "undo_window_opened", "task_id": taskID, "tool": tool, "deadline": row.Deadline,
	})
	return nil
}

// SweepExpiredUndoWindows flips every still-open undo_windows row whose
// deadline has passed to state="expired", ledgering undo_window_expired
// for each. main.go runs this on an interval goroutine (RunUndoSweeper);
// it is also safe to call directly (tests do).
func (e *Engine) SweepExpiredUndoWindows(ctx context.Context) (int, error) {
	rows, err := e.store.ListOpenUndoWindows(ctx)
	if err != nil {
		return 0, err
	}
	now := e.nowUTC()
	n := 0
	for _, w := range rows {
		deadline, err := time.Parse(time.RFC3339Nano, w.Deadline)
		if err != nil || now.Before(deadline) {
			continue
		}
		if err := e.store.SetUndoWindowState(ctx, sqlcgen.SetUndoWindowStateParams{State: "expired", ID: w.ID}); err != nil {
			continue
		}
		n++
		e.ledgerRaw(ctx, w.TraceID, "undo_window_expired", map[string]any{
			"event": "undo_window_expired", "task_id": w.TaskID, "tool": w.Tool,
		})
		e.fireUndoExpiryHook(w.TraceID, w.TaskID, w.Tool)
	}
	return n, nil
}

// RunUndoSweeper runs SweepExpiredUndoWindows every interval until ctx is
// cancelled (main.go: the same "goroutine cancelled by the daemon's own
// shutdown context" pattern the boot reindex goroutine already uses).
func (e *Engine) RunUndoSweeper(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_, _ = e.SweepExpiredUndoWindows(ctx)
		}
	}
}

// ErrNoOpenUndoWindow is returned by TriggerUndo when no open (and
// unexpired) undo_windows row matches traceID.
var ErrNoOpenUndoWindow = errors.New("policy: no open undo window for this trace")

// TriggerUndo implements `kahya undo --trace <id>`'s server-side half: it
// finds the most recent OPEN undo_windows row for traceID, marks it
// "triggered", demotes the owning (tool,class,scope) triple (HANDOFF S4:
// undo is a demotion trigger), and ledgers undo_triggered. Recipe
// EXECUTION itself (Trash restore, git checkpoint restore, ...) is
// delegated to the owning tool (W3-03) - this is only the window/trigger
// plumbing, per this task's Out of scope.
//
// Scope is not recorded on undo_windows (the migration's fixed 3-table
// schema has no scope column there - see approval_tokens' identical
// limitation in tokens.go), so the demotion below targets scope="global"
// as a documented simplification; per-resource undo-triggered demotion
// scoping is deferred alongside full scope-value plumbing.
func (e *Engine) TriggerUndo(ctx context.Context, traceID string) (sqlcgen.UndoWindow, error) {
	row, err := e.store.GetOpenUndoWindowByTrace(ctx, traceID)
	if errors.Is(err, sql.ErrNoRows) {
		return sqlcgen.UndoWindow{}, ErrNoOpenUndoWindow
	}
	if err != nil {
		return sqlcgen.UndoWindow{}, err
	}

	now := e.nowUTC()
	if deadline, perr := time.Parse(time.RFC3339Nano, row.Deadline); perr == nil && now.After(deadline) {
		_ = e.store.SetUndoWindowState(ctx, sqlcgen.SetUndoWindowStateParams{State: "expired", ID: row.ID})
		e.ledgerRaw(ctx, traceID, "undo_window_expired", map[string]any{
			"event": "undo_window_expired", "task_id": row.TaskID, "tool": row.Tool,
		})
		e.fireUndoExpiryHook(traceID, row.TaskID, row.Tool)
		return sqlcgen.UndoWindow{}, ErrNoOpenUndoWindow
	}

	if err := e.store.SetUndoWindowState(ctx, sqlcgen.SetUndoWindowStateParams{State: "triggered", ID: row.ID}); err != nil {
		return sqlcgen.UndoWindow{}, err
	}
	e.ledgerRaw(ctx, traceID, "undo_triggered", map[string]any{
		"event": "undo_triggered", "task_id": row.TaskID, "tool": row.Tool,
	})

	class := ActionClass("")
	if t, ok := e.policy.ToolsByName[row.Tool]; ok {
		class = t.Class
	}
	e.demote(ctx, row.Tool, class, defaultScope, traceID, "undo")
	return row, nil
}

// FeedbackResult is Engine.Approve's return value.
type FeedbackResult struct {
	Token string
}

// ErrW3RequiresLocalSurface is returned by Approve when a pending W3
// approval's surface isn't "local" (HANDOFF S5 safety #5: "W3 yazili
// 'onayla' YALNIZ yerel yuzeyden kabul edilir" - Telegram may notify a W3
// pending approval exists, it may never itself carry the approval).
var ErrW3RequiresLocalSurface = errors.New("policy: W3 approval must carry surface=local")

// Approve implements POST /policy/feedback's approve outcome: look up the
// pending_approvals row by id (BLOCKER 1 - never trust anything decoded
// from the id itself), enforce the W3 surface=local hard rule against the
// ROW's real class, atomically single-use-consume the row (BLOCKER 2 -
// missing/expired/already-consumed all reject with no token minted), mint
// a one-time token bound to the row's task_id+approved_bytes_hash, open a
// W1 undo window (a manually-approved W1 write earns the same 5-minute
// safety net an auto-allowed one gets), bump consecutive_approvals, and -
// exactly at the 20th consecutive approval - ledger promotion_suggested
// (the level itself never changes here; only `kahya autonomy promote`
// changes it, per HANDOFF S4).
func (e *Engine) Approve(ctx context.Context, pendingApprovalID, surface string) (FeedbackResult, error) {
	pa, err := e.getValidPendingApproval(ctx, pendingApprovalID)
	if err != nil {
		return FeedbackResult{}, err
	}
	class := ActionClass(pa.Class)

	// Checked BEFORE consuming: a W3 pending approval rejected here (wrong
	// surface) must remain usable for a LATER local approval, not be
	// burned by the failed attempt.
	//
	// Ledger kind is EXACTLY "w3_nonlocal_approval_rejected" (this task's
	// own spec, verbatim) - Go-side, arrives before W3-07 (Telegram) even
	// exists, so Telegram can never be wired to approve a W3 action later:
	// this check runs regardless of which surface ever calls
	// POST /policy/feedback.
	if class == ClassW3 && surface != "local" {
		e.ledgerRaw(ctx, pa.TraceID, "w3_nonlocal_approval_rejected", map[string]any{
			"event": "w3_nonlocal_approval_rejected", "tool": pa.Tool, "class": class, "scope": pa.Scope, "surface": surface,
		})
		return FeedbackResult{}, ErrW3RequiresLocalSurface
	}

	if err := e.consumePendingApproval(ctx, pendingApprovalID); err != nil {
		return FeedbackResult{}, err
	}

	token, err := e.mintToken(ctx, pa.TaskID, pa.TraceID, pa.Tool, class, pa.Scope, pa.ApprovedBytesHash)
	if err != nil {
		return FeedbackResult{}, err
	}
	if class == ClassW1 {
		if err := e.openUndoWindow(ctx, pa.TaskID, pa.Tool, pa.TraceID); err != nil {
			e.ledgerRaw(ctx, pa.TraceID, "undo_window_open_failed", map[string]any{
				"event": "undo_window_open_failed", "task_id": pa.TaskID, "tool": pa.Tool, "err": err.Error(),
			})
		}
	}

	state, err := e.loadState(ctx, pa.Tool, class, pa.Scope)
	if err == nil {
		newCount := int(state.ConsecutiveApprovals) + 1
		if err := e.saveState(ctx, pa.Tool, class, pa.Scope, int(state.Level), newCount); err == nil {
			approvedPayload := map[string]any{
				"event": "policy_feedback_approved", "tool": pa.Tool, "class": class,
				"scope": pa.Scope, "consecutive_approvals": newCount, "surface": surface,
			}
			// MINOR B fix: HANDOFF §5 + BACKLOG require every
			// non-local-surface (today: Telegram) approval be ledgered
			// with a "remote" label, not merely surface:"<name>" - a
			// ledger reader must be able to select "every remotely
			// approved action" without hardcoding every current/future
			// non-local surface name. surface=="local" never gets this
			// key at all (rather than remote:false), matching this
			// package's existing "presence of a key signals the
			// condition" convention elsewhere in this event vocabulary.
			if surface != "local" {
				approvedPayload["remote"] = true
			}
			e.ledgerRaw(ctx, pa.TraceID, "policy_feedback_approved", approvedPayload)
			if newCount == promotionThreshold {
				e.ledgerRaw(ctx, pa.TraceID, "promotion_suggested", map[string]any{
					"event": "promotion_suggested", "tool": pa.Tool, "class": class,
					"scope": pa.Scope, "consecutive_approvals": newCount,
				})
			}
		}
	}

	return FeedbackResult{Token: token}, nil
}

// Deny implements POST /policy/feedback's deny outcome: look up the
// pending_approvals row by id (BLOCKER 1), atomically single-use-consume
// it (BLOCKER 2 - a missing/expired/already-consumed id rejects, and a
// double-deny can only ever demote once), demote the row's REAL
// (tool,class,scope) triple (HANDOFF S4: "red -> bir seviye duser"), and
// ledger the denial. No token is minted - there is nothing to execute.
func (e *Engine) Deny(ctx context.Context, pendingApprovalID string) error {
	pa, err := e.getValidPendingApproval(ctx, pendingApprovalID)
	if err != nil {
		return err
	}
	if err := e.consumePendingApproval(ctx, pendingApprovalID); err != nil {
		return err
	}
	class := ActionClass(pa.Class)
	e.ledgerRaw(ctx, pa.TraceID, "policy_feedback_denied", map[string]any{
		"event": "policy_feedback_denied", "tool": pa.Tool, "class": class, "scope": pa.Scope,
	})
	e.demote(ctx, pa.Tool, class, pa.Scope, pa.TraceID, "denied")
	return nil
}

// Promote implements `kahya autonomy promote <tool> <class> <scope>` - the
// ONLY promotion path (HANDOFF S4: "kullanici onaylamadan asla otomatik
// terfi olmaz"). It moves the (tool,class,scope) triple's level up by
// exactly one step (floor at L4) and resets consecutive_approvals to 0,
// so the next promotion cycle starts counting fresh.
func (e *Engine) Promote(ctx context.Context, traceID, tool string, class ActionClass, scope string) (int, error) {
	if _, ok := e.policy.ToolsByName[tool]; !ok {
		return 0, fmt.Errorf("policy: unknown tool %q", tool)
	}
	if !validClasses[class] {
		return 0, fmt.Errorf("policy: invalid class %q", class)
	}
	scope = normalizeScope(scope)

	state, err := e.loadState(ctx, tool, class, scope)
	if err != nil {
		return 0, err
	}
	from := int(state.Level)
	to := from + 1
	if to > L4 {
		to = L4
	}
	if err := e.saveState(ctx, tool, class, scope, to, 0); err != nil {
		return 0, err
	}
	e.ledgerRaw(ctx, traceID, "promoted", map[string]any{
		"event": "promoted", "tool": tool, "class": class, "scope": scope,
		"from_level": from, "to_level": to,
	})
	return to, nil
}

// ListState implements GET /policy/state's ladder dump.
func (e *Engine) ListState(ctx context.Context) ([]sqlcgen.AutonomyState, error) {
	return e.store.ListAutonomyState(ctx)
}

// PendingApprovalInfo is one Engine.ListPendingApprovals/GetPendingApprovalDetail
// row: everything `kahya approvals`/`kahya approve <id>` needs (this
// task's spec: "id, tool, class, summary, age" for the list; ToolInput
// additionally for the detail render).
type PendingApprovalInfo struct {
	ID        string
	Tool      string
	Class     ActionClass
	Scope     string
	ToolInput []byte
	MintedAt  time.Time
	// TraceID/TaskID are the correlation ids Check originally resolved this
	// decision under (W3-07's pendingApprovalHook needs both: TraceID for
	// its own egress-checked-send/ledger lines, TaskID purely for
	// completeness) - not needed by `kahya approvals`/`kahya approve`'s own
	// CLI rendering, which is why these were absent before W3-07 added the
	// hook that needs them.
	TraceID string
	TaskID  string
}

// ListPendingApprovals implements `kahya approvals`: every not-yet-
// consumed, not-yet-EXPIRED pending_approvals row, oldest first. Expiry
// is checked here in Go (time.Parse+time.After), mirroring
// getValidPendingApproval's own check, rather than in SQL - see
// ListUnconsumedPendingApprovals' own doc comment in queries.sql for why.
func (e *Engine) ListPendingApprovals(ctx context.Context) ([]PendingApprovalInfo, error) {
	rows, err := e.store.ListUnconsumedPendingApprovals(ctx)
	if err != nil {
		return nil, err
	}
	now := e.nowUTC()
	out := make([]PendingApprovalInfo, 0, len(rows))
	for _, r := range rows {
		expiresAt, perr := time.Parse(time.RFC3339Nano, r.ExpiresAt)
		if perr != nil || now.After(expiresAt) {
			continue // expired - not "pending" anymore, even though not yet swept
		}
		mintedAt, perr := time.Parse(time.RFC3339Nano, r.MintedAt)
		if perr != nil {
			mintedAt = now
		}
		out = append(out, PendingApprovalInfo{
			ID: r.ID, Tool: r.Tool, Class: ActionClass(r.Class), Scope: r.Scope,
			ToolInput: r.ToolInput, MintedAt: mintedAt,
			TraceID: r.TraceID, TaskID: r.TaskID,
		})
	}
	return out, nil
}

// GetPendingApprovalDetail implements `kahya approve <id>`'s own lookup:
// the single pending_approvals row (still valid - not expired/consumed)
// identified by id, WITHOUT consuming it (a human reviewing the diff
// before deciding must be able to look, then decide, in two separate
// steps) - approving/denying is still Approve/Deny's job, unchanged.
func (e *Engine) GetPendingApprovalDetail(ctx context.Context, id string) (PendingApprovalInfo, error) {
	pa, err := e.getValidPendingApproval(ctx, id)
	if err != nil {
		return PendingApprovalInfo{}, err
	}
	mintedAt, perr := time.Parse(time.RFC3339Nano, pa.MintedAt)
	if perr != nil {
		mintedAt = e.nowUTC()
	}
	return PendingApprovalInfo{
		ID: pa.ID, Tool: pa.Tool, Class: ActionClass(pa.Class), Scope: pa.Scope,
		ToolInput: pa.ToolInput, MintedAt: mintedAt,
		TraceID: pa.TraceID, TaskID: pa.TaskID,
	}, nil
}
