package anthproxy

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeLedger records every LogEvent call for assertion.
type fakeLedger struct {
	mu    sync.Mutex
	calls []fakeLedgerCall
}

type fakeLedgerCall struct {
	traceID string
	kind    string
	payload map[string]any
}

func (f *fakeLedger) LogEvent(_ context.Context, traceID, kind string, payload map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeLedgerCall{traceID: traceID, kind: kind, payload: payload})
	return nil
}

func (f *fakeLedger) countKind(kind string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.kind == kind {
			n++
		}
	}
	return n
}

// fakeNotifier records every Notify/Alarm call.
type fakeNotifier struct {
	mu       sync.Mutex
	notified []string
	alarmed  []string
}

func (f *fakeNotifier) Notify(_ context.Context, _, kind, _ string, _ map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notified = append(f.notified, kind)
	return nil
}

func (f *fakeNotifier) Alarm(_ context.Context, _, kind, _ string, _ map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alarmed = append(f.alarmed, kind)
	return nil
}

func (f *fakeNotifier) countAlarm(kind string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, k := range f.alarmed {
		if k == kind {
			n++
		}
	}
	return n
}

func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func testLimits() Limits {
	return Limits{
		DailyBudgetUSD:         10,
		MonthlyBudgetUSD:       150,
		TaskTokenCeiling:       500_000,
		DowngradeAtRatio:       0.8,
		CacheHitAlarmThreshold: 0.5,
		EstRequestTokens:       50_000,
	}
}

// noReservation is RecordUsage's reservation param for calls in this file
// that seed history directly (never having gone through
// CheckBeforeForward first) - releaseLocked treats the zero value as a
// no-op.
const noReservation = ReservationID(0)

// TestCheckBeforeForwardBlocksAtTaskCeiling is the fail-closed-ordering
// acceptance test: a task whose PRIOR usage already reached the 500K
// ceiling is blocked before its next request is ever forwarded.
func TestCheckBeforeForwardBlocksAtTaskCeiling(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	g := NewGovernor(testLimits(), fixedClock(now), nil)

	ledger := &fakeLedger{}
	g.RecordUsage(context.Background(), noReservation, ledger, "trace1", "t_ceiling", "claude-sonnet-5",
		Usage{InputTokens: 500_000}, 1.0, "ok", 100, "")

	res := g.CheckBeforeForward("t_ceiling", "claude-sonnet-5", nil)
	if res.Allowed {
		t.Fatal("CheckBeforeForward() Allowed = true, want false once the task is at/over its 500K ceiling")
	}
	if res.Message != MsgTaskCeiling {
		t.Errorf("Message = %q, want %q", res.Message, MsgTaskCeiling)
	}

	// A DIFFERENT task must be unaffected.
	if res2 := g.CheckBeforeForward("t_other", "claude-sonnet-5", nil); !res2.Allowed {
		t.Errorf("a different task_id was blocked by another task's ceiling: %+v", res2)
	}
}

// TestCheckBeforeForwardBlocksAtDailyBudget matches the acceptance
// criterion: with daily_budget_usd overridden to $0.01 and one priced
// fixture event inserted, the next proxied request is blocked with the
// Turkish budget message and gains a task_paused_budget ledger row (that
// ledgering itself is proxy.go's job - this test proves the governor side:
// CheckBeforeForward returns !Allowed with the exact message).
func TestCheckBeforeForwardBlocksAtDailyBudget(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	limits := testLimits()
	limits.DailyBudgetUSD = 0.01
	g := NewGovernor(limits, fixedClock(now), nil)

	ledger := &fakeLedger{}
	g.RecordUsage(context.Background(), noReservation, ledger, "trace1", "t_budget", "claude-sonnet-5",
		Usage{InputTokens: 1000, OutputTokens: 1000}, 0.02, "ok", 50, "")

	res := g.CheckBeforeForward("t_budget", "claude-sonnet-5", nil)
	if res.Allowed {
		t.Fatal("CheckBeforeForward() Allowed = true, want false once daily spend >= daily_budget_usd")
	}
	if res.Message != MsgDailyBudgetBlock {
		t.Errorf("Message = %q, want %q", res.Message, MsgDailyBudgetBlock)
	}
}

