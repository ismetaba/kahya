package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"kahya/kahyad/internal/config"
)

// FIX-FINAL scheduled-jobs-ignore-lifecycle (FIX-I-5 / G00 C-5): Trigger
// ran every job handler on context.Background() in an untracked goroutine,
// so no SIGTERM ever reached morning-briefing / nightly-consolidation /
// backup / push / ritual, their cloud workers outlived kahyad, and a run
// could race the deferred st.Close().

func schedulerWithJob(t *testing.T, fn Handler) (*Scheduler, *fakeEventLogger) {
	t.Helper()
	led := &fakeEventLogger{}
	s := New(led, nil)
	s.RegisterHandler("h", fn)
	s.LoadJobs([]config.JobConfig{{Name: "j", Handler: "h"}})
	return s, led
}

// TestTriggeredJobContextEndsWithBaseContext: cancelling the base context
// ends the running handler's ctx, and the cancelled run is still ledgered
// job.failed.
func TestTriggeredJobContextEndsWithBaseContext(t *testing.T) {
	started := make(chan struct{})
	s, led := schedulerWithJob(t, func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.SetBaseContext(base)
	if err := s.Trigger(context.Background(), "tr-base", "j"); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	<-started
	cancel()
	if !s.WaitRuns(5 * time.Second) {
		t.Fatal("the run did not end after the base context was cancelled")
	}
	row, ok := led.rowOfKind("tr-base", EventJobFailed)
	if !ok {
		t.Fatalf("no %s row for the cancelled run (kinds %v)", EventJobFailed, led.kindsFor("tr-base"))
	}
	if msg, _ := row.payload["error"].(string); msg != context.Canceled.Error() {
		t.Errorf("job.failed error = %q, want %q", msg, context.Canceled.Error())
	}
}

// TestWaitRunsJoinsRunningJobAndRefusesNewTriggers: WaitRuns waits for the
// running handler, and once it began Trigger refuses with ErrShuttingDown
// (and ledgers no job.triggered).
func TestWaitRunsJoinsRunningJobAndRefusesNewTriggers(t *testing.T) {
	release := make(chan struct{})
	finished := make(chan struct{})
	s, led := schedulerWithJob(t, func(ctx context.Context) error {
		<-release
		close(finished)
		return nil
	})
	if err := s.Trigger(context.Background(), "tr-1", "j"); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	joined := make(chan bool, 1)
	go func() { joined <- s.WaitRuns(5 * time.Second) }()
	time.Sleep(50 * time.Millisecond)
	select {
	case <-joined:
		t.Fatal("WaitRuns returned while the handler was still running")
	default:
	}
	if err := s.Trigger(context.Background(), "tr-2", "j"); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("Trigger after WaitRuns began = %v, want ErrShuttingDown", err)
	}
	if kinds := led.kindsFor("tr-2"); len(kinds) != 0 {
		t.Errorf("a refused trigger ledgered %v, want nothing", kinds)
	}
	close(release)
	if ok := <-joined; !ok {
		t.Fatal("WaitRuns reported a timeout although the handler returned")
	}
	select {
	case <-finished:
	default:
		t.Fatal("WaitRuns returned before the handler finished")
	}
}

// TestWaitRunsIsBounded: a handler that ignores its context does not hang
// shutdown forever - WaitRuns gives up after d and reports false.
func TestWaitRunsIsBounded(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	s, _ := schedulerWithJob(t, func(ctx context.Context) error {
		<-release
		return nil
	})
	if err := s.Trigger(context.Background(), "tr-hang", "j"); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	start := time.Now()
	if s.WaitRuns(100 * time.Millisecond) {
		t.Fatal("WaitRuns = true for a handler that never returned")
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("WaitRuns took %v, want ~100ms", el)
	}
}
