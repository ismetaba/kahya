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
	"encoding/json"
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
	// EstRequestTokens is the fail-closed fallback per-request token
	// estimate (config key est_request_tokens, committed default 50000)
	// CheckBeforeForward's reservation step (BLOCKER 2 fix) uses when the
	// about-to-be-forwarded request's own max_tokens/body size cannot be
	// parsed - see estimateRequestLocked's doc comment for the full
	// estimation strategy. <=0 falls back to defaultEstRequestTokens, so a
	// zero-value Limits (e.g. a test literal that predates this field)
	// never silently disables the reservation.
	EstRequestTokens int64
}

// defaultEstRequestTokens is the built-in floor for Limits.EstRequestTokens
// (see its doc comment) - used whenever that field is <=0.
const defaultEstRequestTokens = 50_000

// Turkish user-facing block messages (byte-exact from the task file — do
// not paraphrase, do not ASCII-fold the diacritics).
const (
	MsgTaskCeiling        = "Görev token tavanına ulaştı (500K) — duraklatıldı."
	MsgDailyBudgetBlock   = "Günlük bütçe doldu ($10)."
	MsgMonthlyBudgetBlock = "Aylık bütçe doldu ($150)."
	// MsgKeychainUnavailable is byte-exact with the fixed error JSON in the
	// task spec (tasks/w1-2-core/W12-08-anthropic-forward-proxy.md:
	// `{"type":"error","message":"Keychain erişilemiyor — bulut şeridi
	// kapalı"}`) - NO trailing period (MINOR 3 fix; the previous value here
	// had one, which was never byte-exact with the spec literal).
	MsgKeychainUnavailable = "Keychain erişilemiyor — bulut şeridi kapalı"
)

// Ledger event kinds this package (and proxy.go) write — HANDOFF §5 safety
// #4: every governor/proxy decision is append-only auditable.
//
// EventBudgetDowngradeUnavail ("budget_downgrade_unavailable") is RETIRED
// (W4-08): it used to fire alongside EventBudgetDowngradeOn because the
// Sonnet->yerel downgrade rung had nowhere to route a Sonnet-class task
// before kahyad/internal/router's local lane existed. That lane now exists
// (kahyad/internal/server's POST /v1/task envelope builder consults
// router.SelectModel + this governor's Downgraded() and routes a
// Sonnet-class task locally instead) — there is no longer any "downgrade
// unavailable" case to ledger, so this package no longer emits that event
// kind at all. The string constant is intentionally not kept around: no
// production code path can ever produce this event again.
const (
	EventModelCall           = "model_call"
	EventProxyAuthReject     = "proxy_auth_reject"
	EventTaskPausedBudget    = "task_paused_budget"
	EventBudgetDowngradeOn   = "budget_downgrade_on"
	EventSpendAlarm80        = "spend_alarm_80"
	EventSpendAlarm100       = "spend_alarm_100"
	EventCacheHitAlarm       = "cache_hit_alarm"
	EventCacheBusterSuspect  = "cache_buster_suspect"
	EventKeychainUnavailable = "keychain_unavailable"
	EventKeyOverrideIgnored  = "key_override_ignored"
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
	downgradeLogged  bool
	alarm80Logged    bool
	alarm100Logged   bool
	cacheAlarmLogged bool

	// lastSystemHash/systemHashChanges implement the cache-buster
	// detector (step 4): a change from the PREVIOUS call's hash (not
	// merely a new distinct value ever seen that day) increments the
	// counter.
	lastSystemHash    string
	systemHashChanges int
}

// ReservationID identifies one CheckBeforeForward reservation (BLOCKER 2
// fix). The zero value means "no reservation" (CheckBeforeForward returns
// it when !Allowed, and ReleaseReservation/RecordUsage treat it as a
// no-op) - valid ids start at 1 (see reserveLocked).
type ReservationID uint64

// reservation is one in-flight request's conservative estimate, held only
// long enough for RecordUsage (or, on any failure path that never reaches
// it, the proxy's own deferred ReleaseReservation call) to release it.
// day/month are captured at reservation time (not re-derived from a later
// now() call) so releaseLocked always subtracts from the SAME daily/
// monthly bucket the estimate was added to, even if a request happens to
// straddle a UTC day/month boundary between CheckBeforeForward and its
// eventual release.
type reservation struct {
	taskID     string
	tokens     int64
	usd        float64
	day, month string
}