func TestCheckBeforeForwardBlocksAtMonthlyBudget(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	limits := testLimits()
	limits.DailyBudgetUSD = 1_000_000 // never trip daily first
	limits.MonthlyBudgetUSD = 5
	g := NewGovernor(limits, fixedClock(now), nil)

	g.RecordUsage(context.Background(), noReservation, nil, "trace1", "t_monthly", "claude-sonnet-5",
		Usage{InputTokens: 1}, 6.0, "ok", 10, "")

	res := g.CheckBeforeForward("t_monthly", "claude-sonnet-5", nil)
	if res.Allowed || res.Message != MsgMonthlyBudgetBlock {
		t.Errorf("CheckBeforeForward() = %+v, want blocked with %q", res, MsgMonthlyBudgetBlock)
	}
}

func TestCheckBeforeForwardAllowsUnderLimits(t *testing.T) {
	g := NewGovernor(testLimits(), fixedClock(time.Now()), nil)
	res := g.CheckBeforeForward("t_fresh", "claude-sonnet-5", nil)
	if !res.Allowed {
		t.Errorf("CheckBeforeForward() on a fresh task = %+v, want Allowed=true", res)
	}
}

// TestDowngradeFlipsAt80Percent proves the 80% downgrade rung (Opus ->
// Sonnet; Sonnet stays put + budget_downgrade_unavailable, per HANDOFF §4
// until W3-08 lands the local lane).
func TestDowngradeFlipsAt80Percent(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	g := NewGovernor(testLimits(), fixedClock(now), nil)

	if g.Downgraded() {
		t.Fatal("Downgraded() = true before any spend, want false")
	}

	ledger := &fakeLedger{}
	// $8 of $10 daily budget = exactly the 0.8 ratio.
	g.RecordUsage(context.Background(), noReservation, ledger, "trace1", "t1", "claude-opus-4-8",
		Usage{InputTokens: 1}, 8.0, "ok", 10, "")

	if !g.Downgraded() {
		t.Fatal("Downgraded() = false after crossing 80% of daily budget, want true")
	}
	if got := ledger.countKind(EventBudgetDowngradeOn); got != 1 {
		t.Errorf("EventBudgetDowngradeOn ledgered %d times, want exactly 1", got)
	}
	if got := ledger.countKind(EventBudgetDowngradeUnavail); got != 1 {
		t.Errorf("EventBudgetDowngradeUnavail ledgered %d times, want exactly 1 (Sonnet has nowhere to fall until W3-08)", got)
	}

	// A second call the same day must NOT re-ledger either "once per day"
	// event.
	g.RecordUsage(context.Background(), noReservation, ledger, "trace1", "t1", "claude-opus-4-8",
		Usage{InputTokens: 1}, 0.5, "ok", 10, "")
	if got := ledger.countKind(EventBudgetDowngradeOn); got != 1 {
		t.Errorf("EventBudgetDowngradeOn ledgered %d times after a second call, want still 1 (once per day)", got)
	}

	// DowngradeModel: Opus -> Sonnet; Sonnet unchanged.
	if model, changed := g.DowngradeModel("claude-opus-4-8"); model != "claude-sonnet-5" || !changed {
		t.Errorf("DowngradeModel(opus) = (%q, %v), want (claude-sonnet-5, true)", model, changed)
	}
	if model, changed := g.DowngradeModel("claude-sonnet-5"); model != "claude-sonnet-5" || changed {
		t.Errorf("DowngradeModel(sonnet) = (%q, %v), want (claude-sonnet-5, false) - no Sonnet->Haiku rung", model, changed)
	}
	if model, changed := g.DowngradeModel("claude-haiku-4-5"); model != "claude-haiku-4-5" || changed {
		t.Errorf("DowngradeModel(haiku) = (%q, %v), want unchanged - Haiku is a task-class model, never a rung", model, changed)
	}
}

