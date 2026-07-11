// Package egress implements the W3-05 egress gate: the single decision
// engine every off-box byte kahyad ever sends passes through (HANDOFF §5
// safety #1 flag, all three bullets): "Off-box'a byte gonderen her cagri
// ... hedef allowlist + hacim butcesine tabi. Ayni oturumda hassas okuma
// varsa allowlist-disi egress sert bloke", plus the container-egress and
// approval-cards-count-as-egress bullets this package's siblings
// (proxy.go for containers; the W3-07 Telegram sender and W4-05 ledger
// anchor push, both future in-process callers) build on top of.
//
// Check is the ONE exported decision path — kahyad/internal/anthproxy's
// per-task Anthropic forward-proxy (W12-08, wired via
// kahyad/internal/server/task.go's egress-gate factory), this package's
// own proxy.go (container jobs, via the kahya-egress Docker network), the
// Telegram bot (W3-07), and the ledger anchor push (W4-05) all call this
// SAME Gate.Check, so there is exactly one place host-allowlist/budget/
// sensitive-read policy is decided, never a second copy.
//
// Decision order (this task's spec, verbatim — checked in exactly this
// order, first match wins):
//  1. session.SessionID is marked sensitive (SensitiveTracker) AND host is
//     NOT in the allowlist => DENY egress_blocked_sensitive.
//  2. host is NOT in the allowlist => DENY egress_blocked_allowlist.
//  3. the per-host (or default) daily byte budget would be exceeded =>
//     DENY egress_blocked_budget + a Turkish notify "Egress bütçesi
//     aşıldı: <host>".
//  4. otherwise => ALLOW, and nbytes is added to the host's running daily
//     total.
//
// Every decision — allow and deny alike — writes BOTH an append-only
// ledger row (HANDOFF §5 safety #4) and a JSONL log line, both carrying
// the caller's trace_id (this task's own acceptance criterion).
package egress

import (
	"context"
	"fmt"
	"sync"
	"time"

	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/policy"
)

// Target is one egress destination: a host (any textual form —
// Gate.Check canonicalizes it) and a TCP port.
type Target struct {
	Host string
	Port int
}

// SessionInfo threads correlation/taint identifiers through Check.
// SessionID is the SensitiveTracker lookup key (sensitive.go) — empty
// means "no session to taint-check", never a denial on that basis alone.
// TaskID/TraceID are carried into every ledger/JSONL line this call
// produces; TraceID is also what the log line is scoped under (an empty
// TraceID is replaced by a freshly minted one, matching logx.Logger.With's
// own "never an empty trace_id" convention).
type SessionInfo struct {
	SessionID string
	TaskID    string
	TraceID   string
}

// Decision is Check's result.
type Decision struct {
	Allow bool
	// Reason is Turkish (CLAUDE.md language policy), non-empty iff !Allow.
	Reason string
	// Rule is the ledger/JSONL event kind this decision produced — one of
	// the Event* constants below.
	Rule string
}

// Ledger is the append-only events sink every decision writes to (HANDOFF
// §5 safety #4). *store.Store already has exactly this method shape.
type Ledger interface {
	LogEvent(ctx context.Context, traceID, kind string, payload map[string]any) error
}

// Notifier is the narrow alarm surface Check's budget-exceeded branch
// calls. kahyad/internal/notify.Notifier already has exactly this method
// shape.
type Notifier interface {
	Alarm(ctx context.Context, traceID, kind, message string, payload map[string]any) error
}

// Budget is the narrow per-host daily byte-counter surface Gate needs.
// budget.go's SQLBudget satisfies it against the real egress_budget
// table; tests inject an in-memory fake.
type Budget interface {
	// Bytes returns host's currently recorded byte count for day (0 if no
	// row exists yet).
	Bytes(ctx context.Context, host, day string) (int64, error)
	// Add increments host/day's counter by n (creating the row if
	// absent) and returns the NEW total.
	Add(ctx context.Context, host, day string, n int64) (int64, error)
}

