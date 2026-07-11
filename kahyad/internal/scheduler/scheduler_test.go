package scheduler

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kahya/kahyad/internal/config"
)

// fakeEventLogger is a minimal EventLogger stand-in so scheduler tests
// don't need a real brain.db (mirrors kahyad/internal/server's own
// fakeDB/fakeSearcher test-double convention).
type fakeEventLogger struct {
	mu   sync.Mutex
	rows []loggedEvent
}

type loggedEvent struct {
	traceID string
	kind    string
	payload map[string]any
}

func (f *fakeEventLogger) LogEvent(_ context.Context, traceID, kind string, payload map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = append(f.rows, loggedEvent{traceID: traceID, kind: kind, payload: payload})
	return nil
}

func (f *fakeEventLogger) kindsFor(traceID string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.rows {
		if r.traceID == traceID {
			out = append(out, r.kind)
		}
	}
	return out
}

func (f *fakeEventLogger) rowOfKind(traceID, kind string) (loggedEvent, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.traceID == traceID && r.kind == kind {
			return r, true
		}
	}
	return loggedEvent{}, false
}

func containsAll(got []string, want []string) bool {
	set := make(map[string]bool, len(got))
	for _, k := range got {
		set[k] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}

// waitFor polls cond every 5ms until it returns true or timeout elapses,
// failing t if it never does. Trigger dispatches asynchronously (task
// spec step 4), so tests observing its ledger side effects must poll
// rather than assert immediately after the call returns.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %s", timeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestTriggerUnknownJobReturnsErrUnknownJob(t *testing.T) {
	s := New(nil, nil)
	err := s.Trigger(context.Background(), "trace-unknown", "no-such-job")
	if !errors.Is(err, ErrUnknownJob) {
		t.Fatalf("Trigger() error = %v, want ErrUnknownJob", err)
	}
}

// TestLoadJobsSkipsUnresolvedHandler guards the fail-closed posture
// LoadJobs documents: a jobs: entry whose handler name isn't registered
// must never become triggerable (still ErrUnknownJob), not silently
// resolve to a no-op.
func TestLoadJobsSkipsUnresolvedHandler(t *testing.T) {
	s := New(nil, nil)
	s.LoadJobs([]config.JobConfig{{Name: "ghost", Handler: "does-not-exist"}})

	err := s.Trigger(context.Background(), "trace-ghost", "ghost")
	if !errors.Is(err, ErrUnknownJob) {
		t.Fatalf("Trigger() error = %v, want ErrUnknownJob for an unresolved handler", err)
	}
}

// TestTriggerSmokeJobLedgersTriggeredSmokeAndCompleted exercises the
// task spec's own built-in "smoke" handler end to end: Trigger must
// ledger job.triggered synchronously-enough to be visible right after it
// returns, then asynchronously run the handler (which itself ledgers
// job_smoke_ran) and finally ledger job.completed.
func TestTriggerSmokeJobLedgersTriggeredSmokeAndCompleted(t *testing.T) {
	log := &fakeEventLogger{}
	s := New(log, nil)
	s.LoadJobs([]config.JobConfig{{Name: "smoke", Handler: "smoke"}})

	traceID := "trace-smoke-1"
	if err := s.Trigger(context.Background(), traceID, "smoke"); err != nil {
		t.Fatalf("Trigger() error = %v", err)
	}

	// job.triggered must already be visible by the time Trigger returns
	// (task spec step 4: "append ledger job.triggered" happens before the
	// handler runs asynchronously).
	if kinds := log.kindsFor(traceID); !containsAll(kinds, []string{EventJobTriggered}) {
		t.Fatalf("job.triggered not ledgered synchronously; got kinds=%v", kinds)
	}

	waitFor(t, 2*time.Second, func() bool {
		return containsAll(log.kindsFor(traceID), []string{EventJobTriggered, "job_smoke_ran", EventJobCompleted})
	})

	row, ok := log.rowOfKind(traceID, EventJobTriggered)
	if !ok {
		t.Fatal("job.triggered row missing")
	}
	if row.payload["job_name"] != "smoke" || row.payload["trace_id"] != traceID {
		t.Errorf("job.triggered payload = %+v, want job_name=smoke trace_id=%s", row.payload, traceID)
	}
}

