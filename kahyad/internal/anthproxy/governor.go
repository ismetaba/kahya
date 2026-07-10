// governor.go implements W12-08 step 3: the shared, in-process cost
// governor — one instance per daemon, consulted by every per-task
// anthproxy.Proxy before forwarding a request (CheckBeforeForward) and
// updated once that request's usage/cost is known (RecordUsage). All state
// is rebuilt from the events ledger at boot (Boot) and then held purely in
// memory for the rest of the process's life — this package never imports
// kahyad/internal/store directly (main.go converts sqlcgen rows into
// BootEvent), which is what keeps it store-agnostic and lets every test in
// this package run against a plain fixture slice instead of a real
// brain.db.
package anthproxy

import (
	"context"
	"fmt"
	"sync"
	"time"

	"kahya/kahyad/internal/notify"
)

// Limits are the governor's configured thresholds (config keys committed
// in kahyad/internal/config, HANDOFF §4 cost-governor flag defaults).
type Limits struct {
	DailyBudgetUSD         float64
	MonthlyBudgetUSD       float64
	TaskTokenCeiling       int64
	DowngradeAtRatio       float64
	CacheHitAlarmThreshold float64
}

// Turkish user-facing block messages (byte-exact from the task file — do
// not paraphrase, do not ASCII-fold the diacritics).
const (
	MsgTaskCeiling         = "Görev token tavanına ulaştı (500K) — duraklatıldı."
	MsgDailyBudgetBlock    = "Günlük bütçe doldu ($10)."
	MsgMonthlyBudgetBlock  = "Aylık bütçe doldu ($150)."
	MsgKeychainUnavailable = "Keychain erişilemiyor — bulut şeridi kapalı."
)

// Ledger event kinds this package (and proxy.go) write — HANDOFF §5 safety
// #4: every governor/proxy decision is append-only auditable.
const (
	EventModelCall              = "model_call"
	EventProxyAuthReject        = "proxy_auth_reject"
	EventTaskPausedBudget       = "task_paused_budget"
	EventBudgetDowngradeOn      = "budget_downgrade_on"
	EventBudgetDowngradeUnavail = "budget_downgrade_unavailable"
	EventSpendAlarm80           = "spend_alarm_80"
	EventSpendAlarm100          = "spend_alarm_100"
	EventCacheHitAlarm          = "cache_hit_alarm"
	EventCacheBusterSuspect     = "cache_buster_suspect"
	EventKeychainUnavailable    = "keychain_unavailable"
	EventKeyOverrideIgnored     = "key_override_ignored"
)

// spendAlarmRatio80/100 are the fixed alarm crossings (task spec step 3:
// "Alarms: daily spend crossing 80%/100%") — distinct from
// Limits.DowngradeAtRatio, which happens to default to the same 0.8 value
// but is a separate, independently-configurable knob for the routing rung.
const (
	spendAlarmRatio80  = 0.8
	spendAlarmRatio100 = 1.0
	// minCallsForCacheHitAlarm is the "once >=20 calls that day" floor
	// below which the cache-hit-ratio alarm never fires — a handful of
	// early-day calls is not a meaningful sample.
	minCallsForCacheHitAlarm = 20
	// maxSystemHashChangesPerDay is the cache-buster-suspect threshold
	// (task spec step 4: "> 2 changes/day").
	maxSystemHashChangesPerDay = 2
)

const (
	dayLayout   = "2006-01-02"
	monthLayout = "2006-01"
)

// EventLedger is the narrow store-write dependency this package needs
// (kahyad/internal/store.Store.LogEvent already has exactly this method
// shape — no adapter required). Kept here (rather than importing store
// directly) so this package stays store-agnostic and hermetically
// testable.
type EventLedger interface {
	LogEvent(ctx context.Context, traceID, kind string, payload map[string]any) error
}

// dailyAgg is one UTC calendar day's running aggregate.
type dailyAgg struct {
	usd             float64
	calls           int64
	inputTokens     int64
	cacheReadTokens int64

	// *Logged flags dedupe the "once per day" ledger/alarm events; they
	// live only in memory and reset on daemon restart — see Boot's doc
	// comment for why that gap is accepted for W1-2.
	downgradeLogged        bool
	downgradeUnavailLogged bool
	alarm80Logged          bool
	alarm100Logged         bool
	cacheAlarmLogged       bool

	// lastSystemHash/systemHashChanges implement the cache-buster
	// detector (step 4): a change from the PREVIOUS call's hash (not
	// merely a new distinct value ever seen that day) increments the
	// counter.
	lastSystemHash    string
	systemHashChanges int
}