// Ledger/JSONL event kinds (HANDOFF §5 safety #4 — every decision is
// observable both ways).
const (
	EventAllowed          = "egress_allowed"
	EventBlockedSensitive = "egress_blocked_sensitive"
	EventBlockedAllowlist = "egress_blocked_allowlist"
	EventBlockedBudget    = "egress_blocked_budget"
	// EventSensitiveMarked is sensitive.go's ledger kind (kept here
	// alongside the Check-decision kinds since both are part of this
	// package's one observable event vocabulary).
	EventSensitiveMarked = "sensitive_read_marked"
	// EventMetered is proxy.go's post-hoc CONNECT-tunnel byte-accounting
	// event — bookkeeping, not a decision (the tunnel already happened by
	// the time this fires), so it is logged separately from the four
	// decision kinds above.
	EventMetered = "egress_metered"
)

// Turkish, user-facing deny reasons (CLAUDE.md language policy).
const (
	reasonSensitiveFmt = "Egress reddedildi: bu oturumda hassas (gizli-şerit) okuma yapıldı; %s izin verilenler listesinde değil."
	reasonAllowlistFmt = "Egress reddedildi: %s izin verilenler listesinde değil."
	reasonPrivateFmt   = "Egress reddedildi: %s özel/link-local bir ağ adresi ve izin verilenler listesinde açıkça yer almıyor."
	// reasonBudgetFmt is ALSO the exact Turkish notify message this
	// task's spec fixes verbatim: "Egress bütçesi aşıldı: <host>".
	reasonBudgetFmt = "Egress bütçesi aşıldı: %s"
)

// allowEntry is one canonicalized allowlist entry.
type allowEntry struct {
	ports map[int]bool // nil/empty means "any port"
}

// Gate is the W3-05 decision engine: one per kahyad process, shared by
// every in-process caller (this package's doc comment).
type Gate struct {
	allowlist              map[string]allowEntry
	defaultDailyByteBudget int64
	perHostDailyByteBudget map[string]int64

	sessions *SensitiveTracker
	budget   Budget
	ledger   Ledger
	notifier Notifier
	log      *logx.Logger

	now func() time.Time

	// mu serializes Check: a budget check-then-add is a single logical
	// decision that must never race a concurrent Check for the same host
	// (kahyad already caps brain.db to one connection, but Gate's own
	// read-then-write against Budget must still be atomic at the Go
	// level, independent of that).
	mu sync.Mutex
}

// NewGate constructs a Gate from cfg (policy.yaml's egress: section,
// W3-01), sessions (the shared SensitiveTracker fs.go's sensitive-read
// seam also marks), budget (persistence, budget.go), ledger, notifier,
// and log (any may be nil except sessions/budget, which a real caller
// always provides — see gate_test.go for the exact nil-safety each
// dependency gets).
func NewGate(cfg policy.EgressConfig, sessions *SensitiveTracker, budget Budget, ledger Ledger, notifier Notifier, log *logx.Logger) *Gate {
	allow := make(map[string]allowEntry, len(cfg.Allowlist))
	for _, e := range cfg.Allowlist {
		host, err := CanonicalizeHost(e.Host)
		if err != nil {
			// policy.Load's own validateEgress already rejects an empty
			// host at boot — unreachable with a validated Policy, but
			// skipping (rather than panicking) keeps NewGate itself
			// total.
			continue
		}
		var ports map[int]bool
		if len(e.Ports) > 0 {
			ports = make(map[int]bool, len(e.Ports))
			for _, p := range e.Ports {
				ports[p] = true
			}
		}
		allow[host] = allowEntry{ports: ports}
	}
	perHost := make(map[string]int64, len(cfg.DailyByteBudget))
	for h, b := range cfg.DailyByteBudget {
		if canon, err := CanonicalizeHost(h); err == nil {
			perHost[canon] = b
		}
	}
	return &Gate{
		allowlist:              allow,
		defaultDailyByteBudget: cfg.DefaultDailyByteBudget,
		perHostDailyByteBudget: perHost,
		sessions:               sessions,
		budget:                 budget,
		ledger:                 ledger,
		notifier:               notifier,
		log:                    log,
		now:                    time.Now,
	}
}