// TestTriggerFailingHandlerLedgersJobFailed guards the job.failed path:
// a handler returning an error must ledger job.failed (never
// job.completed), carrying the error string.
func TestTriggerFailingHandlerLedgersJobFailed(t *testing.T) {
	log := &fakeEventLogger{}
	s := New(log, nil)
	s.RegisterHandler("boom", func(context.Context) error { return errors.New("kaboom") })
	s.LoadJobs([]config.JobConfig{{Name: "boom-job", Handler: "boom"}})

	traceID := "trace-boom-1"
	if err := s.Trigger(context.Background(), traceID, "boom-job"); err != nil {
		t.Fatalf("Trigger() error = %v", err)
	}

	waitFor(t, 2*time.Second, func() bool {
		return containsAll(log.kindsFor(traceID), []string{EventJobTriggered, EventJobFailed})
	})

	if kinds := log.kindsFor(traceID); containsAll(kinds, []string{EventJobCompleted}) {
		t.Fatalf("job.completed ledgered for a failing handler; got kinds=%v", kinds)
	}
	row, ok := log.rowOfKind(traceID, EventJobFailed)
	if !ok {
		t.Fatal("job.failed row missing")
	}
	if row.payload["error"] != "kaboom" {
		t.Errorf("job.failed payload[error] = %v, want %q", row.payload["error"], "kaboom")
	}
}

// TestRegisterTickFiresRepeatedlyWithRealCron is the task spec step 7
// tick test: a 100ms tick must fire at least 3 times within 1s, using a
// real robfig/cron/v3 instance (not a fake/mocked clock) — the whole
// point of this test is proving the wrapper actually drives a real cron.
func TestRegisterTickFiresRepeatedlyWithRealCron(t *testing.T) {
	s := New(nil, nil)
	var count int32
	if err := s.RegisterTick("test-tick", "@every 100ms", func(context.Context) {
		atomic.AddInt32(&count, 1)
	}); err != nil {
		t.Fatalf("RegisterTick() error = %v", err)
	}
	s.StartTicks()
	defer s.StopTicks()

	time.Sleep(1 * time.Second)
	if got := atomic.LoadInt32(&count); got < 3 {
		t.Fatalf("tick fired %d times in 1s, want >= 3", got)
	}
}

// TestRegisterTickProvidesPerRunTraceID guards RegisterTick's documented
// contract: each firing gets its OWN freshly minted trace_id, retrievable
// from the callback's ctx via TraceIDFromContext.
func TestRegisterTickProvidesPerRunTraceID(t *testing.T) {
	s := New(nil, nil)
	var mu sync.Mutex
	seen := make(map[string]bool)
	fired := make(chan struct{}, 8)

	if err := s.RegisterTick("trace-tick", "@every 100ms", func(ctx context.Context) {
		id := TraceIDFromContext(ctx)
		mu.Lock()
		seen[id] = true
		mu.Unlock()
		select {
		case fired <- struct{}{}:
		default:
		}
	}); err != nil {
		t.Fatalf("RegisterTick() error = %v", err)
	}
	s.StartTicks()
	defer s.StopTicks()

	for i := 0; i < 3; i++ {
		select {
		case <-fired:
		case <-time.After(2 * time.Second):
			t.Fatal("tick never fired")
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 {
		t.Fatalf("expected multiple distinct per-run trace_ids, got %v", seen)
	}
	for id := range seen {
		if id == "" {
			t.Fatal("tick ran with an empty trace_id")
		}
	}
}
