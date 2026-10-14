//go:build darwin

package briefing

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// TestLiveCalendarCollectorReadsTodayWithinBudget (FIX-P-9) runs the REAL
// fixed calendar JXA snippet against the user's own Calendar.app and logs
// how long it took. It needs the one-time Calendar Automation grant, so it
// only runs with KAHYA_CALENDAR_LIVE=1 (never in `make test`):
//
//	KAHYA_CALENDAR_LIVE=1 go test -tags sqlite_fts5 -run TestLiveCalendarCollectorReadsTodayWithinBudget -v ./kahyad/internal/briefing/
//
// This is what makes the "large calendar exceeds the budget" finding
// checkable on the user's machine: the whose() snippet must finish well
// inside calendarCollectBudget.
func TestLiveCalendarCollectorReadsTodayWithinBudget(t *testing.T) {
	if os.Getenv("KAHYA_CALENDAR_LIVE") != "1" {
		t.Skip("set KAHYA_CALENDAR_LIVE=1 to read the real Calendar.app (needs the Automation grant)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), calendarCollectBudget)
	defer cancel()
	start := time.Now()
	out, err := ExecCalendarRunner{}.Run(ctx)
	d := time.Since(start)
	if err != nil {
		t.Fatalf("calendar collector after %s: %v", d, err)
	}
	var items []calendarEventJSON
	if err := json.Unmarshal(out, &items); err != nil {
		t.Fatalf("calendar JXA output is not the {title,time,calendar} JSON array: %v (%q)", err, out)
	}
	t.Logf("calendar collector took %s for %d events today", d, len(items))
	if d >= calendarCollectBudget {
		t.Fatalf("calendar collector took %s, want < %s", d, calendarCollectBudget)
	}
}
