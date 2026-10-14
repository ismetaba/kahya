package backup

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/notify"
	"kahya/kahyad/internal/store/sqlcgen"
)

// fakeTMRunner is the TMRunner test double — HARD CONSTRAINT: no test in
// this package ever shells out to the real tmutil.
type fakeTMRunner struct {
	addExclusionCalls     []string
	removeExclusionCalls  []string
	excluded              map[string]bool
	isExcludedErr         error
	destinationConfigured bool
	destinationErr        error
	// recordAdds makes AddExclusion mark the path excluded (as real tmutil
	// does), so IsExcluded verifies it; false models an exclusion that
	// did not stick (FIX-P-18's tm_exclusion_unverified).
	recordAdds bool
}

func (f *fakeTMRunner) AddExclusion(_ context.Context, path string) error {
	f.addExclusionCalls = append(f.addExclusionCalls, path)
	if f.recordAdds {
		if f.excluded == nil {
			f.excluded = map[string]bool{}
		}
		f.excluded[path] = true
	}
	return nil
}

func (f *fakeTMRunner) RemoveExclusion(_ context.Context, path string) error {
	f.removeExclusionCalls = append(f.removeExclusionCalls, path)
	return nil
}

func (f *fakeTMRunner) IsExcluded(_ context.Context, path string) (bool, error) {
	if f.isExcludedErr != nil {
		return false, f.isExcludedErr
	}
	return f.excluded[path], nil
}

func (f *fakeTMRunner) DestinationConfigured(context.Context) (bool, error) {
	return f.destinationConfigured, f.destinationErr
}

// fakeClock is the Clock test double.
type fakeClock struct{ now time.Time }

func (f *fakeClock) Now() time.Time { return f.now }

// newTestTM builds a TimeMachine with no ledger reader (in-memory rate
// limit only) whose existence check reports every path present, so the
// fixed "/appsupport/brain.db" fixtures reach the fake tmutil.
func newTestTM(runner TMRunner, notifier Notifier, clock Clock) *TimeMachine {
	tm := NewTimeMachine(runner, notifier, clock, nil, nil)
	tm.exists = func(string) bool { return true }
	return tm
}

func TestEnsureExclusionsExcludesDBFilesAndClearsBackupDirExclusion(t *testing.T) {
	runner := &fakeTMRunner{excluded: map[string]bool{"/kahya/backups": true}}
	tm := newTestTM(runner, &fakeNotifier{}, &fakeClock{})

	tm.EnsureExclusions(context.Background(), "/appsupport/brain.db", "/kahya/backups")

	wantAdds := []string{"/appsupport/brain.db", "/appsupport/brain.db-wal", "/appsupport/brain.db-shm"}
	if len(runner.addExclusionCalls) != len(wantAdds) {
		t.Fatalf("AddExclusion calls = %v, want %v", runner.addExclusionCalls, wantAdds)
	}
	for i, want := range wantAdds {
		if runner.addExclusionCalls[i] != want {
			t.Errorf("AddExclusion[%d] = %q, want %q", i, runner.addExclusionCalls[i], want)
		}
	}

	if len(runner.removeExclusionCalls) != 1 || runner.removeExclusionCalls[0] != "/kahya/backups" {
		t.Errorf("RemoveExclusion calls = %v, want exactly [\"/kahya/backups\"] (it was reported excluded)", runner.removeExclusionCalls)
	}
}

func TestEnsureExclusionsLeavesUnexcludedBackupDirAlone(t *testing.T) {
	runner := &fakeTMRunner{excluded: map[string]bool{}}
	tm := newTestTM(runner, &fakeNotifier{}, &fakeClock{})

	tm.EnsureExclusions(context.Background(), "/appsupport/brain.db", "/kahya/backups")

	if len(runner.removeExclusionCalls) != 0 {
		t.Errorf("RemoveExclusion calls = %v, want none (backups dir was already not excluded)", runner.removeExclusionCalls)
	}
}

func TestEnsureExclusionsNeverErrorsOnTMUtilFailure(t *testing.T) {
	runner := &fakeTMRunner{isExcludedErr: errors.New("tmutil: boom")}
	tm := newTestTM(runner, &fakeNotifier{}, &fakeClock{})

	// Must not panic and must not block — EnsureExclusions has no return
	// value at all, so simply completing is the assertion.
	tm.EnsureExclusions(context.Background(), "/appsupport/brain.db", "/kahya/backups")
}

