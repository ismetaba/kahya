package briefing

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCollectCalendarParsesTitleAndTime(t *testing.T) {
	runner := fakeCalendarRunner{JSON: []byte(`[{"title":"Diş randevusu","time":"09:30","calendar":"Ev"}]`)}
	events, err := CollectCalendar(context.Background(), runner, nil)
	if err != nil {
		t.Fatalf("CollectCalendar: %v", err)
	}
	if len(events) != 1 || events[0].Title != "Diş randevusu" || events[0].Time != "09:30" {
		t.Fatalf("events = %+v, unexpected", events)
	}
}

func TestCollectCalendarFiltersByName(t *testing.T) {
	runner := fakeCalendarRunner{JSON: []byte(`[
		{"title":"İş toplantısı","time":"10:00","calendar":"İş"},
		{"title":"Doğum günü","time":"18:00","calendar":"Ev"}
	]`)}
	events, err := CollectCalendar(context.Background(), runner, []string{"İş"})
	if err != nil {
		t.Fatalf("CollectCalendar: %v", err)
	}
	if len(events) != 1 || events[0].Title != "İş toplantısı" {
		t.Fatalf("events = %+v, want only the İş calendar's event", events)
	}
}

// TestCollectCalendarMissingGrantReturnsErrCalendarNoAccess is the
// hermetic double for the runtime-only TCC-revoked acceptance criterion:
// a CalendarRunner reporting ErrCalendarNoAccess (never a real TCC dialog)
// must surface that sentinel UNCHANGED, so Orchestrator.Run can turn it
// into the byte-exact "Takvim erişimi yok" line instead of failing the
// whole briefing.
func TestCollectCalendarMissingGrantReturnsErrCalendarNoAccess(t *testing.T) {
	runner := fakeCalendarRunner{Err: ErrCalendarNoAccess}
	_, err := CollectCalendar(context.Background(), runner, nil)
	if !errors.Is(err, ErrCalendarNoAccess) {
		t.Fatalf("CollectCalendar err = %v, want ErrCalendarNoAccess", err)
	}
}

func TestIsCalendarAuthErrorRecognizesDashed1743(t *testing.T) {
	if !isCalendarAuthError("execution error: Not authorized to send Apple events to Calendar. (-1743)") {
		t.Fatal("isCalendarAuthError: want true for a -1743 stderr")
	}
	if isCalendarAuthError("some other unrelated error") {
		t.Fatal("isCalendarAuthError: want false for an unrelated error")
	}
}

func TestCollectCalendarNilRunnerIsNoop(t *testing.T) {
	events, err := CollectCalendar(context.Background(), nil, nil)
	if err != nil || events != nil {
		t.Fatalf("CollectCalendar(nil runner) = %v, %v, want nil, nil", events, err)
	}
}

// TestCalendarJXAUsesWhoseFilterAndBatchReads (FIX-P-9): the fixed calendar
// snippet filters today's events inside Calendar.app (events.whose on
// startDate) and reads summary/startDate in one batched Apple Event per
// calendar - never a JS loop over every event with a per-event property
// read. It stays read-only and free of every escape body
// mcp/osascript/scan.go rejects for model-authored scripts.
func TestCalendarJXAUsesWhoseFilterAndBatchReads(t *testing.T) {
	for _, want := range []string{
		"events.whose(",
		"_greaterThanEquals",
		"_lessThanEquals",
		"todays.summary()",
		"todays.startDate()",
	} {
		if !strings.Contains(calendarJXA, want) {
			t.Errorf("calendarJXA lacks %q", want)
		}
	}
	for _, banned := range []string{
		"cal.events()",   // every event of the calendar, resolved in JS
		"ev.startDate()", // one Apple Event per event
		"do shell script",
		"doShellScript",
		"NSTask",
		"NSAppleScript",
		"ObjC.import",
		"ObjC",
		"$.", // the JXA ObjC bridge shorthand
	} {
		if strings.Contains(calendarJXA, banned) {
			t.Errorf("calendarJXA contains %q", banned)
		}
	}
}