// TestSpendAlarmsFireOnceAt80And100 proves the alarm bullet independent of
// the downgrade rung.
func TestSpendAlarmsFireOnceAt80And100(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	g := NewGovernor(testLimits(), fixedClock(now), nil)
	notifier := &fakeNotifier{}
	g.notifier = notifier

	g.RecordUsage(context.Background(), noReservation, nil, "trace1", "t1", "claude-sonnet-5", Usage{InputTokens: 1}, 8.5, "ok", 10, "")
	if got := notifier.countAlarm(EventSpendAlarm80); got != 1 {
		t.Errorf("EventSpendAlarm80 fired %d times, want 1", got)
	}
	if got := notifier.countAlarm(EventSpendAlarm100); got != 0 {
		t.Errorf("EventSpendAlarm100 fired %d times before reaching 100%%, want 0", got)
	}

	g.RecordUsage(context.Background(), noReservation, nil, "trace1", "t1", "claude-sonnet-5", Usage{InputTokens: 1}, 2.0, "ok", 10, "")
	if got := notifier.countAlarm(EventSpendAlarm100); got != 1 {
		t.Errorf("EventSpendAlarm100 fired %d times after crossing 100%%, want 1", got)
	}
	// 80% alarm must not re-fire.
	if got := notifier.countAlarm(EventSpendAlarm80); got != 1 {
		t.Errorf("EventSpendAlarm80 fired %d times total, want still 1 (once per day)", got)
	}
}

// TestCacheHitAlarmFiresBelowThresholdWith20Calls proves the "daily
// cache-hit ratio < threshold once >=20 calls" alarm.
func TestCacheHitAlarmFiresBelowThresholdWith20Calls(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	g := NewGovernor(testLimits(), fixedClock(now), nil)
	notifier := &fakeNotifier{}
	g.notifier = notifier

	// 19 calls with a poor cache-hit ratio: must not fire yet (floor is
	// >=20 calls).
	for i := 0; i < 19; i++ {
		g.RecordUsage(context.Background(), noReservation, nil, "trace1", "t1", "claude-sonnet-5",
			Usage{InputTokens: 100, CacheReadInputTokens: 0}, 0.01, "ok", 5, "")
	}
	if got := notifier.countAlarm(EventCacheHitAlarm); got != 0 {
		t.Fatalf("EventCacheHitAlarm fired after only 19 calls, want 0")
	}

	// The 20th call crosses the floor; ratio is still 0 < 0.5 threshold.
	g.RecordUsage(context.Background(), noReservation, nil, "trace1", "t1", "claude-sonnet-5",
		Usage{InputTokens: 100, CacheReadInputTokens: 0}, 0.01, "ok", 5, "")
	if got := notifier.countAlarm(EventCacheHitAlarm); got != 1 {
		t.Errorf("EventCacheHitAlarm fired %d times at call 20 with a 0%% hit ratio, want 1", got)
	}
}

func TestCacheHitAlarmDoesNotFireAboveThreshold(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	g := NewGovernor(testLimits(), fixedClock(now), nil)
	notifier := &fakeNotifier{}
	g.notifier = notifier

	for i := 0; i < 25; i++ {
		g.RecordUsage(context.Background(), noReservation, nil, "trace1", "t1", "claude-sonnet-5",
			Usage{InputTokens: 10, CacheReadInputTokens: 90}, 0.01, "ok", 5, "")
	}
	if got := notifier.countAlarm(EventCacheHitAlarm); got != 0 {
		t.Errorf("EventCacheHitAlarm fired %d times with a 90%% hit ratio, want 0", got)
	}
}