// --- (g) no-TM-destination path: event + exact Turkish alarm, rate-limited to once/24h ---

func TestCheckOffsiteNoDestinationAlarmsAndLedgers(t *testing.T) {
	runner := &fakeTMRunner{destinationConfigured: false}
	notifier := &fakeNotifier{}
	clock := &fakeClock{now: time.Date(2026, 7, 12, 3, 0, 0, 0, time.UTC)}
	tm := newTestTM(runner, notifier, clock)

	tm.CheckOffsite(context.Background(), "trace-g1")

	calls := notifier.calls()
	if len(calls) != 1 {
		t.Fatalf("alarm calls = %d, want 1", len(calls))
	}
	if calls[0].kind != EventBackupNoOffsite {
		t.Errorf("alarm kind = %q, want %q", calls[0].kind, EventBackupNoOffsite)
	}
	if calls[0].message != AlarmNoOffsite {
		t.Errorf("alarm message = %q, want %q", calls[0].message, AlarmNoOffsite)
	}
}

func TestCheckOffsiteConfiguredNeverAlarms(t *testing.T) {
	runner := &fakeTMRunner{destinationConfigured: true}
	notifier := &fakeNotifier{}
	clock := &fakeClock{now: time.Now()}
	tm := newTestTM(runner, notifier, clock)

	tm.CheckOffsite(context.Background(), "trace-g-configured")

	if len(notifier.calls()) != 0 {
		t.Errorf("alarm calls = %+v, want none when a destination is configured", notifier.calls())
	}
}

func TestCheckOffsiteRateLimitedToOncePerDay(t *testing.T) {
	runner := &fakeTMRunner{destinationConfigured: false}
	notifier := &fakeNotifier{}
	clock := &fakeClock{now: time.Date(2026, 7, 12, 3, 0, 0, 0, time.UTC)}
	tm := newTestTM(runner, notifier, clock)

	tm.CheckOffsite(context.Background(), "trace-g2a")
	if got := len(notifier.calls()); got != 1 {
		t.Fatalf("after 1st check: alarm calls = %d, want 1", got)
	}

	// Second call less than 24h later: still just 1 total.
	clock.now = clock.now.Add(23 * time.Hour)
	tm.CheckOffsite(context.Background(), "trace-g2b")
	if got := len(notifier.calls()); got != 1 {
		t.Fatalf("after 2nd check (<24h later): alarm calls = %d, want still 1", got)
	}

	// Third call, now >24h after the FIRST alarm: fires again, total 2.
	clock.now = clock.now.Add(2 * time.Hour) // cumulative +25h from the first alarm
	tm.CheckOffsite(context.Background(), "trace-g2c")
	if got := len(notifier.calls()); got != 2 {
		t.Fatalf("after 3rd check (>24h later): alarm calls = %d, want 2", got)
	}
}

func TestCheckOffsiteTMUtilErrorNeverAlarms(t *testing.T) {
	runner := &fakeTMRunner{destinationErr: errors.New("tmutil: boom")}
	notifier := &fakeNotifier{}
	tm := newTestTM(runner, notifier, &fakeClock{})

	tm.CheckOffsite(context.Background(), "trace-g-err")

	if len(notifier.calls()) != 0 {
		t.Errorf("alarm calls = %+v, want none when tmutil itself errors (best-effort diagnostic only)", notifier.calls())
	}
}

// ---- FIX-P-18: restart-proof rate limit, guarded + verified exclusions ----