// SetClock overrides Gate's clock (tests only — exercises budget-rollover
// at a controlled local-day boundary without a real wait).
func (g *Gate) SetClock(now func() time.Time) { g.now = now }

// matchAllowlist reports whether canonicalHost:port is present in the
// allowlist — an EXACT string match against canonicalHost (never a
// prefix/suffix/substring match), so a hostname, an IP literal, and a
// private/link-local IP literal are all subject to the identical rule:
// present verbatim (an operator explicitly added it), or denied.
func (g *Gate) matchAllowlist(canonicalHost string, port int) bool {
	entry, ok := g.allowlist[canonicalHost]
	if !ok {
		return false
	}
	if len(entry.ports) == 0 {
		return true
	}
	return entry.ports[port]
}

// Check is the W3-05 gate's one exported decision entrypoint (this
// package's doc comment). nbytes is the number of bytes THIS call is
// about to send (a plain HTTP request's body size, or 0 for a CONNECT
// tunnel's pre-dial admission check — see proxy.go, which separately
// meters the tunnel's actual bytes post-hoc via MeterUsage once it
// closes). An error return is a Gate-internal failure (e.g. the budget
// store is unreachable) — callers MUST fail closed on it exactly like
// every other policy check in this codebase (tasks/README.md: "any
// policy/permission/classification error or timeout results in DENY").
func (g *Gate) Check(ctx context.Context, target Target, nbytes int64, session SessionInfo) (Decision, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	canonical, err := CanonicalizeHost(target.Host)
	if err != nil {
		return g.deny(ctx, target.Host, EventBlockedAllowlist, fmt.Sprintf(reasonAllowlistFmt, target.Host), session), nil
	}

	sensitive := session.SessionID != "" && g.sessions != nil && g.sessions.IsMarked(session.SessionID)
	matched := g.matchAllowlist(canonical, target.Port)

	if sensitive && !matched {
		reason := fmt.Sprintf(reasonSensitiveFmt, canonical)
		return g.deny(ctx, canonical, EventBlockedSensitive, reason, session), nil
	}
	if !matched {
		reason := fmt.Sprintf(reasonAllowlistFmt, canonical)
		if IsIPLiteral(canonical) && isPrivateOrLinkLocal(canonical) {
			reason = fmt.Sprintf(reasonPrivateFmt, canonical)
		}
		return g.deny(ctx, canonical, EventBlockedAllowlist, reason, session), nil
	}

	day := g.now().Format("2006-01-02")
	limit := g.defaultDailyByteBudget
	if override, ok := g.perHostDailyByteBudget[canonical]; ok {
		limit = override
	}

	var current int64
	if g.budget != nil {
		current, err = g.budget.Bytes(ctx, canonical, day)
		if err != nil {
			return Decision{}, fmt.Errorf("egress: read budget for %s: %w", canonical, err)
		}
	}
	if limit > 0 && current+nbytes > limit {
		reason := fmt.Sprintf(reasonBudgetFmt, canonical)
		d := g.deny(ctx, canonical, EventBlockedBudget, reason, session)
		if g.notifier != nil {
			_ = g.notifier.Alarm(ctx, session.TraceID, EventBlockedBudget, reason, map[string]any{
				"host": canonical, "task_id": session.TaskID,
			})
		}
		return d, nil
	}

	if g.budget != nil && nbytes > 0 {
		if _, err := g.budget.Add(ctx, canonical, day, nbytes); err != nil {
			return Decision{}, fmt.Errorf("egress: add budget for %s: %w", canonical, err)
		}
	}

	g.record(ctx, session, EventAllowed, map[string]any{
		"host": canonical, "port": target.Port, "bytes": nbytes,
	})
	return Decision{Allow: true, Rule: EventAllowed}, nil
}