// TestCacheBusterSuspectAfterThreeChanges proves step 4's cache-buster
// detector: >2 changes to system[0]'s hash in one day ledgers
// cache_buster_suspect.
func TestCacheBusterSuspectAfterThreeChanges(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	g := NewGovernor(testLimits(), fixedClock(now), nil)
	ledger := &fakeLedger{}

	hashes := []string{"h1", "h2", "h3", "h4"} // h1->h2, h2->h3, h3->h4 = 3 changes
	for _, h := range hashes {
		g.RecordUsage(context.Background(), noReservation, ledger, "trace1", "t1", "claude-sonnet-5",
			Usage{InputTokens: 1}, 0.001, "ok", 5, h)
	}
	if got := ledger.countKind(EventCacheBusterSuspect); got == 0 {
		t.Error("EventCacheBusterSuspect never ledgered after 3 same-day system-prompt hash changes")
	}
}

func TestCacheBusterNotSuspectForStableHash(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	g := NewGovernor(testLimits(), fixedClock(now), nil)
	ledger := &fakeLedger{}

	for i := 0; i < 10; i++ {
		g.RecordUsage(context.Background(), noReservation, ledger, "trace1", "t1", "claude-sonnet-5",
			Usage{InputTokens: 1}, 0.001, "ok", 5, "stable-hash")
	}
	if got := ledger.countKind(EventCacheBusterSuspect); got != 0 {
		t.Errorf("EventCacheBusterSuspect ledgered %d times for a stable system-prompt hash, want 0", got)
	}
}

// TestModelCallAlwaysLedgered proves step 2's ledger event shape.
func TestModelCallAlwaysLedgered(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	g := NewGovernor(testLimits(), fixedClock(now), nil)
	ledger := &fakeLedger{}

	g.RecordUsage(context.Background(), noReservation, ledger, "trace-xyz", "t1", "claude-sonnet-5",
		Usage{InputTokens: 10, OutputTokens: 20, CacheReadInputTokens: 5, CacheCreationInputTokens: 2}, 0.5, "ok", 123, "")

	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if len(ledger.calls) == 0 {
		t.Fatal("no ledger calls recorded")
	}
	call := ledger.calls[0]
	if call.kind != EventModelCall {
		t.Fatalf("first ledger call kind = %q, want %q", call.kind, EventModelCall)
	}
	if call.traceID != "trace-xyz" {
		t.Errorf("traceID = %q, want trace-xyz", call.traceID)
	}
	for _, key := range []string{"task_id", "model", "input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens", "usd", "status", "duration_ms"} {
		if _, ok := call.payload[key]; !ok {
			t.Errorf("model_call payload missing key %q: %+v", key, call.payload)
		}
	}
}

// TestBootRebuildsTotalsFromFixtureEvents is the boot-time-rebuild
// acceptance criterion: replaying a fixture events table produces the same
// in-memory totals as if those calls had happened live.
func TestBootRebuildsTotalsFromFixtureEvents(t *testing.T) {
	day := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	events := []BootEvent{
		{Ts: day, Record: ModelCallRecord{TaskID: "t1", Model: "claude-sonnet-5", InputTokens: 100_000, OutputTokens: 50_000, USD: 1.5}},
		{Ts: day.Add(time.Hour), Record: ModelCallRecord{TaskID: "t1", Model: "claude-sonnet-5", InputTokens: 50_000, OutputTokens: 25_000, USD: 0.75}},
		{Ts: day.Add(2 * time.Hour), Record: ModelCallRecord{TaskID: "t2", Model: "claude-opus-4-8", InputTokens: 10_000, OutputTokens: 5_000, USD: 3.0}},
	}

	g := Boot(events, testLimits(), fixedClock(day.Add(3*time.Hour)), nil)

	g.mu.Lock()
	defer g.mu.Unlock()

	if got, want := g.perTask["t1"], int64(100_000+50_000+50_000+25_000); got != want {
		t.Errorf("perTask[t1] = %d, want %d", got, want)
	}
	if got, want := g.perTask["t2"], int64(10_000+5_000); got != want {
		t.Errorf("perTask[t2] = %d, want %d", got, want)
	}

	today := day.UTC().Format(dayLayout)
	agg := g.daily[today]
	if agg == nil {
		t.Fatalf("no daily aggregate for %s after Boot", today)
	}
	if agg.calls != 3 {
		t.Errorf("daily calls = %d, want 3", agg.calls)
	}
	wantUSD := 1.5 + 0.75 + 3.0
	if agg.usd != wantUSD {
		t.Errorf("daily usd = %v, want %v", agg.usd, wantUSD)
	}

	month := day.UTC().Format(monthLayout)
	if g.monthly[month] != wantUSD {
		t.Errorf("monthly[%s] = %v, want %v", month, g.monthly[month], wantUSD)
	}
}

