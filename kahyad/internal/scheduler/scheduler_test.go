package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/logx"
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

// readJSONLLines parses every line of path as a standalone JSON object —
// the same shape kahyad/internal/logx.Logger emits — failing t on any
// line that isn't valid JSON. Used by the BLOCKER 1/2 panic-recovery
// regression tests below to assert the exact JSONL error line
// (event=job_panic / event=tick_panic) a recovered panic must produce.
func readJSONLLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line %q is not valid JSON: %v", line, err)
		}
		out = append(out, m)
	}
	return out
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

// TestTriggerPanickingHandlerRecoversAndLedgersJobFailed is the BLOCKER 1
// regression test: a Handler that panics must NOT crash the daemon (this
// test process would simply die if Trigger's goroutine let the panic
// propagate unrecovered) — instead it must ledger job.failed for that
// exact job_name+trace_id, indistinguishable from an ordinary returned
// error, and the scheduler must remain fully usable afterward (a second
// Trigger of the SAME job must still dispatch and ledger normally).
func TestTriggerPanickingHandlerRecoversAndLedgersJobFailed(t *testing.T) {
	log := &fakeEventLogger{}
	s := New(log, nil)
	s.RegisterHandler("panics", func(context.Context) error {
		panic("kaboom-panic")
	})
	s.LoadJobs([]config.JobConfig{{Name: "panic-job", Handler: "panics"}})

	traceID := "trace-panic-1"
	if err := s.Trigger(context.Background(), traceID, "panic-job"); err != nil {
		t.Fatalf("Trigger() error = %v", err)
	}

	waitFor(t, 2*time.Second, func() bool {
		return containsAll(log.kindsFor(traceID), []string{EventJobTriggered, EventJobFailed})
	})
	if kinds := log.kindsFor(traceID); containsAll(kinds, []string{EventJobCompleted}) {
		t.Fatalf("job.completed ledgered for a panicking handler; got kinds=%v", kinds)
	}

	row, ok := log.rowOfKind(traceID, EventJobFailed)
	if !ok {
		t.Fatal("job.failed row missing for a panicking handler")
	}
	errStr, _ := row.payload["error"].(string)
	if !strings.Contains(errStr, "kaboom-panic") {
		t.Errorf("job.failed payload[error] = %v, want it to mention the panic value %q", row.payload["error"], "kaboom-panic")
	}

	// The daemon (this test process) is still alive at this point by
	// definition (we're still executing). Prove the scheduler itself is
	// still fully functional — a panic in one run must never poison later
	// runs of the same (or any other) job.
	traceID2 := "trace-panic-2"
	if err := s.Trigger(context.Background(), traceID2, "panic-job"); err != nil {
		t.Fatalf("second Trigger() error = %v", err)
	}
	waitFor(t, 2*time.Second, func() bool {
		return containsAll(log.kindsFor(traceID2), []string{EventJobTriggered, EventJobFailed})
	})
}

