package briefing

import (
	"context"
	"testing"
	"time"
)

// TestStoreDedupeCheckerWindowIsHalfOpen (FIX-P-14): the production dedupe
// window is [dayStart, dayEnd) over real created_at instants: a delivery
// exactly at dayStart (or a fraction of a second after) counts, one exactly
// at dayEnd belongs to the next day, and one just before dayStart to the
// previous one. julianday() keeps fractional-second rows ordered correctly.
func TestStoreDedupeCheckerWindowIsHalfOpen(t *testing.T) {
	ist := istanbulZone()
	dayStart := time.Date(2026, 7, 13, 0, 0, 0, 0, ist) // 2026-07-12T21:00:00Z
	dayEnd := dayStart.AddDate(0, 0, 1)
	for _, tc := range []struct {
		name string
		at   time.Time
		want bool
	}{
		{"at_day_start", dayStart, true},
		{"fractional_after_start", dayStart.Add(500 * time.Millisecond), true},
		{"just_before_day_end", dayEnd.Add(-time.Millisecond), true},
		{"at_day_end", dayEnd, false},
		{"just_before_day_start", dayStart.Add(-time.Nanosecond), false},
		{"previous_ms", dayStart.Add(-time.Millisecond), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newTestStore(t)
			seedDelivered(t, st, tc.at)
			got, err := StoreDedupeChecker{Store: st.Queries}.AlreadyDelivered(context.Background(), dayStart, dayEnd)
			if err != nil {
				t.Fatalf("AlreadyDelivered: %v", err)
			}
			if got != tc.want {
				t.Errorf("delivery at %s: AlreadyDelivered(%s, %s) = %v, want %v",
					tc.at.UTC().Format(time.RFC3339Nano), dayStart.UTC().Format(time.RFC3339Nano), dayEnd.UTC().Format(time.RFC3339Nano), got, tc.want)
			}
		})
	}
}

// TestStoreDedupeCheckerCountsOnlyDeliveredRows (FIX-P-14): other briefing
// kinds inside the window never count as a delivery.
func TestStoreDedupeCheckerCountsOnlyDeliveredRows(t *testing.T) {
	st := newTestStore(t)
	if err := st.LogEvent(context.Background(), "trace-x", EventSkippedDuplicate, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	got, err := StoreDedupeChecker{Store: st.Queries}.AlreadyDelivered(context.Background(), now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil || got {
		t.Fatalf("AlreadyDelivered with only a skipped_duplicate row = %v, %v; want false, nil", got, err)
	}
}