// MeterUsage records nbytes (both directions combined) against host's
// running daily budget AFTER a CONNECT tunnel has already closed
// (proxy.go) — bookkeeping, not a decision: the bytes already flowed, so
// this can never retroactively deny them; it only makes a LATER Check
// call for the same host see the updated total (this is what makes "push
// 2KB against a 1KB budget, second request blocked" hold for a sequence
// of tunneled connections, exactly as it does for a sequence of plain-HTTP
// Check calls). Logged as EventMetered, distinct from the four decision
// kinds, so a JSONL/ledger reader can always tell a real admission
// decision apart from this after-the-fact accounting entry.
func (g *Gate) MeterUsage(ctx context.Context, host string, nbytes int64, session SessionInfo) {
	if nbytes <= 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	canonical, err := CanonicalizeHost(host)
	if err != nil || g.budget == nil {
		return
	}
	day := g.now().Format("2006-01-02")
	if _, err := g.budget.Add(ctx, canonical, day, nbytes); err != nil {
		if g.log != nil {
			g.log.With(session.TraceID).Warn("egress_meter_failed", "host", canonical, "err", err.Error())
		}
		return
	}
	g.record(ctx, session, EventMetered, map[string]any{"host": canonical, "bytes": nbytes})
}

// MarkSensitiveRead flags sessionID sensitive (SensitiveTracker.Mark,
// rises-only) and ledgers/logs EventSensitiveMarked — this is the Go-level
// counterpart of POST /session/sensitive-read: kahyad/internal/server/
// egress.go mounts that HTTP wire endpoint AND wires mcp/fs's fs_read
// seam to call this method directly, in-process (mcp/fs.
// SensitiveReadMarker's own doc comment: the same "in-process today, a
// real HTTP client later" seam PolicyClient already established). An
// empty sessionID is an error — there is nothing to mark.
func (g *Gate) MarkSensitiveRead(ctx context.Context, sessionID, traceID string) error {
	if sessionID == "" {
		return fmt.Errorf("egress: MarkSensitiveRead: empty session_id")
	}
	if g.sessions != nil {
		g.sessions.Mark(sessionID)
	}
	g.record(ctx, SessionInfo{SessionID: sessionID, TraceID: traceID}, EventSensitiveMarked, nil)
	return nil
}

func (g *Gate) deny(ctx context.Context, host, event, reason string, session SessionInfo) Decision {
	g.record(ctx, session, event, map[string]any{"host": host, "reason": reason})
	return Decision{Allow: false, Reason: reason, Rule: event}
}

// record writes BOTH the append-only DB ledger row and a JSONL log line
// for kind (this task's own acceptance criterion: "every gate decision
// produces a JSONL log line and an events row carrying trace_id") —
// mirrors mcp/fs.Server.logAndLedger's identical dual-write convention.
func (g *Gate) record(ctx context.Context, session SessionInfo, kind string, payload map[string]any) {
	if payload == nil {
		payload = map[string]any{}
	}
	payload["event"] = kind
	payload["task_id"] = session.TaskID
	payload["session_id"] = session.SessionID

	if g.ledger != nil {
		if err := g.ledger.LogEvent(ctx, session.TraceID, kind, payload); err != nil {
			if g.log != nil {
				g.log.With(session.TraceID).Warn(kind+"_ledger_error", "err", err.Error())
			}
		}
	}
	if g.log != nil {
		g.log.With(session.TraceID).Info(kind, mapToArgs(payload)...)
	}
}

// mapToArgs flattens payload into the alternating key/value... variadic
// shape logx.Logger.Info/Warn expects (mirrors mcp/fs's/mcp/shell's
// identical helper, duplicated here per this codebase's own
// per-package-copy convention for small helpers not worth sharing across
// a package boundary).
func mapToArgs(payload map[string]any) []any {
	args := make([]any, 0, len(payload)*2)
	for k, v := range payload {
		args = append(args, k, v)
	}
	return args
}