// TestBootThenCheckBeforeForwardBlocksAlreadyAtCeiling proves the rebuilt
// totals are actually consulted by CheckBeforeForward, not merely stored.
func TestBootThenCheckBeforeForwardBlocksAlreadyAtCeiling(t *testing.T) {
	day := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	events := []BootEvent{
		{Ts: day, Record: ModelCallRecord{TaskID: "t_old", Model: "claude-sonnet-5", InputTokens: 500_000, USD: 1.0}},
	}
	g := Boot(events, testLimits(), fixedClock(day), nil)

	res := g.CheckBeforeForward("t_old", "claude-sonnet-5", nil)
	if res.Allowed {
		t.Fatal("CheckBeforeForward() after Boot replayed a ceiling-crossing task, want blocked")
	}
}

// TestReservationPreventsConcurrentBurstFromExceedingTaskCeiling is
// BLOCKER 2's core regression test: CheckBeforeForward used to be a plain
// check-then-act (read completed totals, decide, only debit AFTER the
// caller separately finished the whole request and called RecordUsage) -
// a burst of concurrent requests could each observe "under limit" against
// the SAME completed total and jointly blow past the 500K per-task
// ceiling by an unbounded multiple. Converting it into an atomic
// check-and-RESERVE under g.mu closes that window: seeded at 450K/500K,
// with each request's estimate pinned to exactly 40K (EstRequestTokens,
// via an unparseable nil body so the config-default fallback path is the
// one under test, not the body-size heuristic), only ONE of N concurrent
// callers may be granted (450K+40K=490K<=500K); every other one sees that
// first reservation immediately and is blocked
// (450K+40K(reserved)+40K(this one)=530K>500K) - run with -race to prove
// the reservation bookkeeping itself is race-free.
func TestReservationPreventsConcurrentBurstFromExceedingTaskCeiling(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	limits := testLimits()
	limits.EstRequestTokens = 40_000
	g := NewGovernor(limits, fixedClock(now), nil)

	ledger := &fakeLedger{}
	g.RecordUsage(context.Background(), noReservation, ledger, "trace1", "t_burst", "claude-sonnet-5",
		Usage{InputTokens: 450_000}, 1.0, "ok", 100, "")

	const n = 8
	results := make(chan CheckResult, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- g.CheckBeforeForward("t_burst", "claude-sonnet-5", nil)
		}()
	}
	wg.Wait()
	close(results)

	allowed := 0
	for res := range results {
		if res.Allowed {
			allowed++
		} else if res.Message != MsgTaskCeiling {
			t.Errorf("blocked result had message %q, want %q", res.Message, MsgTaskCeiling)
		}
	}
	if allowed != 1 {
		t.Errorf("allowed = %d of %d concurrent requests, want exactly 1 (450K completed + one 40K reservation = 490K <= 500K; a second would reach 530K > 500K)", allowed, n)
	}

	g.mu.Lock()
	total := g.perTask["t_burst"] + g.perTaskReservedTok["t_burst"]
	g.mu.Unlock()
	if total > limits.TaskTokenCeiling {
		t.Errorf("completed+reserved = %d, want <= %d (the 500K ceiling) even under a concurrent burst", total, limits.TaskTokenCeiling)
	}
}

