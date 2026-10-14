package anthproxy

import (
	"context"
	"testing"
	"time"

	"kahya/kahyad/internal/router"
)

// istanbul is Europe/Istanbul as a fixed +03:00 zone: no DST since 2016,
// and time.LoadLocation("Europe/Istanbul") would need tzdata.
func istanbul() *time.Location { return time.FixedZone("UTC+3", 3*3600) }

// TestDailyBucketUsesConfiguredTimezone (FIX-U-1): spend at 02:30 Istanbul
// (23:30Z the previous UTC day) lands in the Istanbul day's bucket, so at
// 03:30 Istanbul - the same local day but the next UTC day - the daily
// budget still blocks and the downgrade rung is still on. A UTC bucket
// reset an Istanbul user's daily budget at 03:00 local.
func TestDailyBucketUsesConfiguredTimezone(t *testing.T) {
	spendAt := time.Date(2026, 7, 10, 23, 30, 0, 0, time.UTC) // 02:30 Istanbul, 11 July
	checkAt := time.Date(2026, 7, 11, 0, 30, 0, 0, time.UTC)  // 03:30 Istanbul, same local day, next UTC day
	limits := testLimits()
	limits.Location = istanbul()
	limits.DailyBudgetUSD = 1
	clock := spendAt
	g := NewGovernor(limits, func() time.Time { return clock }, nil)
	g.RecordUsage(context.Background(), noReservation, nil, "trace1", "t_tz", router.ModelSonnet,
		Usage{InputTokens: 1000, OutputTokens: 1000}, 1.0, "ok", 50, "")

	g.mu.Lock()
	_, inUTCDay := g.daily["2026-07-10"]
	_, inLocalDay := g.daily["2026-07-11"]
	g.mu.Unlock()
	if inUTCDay || !inLocalDay {
		t.Fatalf("daily buckets: utc-day present=%v local-day present=%v, want the spend keyed on the Istanbul day 2026-07-11 only", inUTCDay, inLocalDay)
	}

	clock = checkAt
	if res := g.CheckBeforeForward("t_tz", router.ModelSonnet, nil); res.Allowed || res.Message != MsgDailyBudgetBlock {
		t.Fatalf("CheckBeforeForward() at 03:30 Istanbul = %+v, want blocked with %q: the $1 spent at 02:30 Istanbul is the same local day", res, MsgDailyBudgetBlock)
	}
	if !g.Downgraded() {
		t.Error("Downgraded() at 03:30 Istanbul = false, want true (same local day, spend >= 80% of the daily budget)")
	}

	// The next local day (00:30 Istanbul on 12 July = 21:30Z on 11 July)
	// starts a fresh bucket.
	clock = time.Date(2026, 7, 11, 21, 30, 0, 0, time.UTC)
	if res := g.CheckBeforeForward("t_tz2", router.ModelSonnet, []byte(`{"max_tokens":10}`)); !res.Allowed {
		t.Fatalf("CheckBeforeForward() on the next Istanbul day = %+v, want allowed", res)
	}
}

// TestMonthlyBucketUsesConfiguredTimezone (FIX-U-1): spend at 01:00 on
// 1 August Istanbul (22:00Z on 31 July) is August spend.
func TestMonthlyBucketUsesConfiguredTimezone(t *testing.T) {
	limits := testLimits()
	limits.Location = istanbul()
	limits.DailyBudgetUSD = 0 // isolate the monthly bucket
	limits.MonthlyBudgetUSD = 1
	clock := time.Date(2026, 7, 31, 22, 0, 0, 0, time.UTC)
	g := NewGovernor(limits, func() time.Time { return clock }, nil)
	g.RecordUsage(context.Background(), noReservation, nil, "trace1", "t_tz", router.ModelSonnet,
		Usage{InputTokens: 1000, OutputTokens: 1000}, 1.0, "ok", 50, "")

	g.mu.Lock()
	_, inUTCMonth := g.monthly["2026-07"]
	_, inLocalMonth := g.monthly["2026-08"]
	g.mu.Unlock()
	if inUTCMonth || !inLocalMonth {
		t.Fatalf("monthly buckets: 2026-07 present=%v 2026-08 present=%v, want the spend keyed on the Istanbul month 2026-08 only", inUTCMonth, inLocalMonth)
	}

	clock = time.Date(2026, 8, 1, 0, 30, 0, 0, time.UTC)
	if res := g.CheckBeforeForward("t_tz", router.ModelSonnet, nil); res.Allowed || res.Message != MsgMonthlyBudgetBlock {
		t.Fatalf("CheckBeforeForward() = %+v, want blocked with %q", res, MsgMonthlyBudgetBlock)
	}
}

