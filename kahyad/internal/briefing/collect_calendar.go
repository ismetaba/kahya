// collect_calendar.go implements the W5-01 local macOS Calendar collector:
// a COMPILE-TIME-CONSTANT JXA snippet run via `osascript` (read-only, no
// `do shell script`, title+time only, today's events). This is
// kahyad-fixed code, not a model-authored script body - HANDOFF §5 safety
// #6 ⚑'s osascript/JXA W2-floor rule governs the MODEL-facing W3-09 tool
// (mcp/osascript's applescript_run/jxa_run/shortcuts_run); this constant,
// never-interpolated snippet falls under that rule's own documented
// "narrow arg-validated host set" carve-out. If this snippet ever needs a
// dynamic argument, it must go through W3-09 instead - it is fixed and
// argument-free specifically so it never has to.
//
// The 03:00/08:30 launchd run cannot show a TCC dialog - this collector
// requires a one-time Calendar Automation grant approved manually, during
// the day (task spec step 9, tracked as a runtime/user-assist item in
// tasks/w5-proactivity/W5-01-morning-briefing.md, never exercised with a
// real TCC dialog in this package's own hermetic tests). Missing grant =>
// CollectCalendar returns ErrCalendarNoAccess, which Orchestrator.Run
// (briefing.go) turns into the section-level, byte-exact
// "Takvim erişimi yok" line - never a fatal error for the whole briefing.
// A collector that runs out of calendarCollectBudget instead yields its own
// line, MsgCalendarTimeout (FIX-P-9): a timeout does not mean a grant is
// missing, so the user is never told to grant one.
package briefing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// calendarJXA is read-only (only ever calls `.calendars()`, `.name()`,
// the `events.whose(...)` filter and the batched `.summary()`/
// `.startDate()` property reads - no `do shell script`, no ObjC bridge, no
// write-shaped method anywhere in this string) and returns today's events
// (local wall-clock midnight to midnight) as a JSON array of {title, time,
// calendar} objects - title and (HH:MM) time only, per the task spec
// ("title+time only").
//
// FIX-P-9: whose() filter + batched summary()/startDate() reads - O(calendars)
// Apple Events instead of O(events). The date filter runs inside
// Calendar.app (one Apple Event per calendar), never a JS loop over every
// event a calendar has ever held, which on a large synced calendar could
// take longer than calendarCollectBudget. The JSON shape is unchanged.
const calendarJXA = `(function () {
  var Calendar = Application('Calendar');
  var now = new Date();
  var start = new Date(now.getFullYear(), now.getMonth(), now.getDate(), 0, 0, 0);
  var end = new Date(now.getFullYear(), now.getMonth(), now.getDate(), 23, 59, 59);
  var out = [];
  var cals = Calendar.calendars();
  for (var i = 0; i < cals.length; i++) {
    var cal = cals[i];
    var calName = String(cal.name());
    var todays = cal.events.whose({_and: [
      {startDate: {_greaterThanEquals: start}},
      {startDate: {_lessThanEquals: end}}
    ]});
    var titles = todays.summary();
    var starts = todays.startDate();
    for (var j = 0; j < titles.length; j++) {
      var sd = starts[j];
      var hh = ('0' + sd.getHours()).slice(-2);
      var mm = ('0' + sd.getMinutes()).slice(-2);
      out.push({title: String(titles[j]), time: hh + ':' + mm, calendar: calName});
    }
  }
  return JSON.stringify(out);
})();`

// ErrCalendarNoAccess is returned by CollectCalendar (and by
// ExecCalendarRunner.Run) whenever the one-time Calendar Automation TCC
// grant is missing/revoked - osascript exits non-zero with a "not
// authorized"/-1743 stderr in that case. errors.Is-comparable so callers
// (Orchestrator.Run) can distinguish "no grant yet" (skip the section,
// never fail the whole briefing) from any other, genuine collector error.
var ErrCalendarNoAccess = errors.New("briefing: calendar automation not authorized (missing TCC grant)")

// CalendarRunner is the narrow "run the fixed calendar JXA snippet,
// return its raw JSON stdout" seam. ExecCalendarRunner is the production
// implementation; tests inject a fake that never shells out at all -
// including one that returns ErrCalendarNoAccess directly, to exercise
// the missing-grant fallback without a real TCC dialog (this package's
// own hermetic acceptance criterion).
type CalendarRunner interface {
	Run(ctx context.Context) ([]byte, error)
}