// TestReservationPreventsConcurrentBurstFromExceedingDailyBudget mirrors
// the task-ceiling regression above for the SHARED daily USD budget: the
// budget is process-wide, not per-task, so two DIFFERENT tasks racing
// CheckBeforeForward must still only let one through once their combined
// reservations would otherwise cross $10. Completed spend is seeded at
// $9.50; each request's config-default estimate (40K tokens, output-priced
// at claude-sonnet-5's $10/MTok intro rate = exactly $0.40) means the
// first grant lands at $9.90 (<=$10) and a second concurrent one would
// reach $10.30 (>$10).
func TestReservationPreventsConcurrentBurstFromExceedingDailyBudget(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	limits := testLimits()
	limits.EstRequestTokens = 40_000
	g := NewGovernor(limits, fixedClock(now), nil)

	g.RecordUsage(context.Background(), noReservation, nil, "trace1", "t_a", "claude-sonnet-5",
		Usage{InputTokens: 1}, 9.5, "ok", 10, "")

	taskIDs := []string{"t_a", "t_b"}
	results := make(chan CheckResult, len(taskIDs))
	var wg sync.WaitGroup
	for _, taskID := range taskIDs {
		wg.Add(1)
		go func(taskID string) {
			defer wg.Done()
			results <- g.CheckBeforeForward(taskID, "claude-sonnet-5", nil)
		}(taskID)
	}
	wg.Wait()
	close(results)

	allowed := 0
	for res := range results {
		if res.Allowed {
			allowed++
		} else if res.Message != MsgDailyBudgetBlock {
			t.Errorf("blocked result had message %q, want %q", res.Message, MsgDailyBudgetBlock)
		}
	}
	if allowed != 1 {
		t.Errorf("allowed = %d of %d concurrent requests across two DIFFERENT tasks, want exactly 1 (the daily budget is shared process-wide, not per-task)", allowed, len(taskIDs))
	}
}

// TestFailedRequestReleaseAllowsLaterCall proves the other half of
// BLOCKER 2's fix: a reservation whose request never reaches RecordUsage
// (the proxy's own deferred ReleaseReservation call handles this in
// production - simulated directly here, since this package doesn't itself
// run HTTP) must be released, not leaked - otherwise a single failed
// request would permanently occupy headroom and wrongly block every later
// legitimate call at the same task.
func TestFailedRequestReleaseAllowsLaterCall(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	limits := testLimits()
	limits.TaskTokenCeiling = 100_000
	limits.EstRequestTokens = 90_000
	g := NewGovernor(limits, fixedClock(now), nil)

	first := g.CheckBeforeForward("t_retry", "claude-sonnet-5", nil)
	if !first.Allowed {
		t.Fatalf("first CheckBeforeForward() = %+v, want Allowed=true", first)
	}

	// While the first reservation is still outstanding, a second call must
	// be blocked - proves the reservation really does occupy headroom
	// (90K reserved + 90K this estimate = 180K > 100K ceiling).
	if blocked := g.CheckBeforeForward("t_retry", "claude-sonnet-5", nil); blocked.Allowed {
		t.Fatal("second CheckBeforeForward() while the first reservation is outstanding = Allowed, want blocked")
	}

	// The first request now "fails" (upstream RoundTrip error, or any path
	// that never reaches RecordUsage) - release it, exactly as the proxy's
	// deferred ReleaseReservation call would.
	g.ReleaseReservation(first.Reservation)

	// A later legitimate request must be allowed again - the failed
	// request's reservation must not have leaked permanently.
	if after := g.CheckBeforeForward("t_retry", "claude-sonnet-5", nil); !after.Allowed {
		t.Error("CheckBeforeForward() after releasing the failed request's reservation = blocked, want Allowed (the reservation must not leak)")
	}
}