// Governor is kahyad's shared, in-process cost governor.
type Governor struct {
	mu     sync.Mutex
	limits Limits
	now    func() time.Time

	perTask map[string]int64     // task_id -> completed input+output+cache_creation sum
	daily   map[string]*dailyAgg // "2006-01-02" (UTC) -> aggregate
	monthly map[string]float64   // "2006-01" (UTC) -> completed usd sum

	// Reservation state (BLOCKER 2 fix): CheckBeforeForward's
	// check-then-act was a TOCTOU - concurrent requests could each observe
	// "under limit" and only debit completed totals AFTER forwarding,
	// jointly blowing past a hard cap by an unbounded multiple. These maps
	// hold the in-flight, not-yet-completed estimate for every request
	// currently between CheckBeforeForward and RecordUsage/
	// ReleaseReservation, all mutated only under mu, so the very next
	// concurrent CheckBeforeForward call sees them immediately. They are
	// intentionally NOT rebuilt by Boot (unlike perTask/daily/monthly) -
	// a reservation only ever exists for the lifetime of one in-flight
	// HTTP request, so there is nothing to reconcile across a restart.
	nextReservationID  uint64
	reservations       map[ReservationID]reservation
	perTaskReservedTok map[string]int64   // task_id -> reserved token sum
	dailyReservedUSD   map[string]float64 // "2006-01-02" (UTC) -> reserved usd sum
	monthlyReservedUSD map[string]float64 // "2006-01" (UTC) -> reserved usd sum

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
		limits:             limits,
		now:                now,
		perTask:            map[string]int64{},
		daily:              map[string]*dailyAgg{},
		monthly:            map[string]float64{},
		reservations:       map[ReservationID]reservation{},
		perTaskReservedTok: map[string]int64{},
		dailyReservedUSD:   map[string]float64{},
		monthlyReservedUSD: map[string]float64{},
		notifier:           notifier,
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
	// Reservation is the handle CheckBeforeForward granted when
	// Allowed - the caller (proxy.go) must pass it to RecordUsage once the
	// call completes, AND (unconditionally, via defer, covering every
	// failure path in between - a reserved-but-never-recorded request must
	// never leak a permanent reservation) to ReleaseReservation. Both are
	// safe to call on the same id - releaseLocked is idempotent, a second
	// release of an already-released id is a no-op. Zero
	// (ReservationID's zero value) when !Allowed: there is nothing to
	// release.
	Reservation ReservationID
}

// estRequestBytesPerToken is a deliberately LOW (hence token-count-
// inflating) bytes-per-token ratio used to translate a request body's raw
// byte length into an input-token estimate: a real English/Turkish token
// averages closer to ~4 bytes, so dividing by 3 instead overestimates
// rather than under - see estimateRequestLocked's doc comment for why
// CheckBeforeForward's whole estimation posture is "over, never under".
const estRequestBytesPerToken = 3

// CheckBeforeForward implements step 3's fail-closed BLOCKING ordering,
// converted (BLOCKER 2 fix) from a plain check-then-act into an atomic
// check-and-RESERVE: the request is checked against the per-task ceiling
// and the daily/monthly budgets using completed totals from calls that
// already finished PLUS every other request's still-outstanding
// reservation PLUS a conservative (over-, never under-) estimate of this
// request's own eventual cost - all computed and compared under g.mu, so
// two requests racing this method never both observe "under limit" for
// the same slice of headroom. If allowed, that estimate is itself added to
// the reservation pools before this method returns, closing the exact
// window a burst of concurrent requests could otherwise use to jointly
// blow past a hard cap: the very next concurrent call sees this one's
// reservation immediately, not just its eventual completed total.
//
// body/model are best-effort inputs to estimateRequestLocked - a malformed
// or absent body/model degrades to a fixed configured estimate, never to
// "no estimate" (see that function's doc comment); this call must never
// skip reserving just because it could not parse the request precisely.
//
// The returned Reservation MUST eventually be released exactly once via
// RecordUsage or ReleaseReservation (both idempotent, so calling both is
// safe) - see CheckResult.Reservation's doc comment.
func (g *Governor) CheckBeforeForward(taskID, model string, body []byte) CheckResult {
	g.mu.Lock()
	defer g.mu.Unlock()

	estTokens, estUSD := g.estimateRequestLocked(model, body)

	if g.limits.TaskTokenCeiling > 0 {
		projected := g.perTask[taskID] + g.perTaskReservedTok[taskID] + estTokens
		if projected > g.limits.TaskTokenCeiling {
			return CheckResult{Message: MsgTaskCeiling}
		}
	}

	today := g.now().UTC().Format(dayLayout)
	if g.limits.DailyBudgetUSD > 0 {
		var completedUSD float64
		if agg := g.daily[today]; agg != nil {
			completedUSD = agg.usd
		}
		if completedUSD+g.dailyReservedUSD[today]+estUSD > g.limits.DailyBudgetUSD {
			return CheckResult{Message: MsgDailyBudgetBlock}
		}
	}

	month := g.now().UTC().Format(monthLayout)
	if g.limits.MonthlyBudgetUSD > 0 {
		if g.monthly[month]+g.monthlyReservedUSD[month]+estUSD > g.limits.MonthlyBudgetUSD {
			return CheckResult{Message: MsgMonthlyBudgetBlock}
		}
	}

	id := g.reserveLocked(taskID, estTokens, estUSD, today, month)
	return CheckResult{Allowed: true, Reservation: id}
}