// TestCheckOffsiteRateLimitPersistsAcrossRestart (FIX-P-18): a NEW
// TimeMachine (a daemon restart) reads the last backup.no_offsite ledger
// row, so a restart 1h after the alarm stays silent; 25h after, it fires.
func TestCheckOffsiteRateLimitPersistsAcrossRestart(t *testing.T) {
	st := testStore(t)
	notifier := notify.New(nil, st)
	// The ledger stamps rows with the real wall clock, so the fake clock
	// starts there.
	clock := &fakeClock{now: time.Now()}
	runner := &fakeTMRunner{destinationConfigured: false}
	count := func() int {
		t.Helper()
		var n int
		if err := st.DB().QueryRow(`SELECT count(*) FROM events WHERE kind = ?`, EventBackupNoOffsite).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	NewTimeMachine(runner, notifier, clock, st.Queries, nil).CheckOffsite(context.Background(), "trace-p18-boot-1")
	if n := count(); n != 1 {
		t.Fatalf("after the first boot: %s rows = %d, want 1", EventBackupNoOffsite, n)
	}

	clock.now = clock.now.Add(time.Hour)
	tm2 := NewTimeMachine(runner, notifier, clock, st.Queries, nil) // restart
	tm2.CheckOffsite(context.Background(), "trace-p18-boot-2")
	if n := count(); n != 1 {
		t.Fatalf("after a restart 1h later: %s rows = %d, want still 1", EventBackupNoOffsite, n)
	}

	clock.now = clock.now.Add(24 * time.Hour)
	tm2.CheckOffsite(context.Background(), "trace-p18-boot-2b")
	if n := count(); n != 2 {
		t.Fatalf("25h after the first alarm: %s rows = %d, want 2", EventBackupNoOffsite, n)
	}
}

// failingEventReader is an EventReader whose lookup always fails.
type failingEventReader struct{}

func (failingEventReader) GetLatestEventByKind(context.Context, string) (sqlcgen.Event, error) {
	return sqlcgen.Event{}, errors.New("events: injected lookup failure")
}

// fixedEventReader always returns one event of the asked kind.
type fixedEventReader struct{ createdAt string }

func (f fixedEventReader) GetLatestEventByKind(_ context.Context, kind string) (sqlcgen.Event, error) {
	return sqlcgen.Event{ID: 1, Kind: kind, CreatedAt: f.createdAt, Ts: f.createdAt}, nil
}

// TestCheckOffsiteLedgerLookupErrorFallsBackToMemoryLimit (FIX-P-18): a
// failing ledger lookup never blocks the alarm; the in-memory 24h limit
// still applies (1 alarm, none 23h later, a second 25h later).
func TestCheckOffsiteLedgerLookupErrorFallsBackToMemoryLimit(t *testing.T) {
	runner := &fakeTMRunner{destinationConfigured: false}
	notifier := &fakeNotifier{}
	clock := &fakeClock{now: time.Date(2026, 7, 12, 3, 0, 0, 0, time.UTC)}
	tm := NewTimeMachine(runner, notifier, clock, failingEventReader{}, nil)

	tm.CheckOffsite(context.Background(), "trace-p18-lookup-1")
	if got := len(notifier.calls()); got != 1 {
		t.Fatalf("after the 1st check: alarm calls = %d, want 1", got)
	}
	clock.now = clock.now.Add(23 * time.Hour)
	tm.CheckOffsite(context.Background(), "trace-p18-lookup-2")
	if got := len(notifier.calls()); got != 1 {
		t.Fatalf("23h later: alarm calls = %d, want still 1", got)
	}
	clock.now = clock.now.Add(2 * time.Hour)
	tm.CheckOffsite(context.Background(), "trace-p18-lookup-3")
	if got := len(notifier.calls()); got != 2 {
		t.Fatalf("25h later: alarm calls = %d, want 2", got)
	}
}

// TestCheckOffsiteFutureDatedLedgerAlarmDoesNotSuppress (FIX-P-18): a last
// alarm stamped AFTER now (the clock moved back) must not silence the
// alarm for that long - the missing-offsite alarm errs toward firing.
func TestCheckOffsiteFutureDatedLedgerAlarmDoesNotSuppress(t *testing.T) {
	runner := &fakeTMRunner{destinationConfigured: false}
	notifier := &fakeNotifier{}
	now := time.Date(2026, 7, 12, 3, 0, 0, 0, time.UTC)
	future := now.Add(30 * 24 * time.Hour).Format(time.RFC3339Nano)
	tm := NewTimeMachine(runner, notifier, &fakeClock{now: now}, fixedEventReader{createdAt: future}, nil)

	tm.CheckOffsite(context.Background(), "trace-p18-future")
	if got := len(notifier.calls()); got != 1 {
		t.Fatalf("alarm calls = %d, want 1 (a future-dated last alarm does not suppress)", got)
	}
}

// readJSONLEvents returns the event names of every line in logDir's
// kahyad.jsonl.
func readJSONLEvents(t *testing.T, logDir string) []string {
	t.Helper()
	f, err := os.Open(filepath.Join(logDir, "kahyad.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var line struct {
			Event   string `json:"event"`
			TraceID string `json:"trace_id"`
		}
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatalf("bad JSONL line %q: %v", sc.Text(), err)
		}
		if line.TraceID == "" {
			t.Fatalf("JSONL line without trace_id: %q", sc.Text())
		}
		out = append(out, line.Event)
	}
	return out
}