// TestTriggerPanickingHandlerLogsJobPanicJSONL guards the JSONL half of the
// BLOCKER 1 fix: a recovered panic must produce its own event=job_panic
// line carrying job_name, trace_id, the recovered value, and a non-empty
// stack trace — using a real logx.Logger (not the fakeEventLogger ledger
// stand-in) so the JSONL output itself is verified.
func TestTriggerPanickingHandlerLogsJobPanicJSONL(t *testing.T) {
	logDir := t.TempDir()
	jsonl, err := logx.New(logDir, "boot-panic-jsonl")
	if err != nil {
		t.Fatalf("logx.New() error = %v", err)
	}
	defer jsonl.Close()

	s := New(nil, jsonl)
	s.RegisterHandler("panics", func(context.Context) error {
		panic("kaboom-jsonl")
	})
	s.LoadJobs([]config.JobConfig{{Name: "panic-jsonl-job", Handler: "panics"}})

	traceID := "trace-panic-jsonl-1"
	if err := s.Trigger(context.Background(), traceID, "panic-jsonl-job"); err != nil {
		t.Fatalf("Trigger() error = %v", err)
	}

	var lines []map[string]any
	waitFor(t, 2*time.Second, func() bool {
		lines = readJSONLLines(t, filepath.Join(logDir, "kahyad.jsonl"))
		for _, l := range lines {
			if l["event"] == "job_panic" {
				return true
			}
		}
		return false
	})

	for _, l := range lines {
		if l["event"] != "job_panic" {
			continue
		}
		if l["job_name"] != "panic-jsonl-job" {
			t.Errorf("job_panic job_name = %v, want panic-jsonl-job", l["job_name"])
		}
		if l["trace_id"] != traceID {
			t.Errorf("job_panic trace_id = %v, want %s", l["trace_id"], traceID)
		}
		panicVal, _ := l["panic"].(string)
		if !strings.Contains(panicVal, "kaboom-jsonl") {
			t.Errorf("job_panic panic = %v, want it to contain %q", l["panic"], "kaboom-jsonl")
		}
		if stack, _ := l["stack"].(string); stack == "" {
			t.Error("job_panic stack empty, want a runtime/debug.Stack() dump")
		}
		return
	}
	t.Fatal("no job_panic JSONL line found")
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

// TestRegisterTickPanicRecoversAndSubsequentTicksFire is the BLOCKER 2
// regression test: a tick fn that panics on EVERY firing must not crash
// the daemon (this test process would die on the very first panic if
// robfig/cron/v3's own per-job goroutine were left unrecovered) and must
// not stop the cron scheduler either — subsequent ticks must keep firing,
// using a real cron.Cron + a short interval (task spec: "assert via a
// real cron instance + a short interval"), the same posture as
// TestRegisterTickFiresRepeatedlyWithRealCron above.
func TestRegisterTickPanicRecoversAndSubsequentTicksFire(t *testing.T) {
	s := New(nil, nil)
	var count int32
	if err := s.RegisterTick("panic-tick", "@every 100ms", func(context.Context) {
		atomic.AddInt32(&count, 1)
		panic("tick-boom")
	}); err != nil {
		t.Fatalf("RegisterTick() error = %v", err)
	}
	s.StartTicks()
	defer s.StopTicks()

	time.Sleep(1 * time.Second)
	if got := atomic.LoadInt32(&count); got < 3 {
		t.Fatalf("panicking tick fired %d times in 1s, want >= 3 (daemon must survive each panic and keep firing)", got)
	}
}

// TestRegisterTickPanicLogsTickPanicJSONL guards the JSONL half of the
// BLOCKER 2 fix: a recovered tick panic must produce its own
// event=tick_panic line carrying name, trace_id, the recovered value, and
// a non-empty stack trace.
func TestRegisterTickPanicLogsTickPanicJSONL(t *testing.T) {
	logDir := t.TempDir()
	jsonl, err := logx.New(logDir, "boot-tick-panic-jsonl")
	if err != nil {
		t.Fatalf("logx.New() error = %v", err)
	}
	defer jsonl.Close()

	s := New(nil, jsonl)
	if err := s.RegisterTick("panic-tick-jsonl", "@every 100ms", func(context.Context) {
		panic("tick-boom-jsonl")
	}); err != nil {
		t.Fatalf("RegisterTick() error = %v", err)
	}
	s.StartTicks()
	defer s.StopTicks()

	var lines []map[string]any
	waitFor(t, 2*time.Second, func() bool {
		lines = readJSONLLines(t, filepath.Join(logDir, "kahyad.jsonl"))
		for _, l := range lines {
			if l["event"] == "tick_panic" {
				return true
			}
		}
		return false
	})

	for _, l := range lines {
		if l["event"] != "tick_panic" {
			continue
		}
		if l["name"] != "panic-tick-jsonl" {
			t.Errorf("tick_panic name = %v, want panic-tick-jsonl", l["name"])
		}
		if id, _ := l["trace_id"].(string); id == "" {
			t.Error("tick_panic trace_id empty")
		}
		panicVal, _ := l["panic"].(string)
		if !strings.Contains(panicVal, "tick-boom-jsonl") {
			t.Errorf("tick_panic panic = %v, want it to contain %q", l["panic"], "tick-boom-jsonl")
		}
		if stack, _ := l["stack"].(string); stack == "" {
			t.Error("tick_panic stack empty, want a runtime/debug.Stack() dump")
		}
		return
	}
	t.Fatal("no tick_panic JSONL line found")
}