// estimateRequestLocked computes CheckBeforeForward's CONSERVATIVE
// upper-bound token/USD estimate for one about-to-be-forwarded request -
// never an exact prediction (the real number is only known after
// RecordUsage parses the response), always an over-estimate, so that
// reserving it against the ceiling/budgets can only make
// CheckBeforeForward MORE likely to block, never less - the fail-closed
// posture HANDOFF §5 requires of a hard cap.
//
// Estimation strategy, in priority order:
//  1. If body parses as JSON, its max_tokens field (when positive) is
//     taken as the output-token bound VERBATIM - Anthropic hard-caps the
//     response at exactly this many tokens, so it is already a true upper
//     bound, not a guess. The input-side contribution is estimated from
//     the body's own byte length (estRequestBytesPerToken) - this
//     necessarily also covers any system/tools/cache_control blocks the
//     body contains, and since there is no pre-flight signal for whether
//     a given call will actually create a cache write, EVERY input-side
//     token here is priced at the pricier 1h cache-WRITE rate rather than
//     the plain input rate ("include cache" in the task spec) so the USD
//     estimate stays an over-estimate even for a cache-writing call.
//  2. If body is empty/unparseable, or JSON but with no positive
//     max_tokens, this falls back to Limits.EstRequestTokens (config key
//     est_request_tokens, <=0 uses defaultEstRequestTokens) for the WHOLE
//     token count, priced entirely at the pricier OUTPUT rate - the most
//     conservative single number available when nothing about the actual
//     request shape is known.
//
// A model this function cannot price (empty/unrecognized/no pricing row
// for "now") still returns a non-zero TOKEN estimate (so the per-task
// ceiling reservation is never skipped just because pricing is unknown),
// but a zero USD estimate (there is no rate to apply) - a request whose
// model cannot be priced is deliberately policed by the token-ceiling
// check only, not the USD budget checks.
func (g *Governor) estimateRequestLocked(model string, body []byte) (tokens int64, usd float64) {
	row, priceErr := PriceFor(model, g.now())
	fallbackTokens := g.limits.EstRequestTokens
	if fallbackTokens <= 0 {
		fallbackTokens = defaultEstRequestTokens
	}

	if probe, ok := parseEstimateProbe(body); ok {
		outTokens := probe.MaxTokens
		if outTokens <= 0 {
			outTokens = fallbackTokens
		}
		inTokens := int64(len(body)) / estRequestBytesPerToken
		if inTokens <= 0 {
			inTokens = 1
		}
		tokens = inTokens + outTokens
		if priceErr == nil {
			usd = float64(inTokens)*row.USDPerMTokCacheWrite1h/1_000_000 +
				float64(outTokens)*row.USDPerMTokOut/1_000_000
		}
		return tokens, usd
	}

	tokens = fallbackTokens
	if priceErr == nil {
		usd = float64(tokens) * row.USDPerMTokOut / 1_000_000
	}
	return tokens, usd
}

// estimateProbe is the subset of a /v1/messages request body
// estimateRequestLocked reads to size its estimate - kept minimal and
// separate from probeRequest's (model/system) shape since the two are read
// for unrelated purposes at unrelated call sites.
type estimateProbe struct {
	MaxTokens int64 `json:"max_tokens"`
}

// parseEstimateProbe reports ok=false for an empty or non-JSON body -
// exactly the signal estimateRequestLocked uses to fall back to the
// configured default instead of a body-derived estimate.
func parseEstimateProbe(body []byte) (estimateProbe, bool) {
	if len(body) == 0 {
		return estimateProbe{}, false
	}
	var p estimateProbe
	if err := json.Unmarshal(body, &p); err != nil {
		return estimateProbe{}, false
	}
	return p, true
}

// reserveLocked grants a new reservation, folding tokens/usd into every
// reservation pool CheckBeforeForward itself consults (per-task tokens,
// daily USD, monthly USD) so the very next call sees it. Must be called
// with g.mu held.
func (g *Governor) reserveLocked(taskID string, tokens int64, usd float64, day, month string) ReservationID {
	g.nextReservationID++
	id := ReservationID(g.nextReservationID)
	g.reservations[id] = reservation{taskID: taskID, tokens: tokens, usd: usd, day: day, month: month}
	g.perTaskReservedTok[taskID] += tokens
	g.dailyReservedUSD[day] += usd
	g.monthlyReservedUSD[month] += usd
	return id
}