func countEvent(events []string, name string) int {
	n := 0
	for _, e := range events {
		if e == name {
			n++
		}
	}
	return n
}

func newTMTestLogger(t *testing.T) (*logx.Logger, string) {
	t.Helper()
	dir := t.TempDir()
	log, err := logx.New(dir, "boot0123456789abcdef0123456789ab")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	return log, dir
}

// TestEnsureExclusionsSkipsMissingSiblingsWithInfoNotWarn (FIX-P-18): a
// -wal/-shm sibling that does not exist yet is skipped with an info line
// (tm_exclusion_skipped_missing), never handed to tmutil or warned about.
func TestEnsureExclusionsSkipsMissingSiblingsWithInfoNotWarn(t *testing.T) {
	runner := &fakeTMRunner{recordAdds: true}
	log, dir := newTMTestLogger(t)
	tm := NewTimeMachine(runner, &fakeNotifier{}, &fakeClock{}, nil, log)
	tm.exists = func(p string) bool { return p == "/appsupport/brain.db" }

	tm.EnsureExclusions(context.Background(), "/appsupport/brain.db", "/kahya/backups")

	if len(runner.addExclusionCalls) != 1 || runner.addExclusionCalls[0] != "/appsupport/brain.db" {
		t.Fatalf("AddExclusion calls = %v, want only the existing brain.db", runner.addExclusionCalls)
	}
	events := readJSONLEvents(t, dir)
	if n := countEvent(events, "tm_exclusion_skipped_missing"); n != 2 {
		t.Fatalf("tm_exclusion_skipped_missing lines = %d, want 2 (events %v)", n, events)
	}
	if n := countEvent(events, "tm_add_exclusion_failed"); n != 0 {
		t.Fatalf("tm_add_exclusion_failed lines = %d, want 0", n)
	}
	if n := countEvent(events, "tm_exclusion_unverified"); n != 0 {
		t.Fatalf("tm_exclusion_unverified lines = %d, want 0 (the exclusion stuck)", n)
	}
}

// TestEnsureExclusionsVerifiesAfterAdd (FIX-P-18): every added exclusion
// is re-checked with isexcluded; one that did not stick is a
// tm_exclusion_unverified warning.
func TestEnsureExclusionsVerifiesAfterAdd(t *testing.T) {
	runner := &fakeTMRunner{excluded: map[string]bool{}} // adds never stick
	log, dir := newTMTestLogger(t)
	tm := NewTimeMachine(runner, &fakeNotifier{}, &fakeClock{}, nil, log)
	tm.exists = func(string) bool { return true }

	tm.EnsureExclusions(context.Background(), "/appsupport/brain.db", "/kahya/backups")

	if n := countEvent(readJSONLEvents(t, dir), "tm_exclusion_unverified"); n != 3 {
		t.Fatalf("tm_exclusion_unverified lines = %d, want 3", n)
	}
}

// TestEnsureExclusionsRealFilesystemExistenceCheck (FIX-P-18): the
// production existence check is a real os.Stat - only the files that
// exist on disk reach tmutil.
func TestEnsureExclusionsRealFilesystemExistenceCheck(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "brain.db")
	if err := os.WriteFile(db, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(db+"-wal", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeTMRunner{recordAdds: true}
	NewTimeMachine(runner, &fakeNotifier{}, &fakeClock{}, nil, nil).EnsureExclusions(context.Background(), db, dir)

	if len(runner.addExclusionCalls) != 2 || runner.addExclusionCalls[0] != db || runner.addExclusionCalls[1] != db+"-wal" {
		t.Fatalf("AddExclusion calls = %v, want [brain.db brain.db-wal] (no -shm on disk)", runner.addExclusionCalls)
	}
}