// ExecCalendarRunner is the production CalendarRunner: `osascript -l
// JavaScript -e <calendarJXA>`.
type ExecCalendarRunner struct{}

// calendarKillWaitDelay bounds how long Run waits for osascript's output
// pipes after its ctx killed it (exec.Cmd.WaitDelay) - the same backstop
// the gh collector uses (FIX-P-8's ghKillWaitDelay), so a pipe held open by
// anything osascript left behind can never stretch the calendar budget.
const calendarKillWaitDelay = 2 * time.Second

var _ CalendarRunner = ExecCalendarRunner{}

// Run implements CalendarRunner.
func (ExecCalendarRunner) Run(ctx context.Context) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "osascript", "-l", "JavaScript", "-e", calendarJXA)
	cmd.WaitDelay = calendarKillWaitDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if isCalendarAuthError(stderr.String()) {
			return nil, ErrCalendarNoAccess
		}
		return nil, fmt.Errorf("briefing: osascript calendar collector: %w: %s", err, stderr.String())
	}
	return stdout.Bytes(), nil
}

// isCalendarAuthError reports whether osascript's stderr indicates the
// Calendar Automation TCC grant is missing/revoked - macOS surfaces this
// as AppleEvent error -1743 ("not authorized to send Apple events").
func isCalendarAuthError(stderr string) bool {
	lower := strings.ToLower(stderr)
	return strings.Contains(stderr, "-1743") ||
		strings.Contains(lower, "not authorized") ||
		strings.Contains(lower, "not allowed")
}

// calendarTitleMaxLen is this collector's own length cap (charclass-
// constrained via capText) - an event title is a short label, never a
// paragraph.
const calendarTitleMaxLen = 200

// CalendarEvent is one collected, already length/charclass-capped
// calendar signal: title and time only (task spec: "title+time only").
type CalendarEvent struct {
	Title string
	Time  string
}

type calendarEventJSON struct {
	Title    string `json:"title"`
	Time     string `json:"time"`
	Calendar string `json:"calendar"`
}

// CollectCalendar runs runner and parses its JSON output into
// []CalendarEvent, filtering to names when non-empty (empty means "every
// calendar"). A nil runner is a documented no-op (nil, nil) - the
// calendar section simply has zero items until one is wired. Returns
// ErrCalendarNoAccess UNCHANGED (errors.Is-comparable) whenever runner
// itself reports the TCC grant is missing - Orchestrator.Run is the ONE
// place that turns this into the byte-exact "Takvim erişimi yok" line,
// never failing the whole briefing.
func CollectCalendar(ctx context.Context, runner CalendarRunner, names []string) ([]CalendarEvent, error) {
	if runner == nil {
		return nil, nil
	}
	out, err := runner.Run(ctx)
	if err != nil {
		if errors.Is(err, ErrCalendarNoAccess) {
			return nil, ErrCalendarNoAccess
		}
		return nil, err
	}

	var items []calendarEventJSON
	if err := json.Unmarshal(out, &items); err != nil {
		return nil, fmt.Errorf("briefing: parse calendar JXA output: %w", err)
	}

	var nameFilter map[string]bool
	if len(names) > 0 {
		nameFilter = make(map[string]bool, len(names))
		for _, n := range names {
			nameFilter[n] = true
		}
	}

	events := make([]CalendarEvent, 0, len(items))
	for _, it := range items {
		if nameFilter != nil && !nameFilter[it.Calendar] {
			continue
		}
		events = append(events, CalendarEvent{
			Title: capText(it.Title, calendarTitleMaxLen),
			Time:  capText(it.Time, 5),
		})
	}
	return events, nil
}

// calendarItems adapts a []CalendarEvent into []CollectedItem (gate.go) -
// no Path (calendar items carry no filesystem path), Section="calendar".
func calendarItems(events []CalendarEvent) []CollectedItem {
	items := make([]CollectedItem, len(events))
	for i, e := range events {
		items[i] = CollectedItem{Section: "calendar", Text: fmt.Sprintf("%s %s", e.Time, e.Title)}
	}
	return items
}
