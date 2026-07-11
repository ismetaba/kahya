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
	"database/sql"
	"encoding/base64"
	"encoding/json"
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

// undoWindowDuration is the W1 5-minute undo grace period (HANDOFF S4
// ladder: "L2 | Eslikci | R, W1 (5-dk geri-alma + defter)").
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

	InsertUndoWindow(ctx context.Context, arg sqlcgen.InsertUndoWindowParams) (sqlcgen.UndoWindow, error)
	GetOpenUndoWindowByTrace(ctx context.Context, traceID string) (sqlcgen.UndoWindow, error)
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
}

// NewEngine constructs an Engine. pol is W3-01's loaded tool registry (the
// engine's only source of tool->class metadata - never trust a caller-
// supplied class). store/ledger may not be nil in production; tests pass
// fakes or a real temp *store.Store (kahyad/internal/store).
func NewEngine(pol Policy, store Store, ledger Ledger) *Engine {
	return &Engine{policy: pol, store: store, ledger: ledger, now: time.Now}
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

// approvalTicket is PendingApprovalID's decoded shape: everything
// Engine.Approve/Deny need to mint a token or ledger a denial, without a
// dedicated "pending approvals" table (the migration's three tables -
// autonomy_state, approval_tokens, undo_windows - are the complete W3-02
// schema; a NEEDS_APPROVAL decision is deliberately NOT persisted as a row
// anywhere - it exists only as this opaque, self-contained reference,
// valid for exactly as long as the approval surface holds onto it).
type approvalTicket struct {
	Tool              string      `json:"tool"`
	Class             ActionClass `json:"class"`
	Scope             string      `json:"scope"`
	TaskID            string      `json:"task_id"`
	TraceID           string      `json:"trace_id"`
	ApprovedBytesHash string      `json:"approved_bytes_hash"`
}

func encodeTicket(t approvalTicket) string {
	b, _ := json.Marshal(t)
	return base64.RawURLEncoding.EncodeToString(b)
}

var ErrInvalidPendingApproval = errors.New("policy: invalid or unrecognized pending_approval_id")

func decodeTicket(s string) (approvalTicket, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return approvalTicket{}, ErrInvalidPendingApproval
	}
	var t approvalTicket
	if err := json.Unmarshal(b, &t); err != nil {
		return approvalTicket{}, ErrInvalidPendingApproval
	}
	if t.Tool == "" || !validClasses[t.Class] {
		return approvalTicket{}, ErrInvalidPendingApproval
	}
	return t, nil
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
		id := encodeTicket(approvalTicket{Tool: in.Tool, Class: class, Scope: scope, TaskID: in.TaskID, TraceID: in.TraceID, ApprovedBytesHash: hash})
		d := Decision{Result: ResultNeedsApproval, Reason: ReasonW3AlwaysApproval, Rule: RuleLadderV1, Class: class, Scope: scope, Level: level, PendingApprovalID: id}
		e.ledgerDecision(ctx, in, class, scope, level, d)
		return d, nil
	}

	threshold, _ := autoLevelFor(class)
	if level >= threshold {
		var token string
		if class != ClassR {
			tok, err := e.mintToken(ctx, in.TaskID, in.TraceID, in.Tool, hash)
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

	id := encodeTicket(approvalTicket{Tool: in.Tool, Class: class, Scope: scope, TaskID: in.TaskID, TraceID: in.TraceID, ApprovedBytesHash: hash})
	d := Decision{Result: ResultNeedsApproval, Reason: ReasonNeedsApproval, Rule: RuleLadderV1, Class: class, Scope: scope, Level: level, PendingApprovalID: id}
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
// ledgers undo_window_opened (HANDOFF S4 ladder L2 row).
func (e *Engine) openUndoWindow(ctx context.Context, taskID, tool, traceID string) error {
	now := e.nowUTC()
	row, err := e.store.InsertUndoWindow(ctx, sqlcgen.InsertUndoWindowParams{
		TaskID: taskID, Tool: tool, TraceID: traceID,
		OpenedAt: rfc3339(now), Deadline: rfc3339(now.Add(undoWindowDuration)),
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

// Approve implements POST /policy/feedback's approve outcome: decode the
// pending_approval_id, enforce the W3 surface=local hard rule, mint a
// one-time token bound to the ticket's task_id+approved_bytes_hash, open a
// W1 undo window (a manually-approved W1 write earns the same 5-minute
// safety net an auto-allowed one gets), bump consecutive_approvals, and -
// exactly at the 20th consecutive approval - ledger promotion_suggested
// (the level itself never changes here; only `kahya autonomy promote`
// changes it, per HANDOFF S4).
func (e *Engine) Approve(ctx context.Context, pendingApprovalID, surface string) (FeedbackResult, error) {
	ticket, err := decodeTicket(pendingApprovalID)
	if err != nil {
		return FeedbackResult{}, err
	}

	if ticket.Class == ClassW3 && surface != "local" {
		e.ledgerRaw(ctx, ticket.TraceID, "policy_feedback_rejected", map[string]any{
			"event": "policy_feedback_rejected", "tool": ticket.Tool, "reason": "w3_requires_local_surface", "surface": surface,
		})
		return FeedbackResult{}, ErrW3RequiresLocalSurface
	}

	token, err := e.mintToken(ctx, ticket.TaskID, ticket.TraceID, ticket.Tool, ticket.ApprovedBytesHash)
	if err != nil {
		return FeedbackResult{}, err
	}
	if ticket.Class == ClassW1 {
		if err := e.openUndoWindow(ctx, ticket.TaskID, ticket.Tool, ticket.TraceID); err != nil {
			e.ledgerRaw(ctx, ticket.TraceID, "undo_window_open_failed", map[string]any{
				"event": "undo_window_open_failed", "task_id": ticket.TaskID, "tool": ticket.Tool, "err": err.Error(),
			})
		}
	}

	state, err := e.loadState(ctx, ticket.Tool, ticket.Class, ticket.Scope)
	if err == nil {
		newCount := int(state.ConsecutiveApprovals) + 1
		if err := e.saveState(ctx, ticket.Tool, ticket.Class, ticket.Scope, int(state.Level), newCount); err == nil {
			e.ledgerRaw(ctx, ticket.TraceID, "policy_feedback_approved", map[string]any{
				"event": "policy_feedback_approved", "tool": ticket.Tool, "class": ticket.Class,
				"scope": ticket.Scope, "consecutive_approvals": newCount,
			})
			if newCount == promotionThreshold {
				e.ledgerRaw(ctx, ticket.TraceID, "promotion_suggested", map[string]any{
					"event": "promotion_suggested", "tool": ticket.Tool, "class": ticket.Class,
					"scope": ticket.Scope, "consecutive_approvals": newCount,
				})
			}
		}
	}

	return FeedbackResult{Token: token}, nil
}

// Deny implements POST /policy/feedback's deny outcome: demote the
// pending ticket's (tool,class,scope) triple (HANDOFF S4: "red -> bir
// seviye duser") and ledger the denial. No token is minted - there is
// nothing to execute.
func (e *Engine) Deny(ctx context.Context, pendingApprovalID string) error {
	ticket, err := decodeTicket(pendingApprovalID)
	if err != nil {
		return err
	}
	e.ledgerRaw(ctx, ticket.TraceID, "policy_feedback_denied", map[string]any{
		"event": "policy_feedback_denied", "tool": ticket.Tool, "class": ticket.Class, "scope": ticket.Scope,
	})
	e.demote(ctx, ticket.Tool, ticket.Class, ticket.Scope, ticket.TraceID, "denied")
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
