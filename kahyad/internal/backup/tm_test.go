package backup

import (
	"context"
	"errors"
	"testing"
	"time"
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
}

func (f *fakeTMRunner) AddExclusion(_ context.Context, path string) error {
	f.addExclusionCalls = append(f.addExclusionCalls, path)
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

func TestEnsureExclusionsExcludesDBFilesAndClearsBackupDirExclusion(t *testing.T) {
	runner := &fakeTMRunner{excluded: map[string]bool{"/kahya/backups": true}}
	tm := NewTimeMachine(runner, &fakeNotifier{}, &fakeClock{}, nil)

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
	tm := NewTimeMachine(runner, &fakeNotifier{}, &fakeClock{}, nil)

	tm.EnsureExclusions(context.Background(), "/appsupport/brain.db", "/kahya/backups")

	if len(runner.removeExclusionCalls) != 0 {
		t.Errorf("RemoveExclusion calls = %v, want none (backups dir was already not excluded)", runner.removeExclusionCalls)
	}
}

func TestEnsureExclusionsNeverErrorsOnTMUtilFailure(t *testing.T) {
	runner := &fakeTMRunner{isExcludedErr: errors.New("tmutil: boom")}
	tm := NewTimeMachine(runner, &fakeNotifier{}, &fakeClock{}, nil)

	// Must not panic and must not block — EnsureExclusions has no return
	// value at all, so simply completing is the assertion.
	tm.EnsureExclusions(context.Background(), "/appsupport/brain.db", "/kahya/backups")
}

// --- (g) no-TM-destination path: event + exact Turkish alarm, rate-limited to once/24h ---

func TestCheckOffsiteNoDestinationAlarmsAndLedgers(t *testing.T) {
	runner := &fakeTMRunner{destinationConfigured: false}
	notifier := &fakeNotifier{}
	clock := &fakeClock{now: time.Date(2026, 7, 12, 3, 0, 0, 0, time.UTC)}
	tm := NewTimeMachine(runner, notifier, clock, nil)

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
	tm := NewTimeMachine(runner, notifier, clock, nil)

	tm.CheckOffsite(context.Background(), "trace-g-configured")

	if len(notifier.calls()) != 0 {
		t.Errorf("alarm calls = %+v, want none when a destination is configured", notifier.calls())
	}
}

func TestCheckOffsiteRateLimitedToOncePerDay(t *testing.T) {
	runner := &fakeTMRunner{destinationConfigured: false}
	notifier := &fakeNotifier{}
	clock := &fakeClock{now: time.Date(2026, 7, 12, 3, 0, 0, 0, time.UTC)}
	tm := NewTimeMachine(runner, notifier, clock, nil)

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
	tm := NewTimeMachine(runner, notifier, &fakeClock{}, nil)

	tm.CheckOffsite(context.Background(), "trace-g-err")

	if len(notifier.calls()) != 0 {
		t.Errorf("alarm calls = %+v, want none when tmutil itself errors (best-effort diagnostic only)", notifier.calls())
	}
}