// releaseLocked removes reservation id's contribution from every pool it
// was added to in reserveLocked. Idempotent: an unknown id (zero value, or
// one already released by a prior call - RecordUsage and the proxy's
// deferred ReleaseReservation may both race to release the same id) is a
// silent no-op, never a panic/error - a release is inherently "at most
// once matters, more than once is harmless". Must be called with g.mu
// held.
func (g *Governor) releaseLocked(id ReservationID) {
	if id == 0 {
		return
	}
	r, ok := g.reservations[id]
	if !ok {
		return
	}
	delete(g.reservations, id)
	g.perTaskReservedTok[r.taskID] -= r.tokens
	g.dailyReservedUSD[r.day] -= r.usd
	g.monthlyReservedUSD[r.month] -= r.usd
}

// ReleaseReservation releases a reservation CheckBeforeForward granted
// WITHOUT recording any completed usage (BLOCKER 2 fix) - proxy.go defers
// this unconditionally right after a successful CheckBeforeForward, so any
// path that ends the request without ever reaching RecordUsage (egress-
// gate/keychain failure after the reservation was already granted, or the
// upstream RoundTrip itself erroring before the reverse proxy's
// ModifyResponse hook ever runs) still releases the reservation instead of
// leaking it forever - a leaked reservation would otherwise permanently
// count against future requests' ceiling/budget checks. Safe to call after
// RecordUsage already released the same id (see releaseLocked).
func (g *Governor) ReleaseReservation(id ReservationID) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.releaseLocked(id)
}

// DowngradeModel implements the Opus->Sonnet leg of HANDOFF §4 ⚑'s FIXED
// Opus->Sonnet->yerel chain in isolation: Opus steps down to Sonnet. Sonnet
// is returned unchanged (changed=false) — callers must NOT invent a
// Sonnet->Haiku rung (Haiku is a task-class model in the §4 routing table,
// never a rung in this chain). Any other model (Haiku, Fable, or an
// unrecognized string) is also returned unchanged — this chain only ever
// applies to the two models the §4 flag names.
//
// NOTE (W4-08): the Sonnet->yerel leg is NOT implemented here — it is
// implemented generically, on whatever base model a task's intent resolved
// to, by kahyad/internal/router.SelectModel (which consults this
// Governor's Downgraded() below, not this method) — see that package's own
// doc comment for why the local lane is represented as a routing BRANCH
// decision, never a model string. This method is kept exactly as W12-08
// left it (a pure Opus->Sonnet mapping) for any caller that only needs
// that one leg in isolation; production routing does not call it.
func (g *Governor) DowngradeModel(model string) (result string, changed bool) {
	if model == "claude-opus-4-8" {
		return "claude-sonnet-5", true
	}
	return model, false
}

// Downgraded reports whether today's spend has crossed
// Limits.DowngradeAtRatio of the daily budget (default 0.8, i.e. $8 of
// $10) — kahyad/internal/server's POST /v1/task envelope builder consults
// this for NEW tasks only (via kahyad/internal/router.SelectModel's
// RouteInput.Downgraded field); it never affects an already-running task.
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
// forwarded, using only prior totals (plus, since BLOCKER 2, every other
// still-outstanding reservation and this request's own estimate). This is
// where the request's OWN ACTUAL contribution lands: reservation
// (BLOCKER 2 fix) is CheckBeforeForward's returned handle for this exact
// request - RecordUsage releases it (subtracting the estimate back out of
// the reserved pools) BEFORE folding the real usage into the completed
// totals, under the same lock, so no concurrent CheckBeforeForward can
// ever observe a moment where both the estimate AND the actual are
// simultaneously counted. reservation may be the zero value (no
// reservation to release - e.g. a test seeding history directly) which
// releaseLocked treats as a no-op. It ledgers kind=model_call
// unconditionally, then runs the once-per-day downgrade/alarm/cache-buster
// bookkeeping against the freshly-updated totals, ledgering/alarming each
// side effect at most once per UTC day. systemHash is the sha256 hex of
// the request's system[0] block (empty string if the request had none).
//
// eventLedger/traceID are passed per-call (rather than stored on Governor)
// because a single shared Governor instance is used across every
// concurrently-running task's Proxy, each of which has its own trace_id;
// ctx is the caller's own background context (proxy.go uses
// context.Background(), matching kahyad/internal/server/task.go's
// persistCtx convention — a disconnected/timed-out client must never
// prevent kahyad from recording that a call happened).
func (g *Governor) RecordUsage(ctx context.Context, reservation ReservationID, eventLedger EventLedger, traceID, taskID, model string, u Usage, usd float64, status string, durationMs int64, systemHash string) {
	g.mu.Lock()
	g.releaseLocked(reservation)
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
	}

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