// Governor is kahyad's shared, in-process cost governor.
type Governor struct {
	mu     sync.Mutex
	limits Limits
	now    func() time.Time

	perTask map[string]int64     // task_id -> input+output+cache_creation sum
	daily   map[string]*dailyAgg // "2006-01-02" (UTC) -> aggregate
	monthly map[string]float64   // "2006-01" (UTC) -> usd sum

	notifier notify.Notifier
}

// NewGovernor constructs an empty Governor (no history replayed) — tests
// that don't need boot rebuild use this directly; production always goes
// through Boot instead so restart-safe totals are in place before the
// first request is ever checked. now defaults to time.Now when nil so
// tests can inject a fixed/controllable clock.
func NewGovernor(limits Limits, now func() time.Time, notifier notify.Notifier) *Governor {
	if now == nil {
		now = time.Now
	}
	return &Governor{
		limits:   limits,
		now:      now,
		perTask:  map[string]int64{},
		daily:    map[string]*dailyAgg{},
		monthly:  map[string]float64{},
		notifier: notifier,
	}
}

// ModelCallRecord is the model_call ledger event payload shape (step 2).
type ModelCallRecord struct {
	TaskID                   string  `json:"task_id"`
	Model                    string  `json:"model"`
	InputTokens              int64   `json:"input_tokens"`
	OutputTokens             int64   `json:"output_tokens"`
	CacheReadInputTokens     int64   `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64   `json:"cache_creation_input_tokens"`
	USD                      float64 `json:"usd"`
	Status                   string  `json:"status"`
	DurationMs               int64   `json:"duration_ms"`
}

// BootEvent pairs one historical model_call ledger row's timestamp with
// its already-JSON-decoded payload — the minimal shape Boot needs. main.go
// is what converts kahyad/internal/store/sqlcgen.Event rows into these
// (this package never imports sqlcgen/store directly).
type BootEvent struct {
	Ts     time.Time
	Record ModelCallRecord
}

// Boot rebuilds a Governor's in-memory totals from every historical
// model_call ledger event (step 3: "SELECT sums for today/this month/per
// task ... then maintained in memory"). events need not be pre-sorted —
// every field Boot accumulates is an order-independent sum.
//
// The per-day "already logged today" dedupe flags (downgrade/alarm
// once-per-day) are NOT reconstructed from ledger history — they start
// false after every Boot, same as NewGovernor. This is a deliberate,
// documented, restart-safe-enough-for-W1-2 simplification: kahyad runs
// under launchd KeepAlive and restarts are rare, and the worst case of
// this gap is a duplicate alarm/downgrade-on notification on the same day
// a restart happens to land on — never a missed budget/ceiling BLOCK,
// since those (CheckBeforeForward) are derived from the additive sums
// this function DOES faithfully replay.
func Boot(events []BootEvent, limits Limits, now func() time.Time, notifier notify.Notifier) *Governor {
	g := NewGovernor(limits, now, notifier)
	for _, e := range events {
		g.mu.Lock()
		g.recordLocked(e.Ts, e.Record)
		g.mu.Unlock()
	}
	return g
}

func (g *Governor) recordLocked(ts time.Time, r ModelCallRecord) {
	tokens := r.InputTokens + r.OutputTokens + r.CacheCreationInputTokens
	if r.TaskID != "" {
		g.perTask[r.TaskID] += tokens
	}
	day := ts.UTC().Format(dayLayout)
	month := ts.UTC().Format(monthLayout)
	agg := g.dayAggLocked(day)
	agg.usd += r.USD
	agg.calls++
	agg.inputTokens += r.InputTokens
	agg.cacheReadTokens += r.CacheReadInputTokens
	g.monthly[month] += r.USD
}

func (g *Governor) dayAggLocked(day string) *dailyAgg {
	a, ok := g.daily[day]
	if !ok {
		a = &dailyAgg{}
		g.daily[day] = a
	}
	return a
}

// CheckResult is CheckBeforeForward's decision.
type CheckResult struct {
	Allowed bool
	// Message is the Turkish block message, set only when !Allowed.
	Message string
}

// CheckBeforeForward implements step 3's fail-closed BLOCKING ordering:
// the request is checked against the per-task ceiling and the daily/
// monthly budgets using ONLY totals accumulated from calls that already
// completed. This call's own token/USD cost is not known until its
// response is parsed (see RecordUsage) — there is no way to predict it in
// advance — so "blocked before forwarding" means "blocked because PRIOR
// usage already reached a limit", never a prediction about this
// particular request.
func (g *Governor) CheckBeforeForward(taskID string) CheckResult {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.limits.TaskTokenCeiling > 0 && g.perTask[taskID] >= g.limits.TaskTokenCeiling {
		return CheckResult{Message: MsgTaskCeiling}
	}

	today := g.now().UTC().Format(dayLayout)
	if g.limits.DailyBudgetUSD > 0 {
		if agg := g.daily[today]; agg != nil && agg.usd >= g.limits.DailyBudgetUSD {
			return CheckResult{Message: MsgDailyBudgetBlock}
		}
	}

	month := g.now().UTC().Format(monthLayout)
	if g.limits.MonthlyBudgetUSD > 0 && g.monthly[month] >= g.limits.MonthlyBudgetUSD {
		return CheckResult{Message: MsgMonthlyBudgetBlock}
	}

	return CheckResult{Allowed: true}
}

// DowngradeModel implements the FIXED Opus->Sonnet->yerel chain (HANDOFF
// §4 ⚑, verbatim): Opus steps down to Sonnet. Sonnet has nowhere left to
// go until W3-08 lands the local lane, so it is returned unchanged
// (changed=false) — callers must NOT invent a Sonnet->Haiku rung (Haiku is
// a task-class model in the §4 routing table, never a rung in this
// chain). Any other model (Haiku, Fable, or an unrecognized string) is
// also returned unchanged — this chain only ever applies to the two
// models the §4 flag names.
func (g *Governor) DowngradeModel(model string) (result string, changed bool) {
	if model == "claude-opus-4-8" {
		return "claude-sonnet-5", true
	}
	return model, false
}

// Downgraded reports whether today's spend has crossed
// Limits.DowngradeAtRatio of the daily budget (default 0.8, i.e. $8 of
// $10) — W12-07's envelope builder consults this for NEW tasks only; it
// never affects an already-running task.
func (g *Governor) Downgraded() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.downgradedLocked()
}

func (g *Governor) downgradedLocked() bool {
	if g.limits.DailyBudgetUSD <= 0 {
		return false
	}
	today := g.now().UTC().Format(dayLayout)
	agg := g.daily[today]
	if agg == nil {
		return false
	}
	return agg.usd >= g.limits.DailyBudgetUSD*g.limits.DowngradeAtRatio
}

// RecordUsage applies one completed call's usage/cost to the governor's
// in-memory state (steps 2/3/4) AFTER the upstream response has been
// fully parsed — CheckBeforeForward already ran before this request was
// forwarded, using only prior totals; this is where the request's OWN
// contribution lands. It ledgers kind=model_call unconditionally, then
// runs the once-per-day downgrade/alarm/cache-buster bookkeeping against
// the freshly-updated totals, ledgering/alarming each side effect at most
// once per UTC day. systemHash is the sha256 hex of the request's
// system[0] block (empty string if the request had none).
//
// eventLedger/traceID are passed per-call (rather than stored on Governor)
// because a single shared Governor instance is used across every
// concurrently-running task's Proxy, each of which has its own trace_id;
// ctx is the caller's own background context (proxy.go uses
// context.Background(), matching kahyad/internal/server/task.go's
// persistCtx convention — a disconnected/timed-out client must never
// prevent kahyad from recording that a call happened).
func (g *Governor) RecordUsage(ctx context.Context, eventLedger EventLedger, traceID, taskID, model string, u Usage, usd float64, status string, durationMs int64, systemHash string) {
	g.mu.Lock()
	now := g.now()
	r := ModelCallRecord{
		TaskID: taskID, Model: model,
		InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
		CacheReadInputTokens: u.CacheReadInputTokens, CacheCreationInputTokens: u.CacheCreationInputTokens,
		USD: usd, Status: status, DurationMs: durationMs,
	}
	g.recordLocked(now, r)

	day := now.UTC().Format(dayLayout)
	agg := g.dayAggLocked(day)

	var busterSuspect bool
	if systemHash != "" {
		if agg.lastSystemHash != "" && agg.lastSystemHash != systemHash {
			agg.systemHashChanges++
			if agg.systemHashChanges > maxSystemHashChangesPerDay {
				busterSuspect = true
			}
		}
		agg.lastSystemHash = systemHash
	}

	fireDowngrade := g.limits.DailyBudgetUSD > 0 &&
		agg.usd >= g.limits.DailyBudgetUSD*g.limits.DowngradeAtRatio &&
		!agg.downgradeLogged
	if fireDowngrade {
		agg.downgradeLogged = true
		// The "->yerel" rung needs W3-08's local lane; until it lands,
		// every downgrade-on crossing is ALSO the moment Sonnet-class
		// tasks discover they have nowhere further to fall (task spec
		// step 3: "Sonnet-class tasks stay on Sonnet ... ledgers
		// budget_downgrade_unavailable once per day").
		agg.downgradeUnavailLogged = true
	}
	fireDowngradeUnavail := fireDowngrade

	alarm80 := g.limits.DailyBudgetUSD > 0 &&
		agg.usd >= g.limits.DailyBudgetUSD*spendAlarmRatio80 && !agg.alarm80Logged
	if alarm80 {
		agg.alarm80Logged = true
	}
	alarm100 := g.limits.DailyBudgetUSD > 0 &&
		agg.usd >= g.limits.DailyBudgetUSD*spendAlarmRatio100 && !agg.alarm100Logged
	if alarm100 {
		agg.alarm100Logged = true
	}

	var cacheAlarm bool
	if agg.calls >= minCallsForCacheHitAlarm && !agg.cacheAlarmLogged {
		denom := agg.inputTokens + agg.cacheReadTokens
		if denom <= 0 {
			denom = 1
		}
		ratio := float64(agg.cacheReadTokens) / float64(denom)
		if ratio < g.limits.CacheHitAlarmThreshold {
			cacheAlarm = true
			agg.cacheAlarmLogged = true
		}
	}
	dailyUSD := agg.usd
	dailyBudget := g.limits.DailyBudgetUSD
	g.mu.Unlock()

	// Every side effect below runs OUTSIDE the lock: eventLedger/notifier
	// may do real I/O (sqlite insert today, a future Telegram call in
	// W3-07) and must never hold up another goroutine's governor check.
	g.ledgerModelCall(ctx, eventLedger, traceID, r)
	if busterSuspect {
		g.ledgerEvent(ctx, eventLedger, traceID, EventCacheBusterSuspect, map[string]any{
			"task_id": taskID, "system_hash": systemHash,
		})
	}
	if fireDowngrade {
		g.ledgerEvent(ctx, eventLedger, traceID, EventBudgetDowngradeOn, map[string]any{"day": day})
	}
	if fireDowngradeUnavail {
		g.ledgerEvent(ctx, eventLedger, traceID, EventBudgetDowngradeUnavail, map[string]any{
			"day": day, "note": "Sonnet->yerel rung needs W3-08; Sonnet-class tasks stay on Sonnet",
		})
	}
	if alarm80 {
		g.alarmEvent(ctx, eventLedger, traceID, EventSpendAlarm80,
			fmt.Sprintf("Gunluk harcama %%80 esigini asti ($%.2f / $%.2f).", dailyUSD, dailyBudget))
	}
	if alarm100 {
		g.alarmEvent(ctx, eventLedger, traceID, EventSpendAlarm100,
			fmt.Sprintf("Gunluk butce doldu ($%.2f / $%.2f).", dailyUSD, dailyBudget))
	}
	if cacheAlarm {
		g.alarmEvent(ctx, eventLedger, traceID, EventCacheHitAlarm, "Gunluk cache-hit orani esigin altina dustu.")
	}
}

func (g *Governor) ledgerModelCall(ctx context.Context, eventLedger EventLedger, traceID string, r ModelCallRecord) {
	if eventLedger == nil {
		return
	}
	_ = eventLedger.LogEvent(ctx, traceID, EventModelCall, map[string]any{
		"task_id": r.TaskID, "model": r.Model,
		"input_tokens":                r.InputTokens,
		"output_tokens":               r.OutputTokens,
		"cache_read_input_tokens":     r.CacheReadInputTokens,
		"cache_creation_input_tokens": r.CacheCreationInputTokens,
		"usd":                         r.USD,
		"status":                      r.Status,
		"duration_ms":                 r.DurationMs,
	})
}

func (g *Governor) ledgerEvent(ctx context.Context, eventLedger EventLedger, traceID, kind string, payload map[string]any) {
	if eventLedger == nil {
		return
	}
	_ = eventLedger.LogEvent(ctx, traceID, kind, payload)
}

// alarmEvent routes through g.notifier when one is set (it ledgers on its
// own — see kahyad/internal/notify.JSONLNotifier — so this must not ALSO
// call ledgerEvent, which would double-write the row); falls back to a
// bare ledger row if no notifier was wired.
func (g *Governor) alarmEvent(ctx context.Context, eventLedger EventLedger, traceID, kind, message string) {
	if g.notifier != nil {
		_ = g.notifier.Alarm(ctx, traceID, kind, message, nil)
		return
	}
	g.ledgerEvent(ctx, eventLedger, traceID, kind, map[string]any{"message": message})
}