// TestBootBucketsHistoryInConfiguredTimezone (FIX-U-1): a restart replays
// ledger rows (UTC ts) into the same local-day bucket, so the 02:30
// Istanbul spend is not forgotten at 03:30.
func TestBootBucketsHistoryInConfiguredTimezone(t *testing.T) {
	limits := testLimits()
	limits.Location = istanbul()
	limits.DailyBudgetUSD = 1
	events := []BootEvent{{
		Ts:     time.Date(2026, 7, 10, 23, 30, 0, 0, time.UTC),
		Record: ModelCallRecord{TaskID: "t_old", Model: router.ModelSonnet, USD: 1.0},
	}}
	g := Boot(events, limits, fixedClock(time.Date(2026, 7, 11, 0, 30, 0, 0, time.UTC)), nil)
	if res := g.CheckBeforeForward("t_new", router.ModelSonnet, nil); res.Allowed {
		t.Fatalf("CheckBeforeForward() after Boot = %+v, want blocked (same Istanbul day as the replayed spend)", res)
	}
}

// TestNewGovernorNilLocationFallsBackToLocal (FIX-U-1): an unset zone means
// time.Local (config.Config.Location()'s own default), never UTC.
func TestNewGovernorNilLocationFallsBackToLocal(t *testing.T) {
	if g := NewGovernor(Limits{}, nil, nil); g.loc != time.Local {
		t.Fatalf("NewGovernor(Limits{}).loc = %v, want time.Local", g.loc)
	}
}

// TestReservationReleasedFromItsLocalDayBucket (FIX-U-1): a reservation
// taken at 23:59 local is released from that local day's bucket even when
// the request completes after local midnight - the reserved pool of the
// old day returns to zero and the new day holds only the real spend.
func TestReservationReleasedFromItsLocalDayBucket(t *testing.T) {
	limits := testLimits()
	limits.Location = istanbul()
	clock := time.Date(2026, 7, 11, 20, 59, 0, 0, time.UTC) // 23:59 Istanbul, 11 July
	g := NewGovernor(limits, func() time.Time { return clock }, nil)
	res := g.CheckBeforeForward("t_res", router.ModelSonnet, []byte(`{"max_tokens":100}`))
	if !res.Allowed {
		t.Fatalf("CheckBeforeForward = %+v, want allowed", res)
	}
	clock = time.Date(2026, 7, 11, 21, 1, 0, 0, time.UTC) // 00:01 Istanbul, 12 July
	g.RecordUsage(context.Background(), res.Reservation, nil, "trace-res", "t_res", router.ModelSonnet,
		Usage{InputTokens: 10, OutputTokens: 10}, 0.01, "ok", 5, "")

	g.mu.Lock()
	defer g.mu.Unlock()
	if r := g.dailyReservedUSD["2026-07-11"]; r != 0 {
		t.Errorf("reserved usd left on 2026-07-11 = %v, want 0", r)
	}
	if agg := g.daily["2026-07-12"]; agg == nil || agg.usd != 0.01 {
		t.Errorf("2026-07-12 bucket = %+v, want the real 0.01 spend", agg)
	}
}
