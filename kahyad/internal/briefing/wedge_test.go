package briefing

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/scheduler"
	"kahya/kahyad/internal/store/sqlcgen"
)

// syncLedger is a goroutine-safe Ledger/scheduler.EventLogger double: the
// scheduler writes job rows from its own goroutine.
type syncLedger struct {
	mu     sync.Mutex
	events []fakeEvent
}

func (l *syncLedger) LogEvent(_ context.Context, traceID, kind string, payload map[string]any) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, fakeEvent{traceID: traceID, kind: kind, payload: payload})
	return nil
}

func (l *syncLedger) count(kind string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, e := range l.events {
		if e.kind == kind {
			n++
		}
	}
	return n
}

// TestHangingGHNeverHoldsSchedulerJobLock (FIX-P-8): through the real
// scheduler, a gh that never answers no longer holds the morning-briefing
// job's per-name lock: both triggered runs finish (job_completed), each
// delivers and ledgers briefing.gh_unavailable. Before the fix the first
// run never returned and the second never started.
func TestHangingGHNeverHoldsSchedulerJobLock(t *testing.T) {
	ledger := &syncLedger{}
	var deliveries sync.Mutex
	delivered := 0
	o := &Orchestrator{
		Classifier: permissiveClassifier(),
		GH:         GHCollector{Runner: alwaysHangingGHRunner{}, Repos: []string{"kahya/x"}, CallTimeout: 100 * time.Millisecond},
		GHBudget:   500 * time.Millisecond,
		Spawner:    &fakeWorkerSpawner{RawJSON: `{"lines":["ozet"]}`},
		Delivery: deliveryFunc(func(context.Context, string, string) bool {
			deliveries.Lock()
			delivered++
			deliveries.Unlock()
			return true
		}),
		Ledger: ledger,
		Now:    time.Now,
	}
	sched := scheduler.New(ledger, nil)
	sched.RegisterHandler("morning-briefing", func(ctx context.Context) error {
		_, err := o.Run(ctx, scheduler.TraceIDFromContext(ctx))
		return err
	})
	sched.LoadJobs([]config.JobConfig{{Name: "morning-briefing", Handler: "morning-briefing"}})
	for _, traceID := range []string{"trace-p8-sched-1", "trace-p8-sched-2"} {
		if err := sched.Trigger(context.Background(), traceID, "morning-briefing"); err != nil {
			t.Fatalf("Trigger: %v", err)
		}
	}

	deadline := time.Now().Add(10 * time.Second)
	for ledger.count(scheduler.EventJobCompleted)+ledger.count(scheduler.EventJobFailed) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of 2 morning-briefing runs finished within 10s - a hung gh still holds the job lock",
				ledger.count(scheduler.EventJobCompleted)+ledger.count(scheduler.EventJobFailed))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := ledger.count(scheduler.EventJobCompleted); n != 2 {
		t.Errorf("job_completed rows = %d, want 2", n)
	}
	if n := ledger.count(EventGHUnavailable); n != 2 {
		t.Errorf("%s rows = %d, want 2 (one per run)", EventGHUnavailable, n)
	}
	deliveries.Lock()
	defer deliveries.Unlock()
	if delivered != 2 {
		t.Errorf("deliveries = %d, want 2", delivered)
	}
}

// deliveryFunc adapts a func to Delivery.
type deliveryFunc func(ctx context.Context, traceID, text string) bool

func (f deliveryFunc) SendNotification(ctx context.Context, traceID, text string) bool {
	return f(ctx, traceID, text)
}

type failingTaint struct{}

func (failingTaint) InsertUntrusted(context.Context, string, string, string) error {
	return errors.New("taint store down")
}

type failingTaskStore struct{}

func (failingTaskStore) InsertTask(context.Context, sqlcgen.InsertTaskParams) (sqlcgen.Task, error) {
	return sqlcgen.Task{}, errors.New("tasks table locked")
}

func (failingTaskStore) UpdateTaskState(context.Context, sqlcgen.UpdateTaskStateParams) error {
	return nil
}

func (failingTaskStore) GetTaskByID(context.Context, string) (sqlcgen.Task, error) {
	return sqlcgen.Task{}, errors.New("not found")
}

// TestRunStartedRunAlwaysEndsWithTerminalLedgerRow (FIX-P-8): a run that
// ledgered briefing.started and then fails before delivering ends with a
// briefing.failed row naming why - the P-8 finding's "no briefing.failed
// event" gap. The delivery case is the realistic stalled-egress picture: gh
// timed out (briefing.gh_unavailable), then the Telegram send failed too.
func TestRunStartedRunAlwaysEndsWithTerminalLedgerRow(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mutate     func(o *Orchestrator)
		wantReason string
		wantGH     int
	}{
		{"delivery_failed_after_gh_timeout", func(o *Orchestrator) {
			o.GH = GHCollector{Runner: alwaysHangingGHRunner{}, Repos: []string{"kahya/x"}, CallTimeout: 50 * time.Millisecond}
			o.Delivery = &fakeDelivery{Sent: false}
		}, "delivery_failed", 1},
		{"taint_failed", func(o *Orchestrator) { o.Taint = failingTaint{} }, "taint_failed", 0},
		{"task_row_failed", func(o *Orchestrator) { o.TaskStore = failingTaskStore{} }, "task_row_failed", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logDir := t.TempDir()
			log, err := logx.New(logDir, "boot-p8-terminal")
			if err != nil {
				t.Fatalf("logx.New: %v", err)
			}
			t.Cleanup(func() { log.Close() })
			ledger := &fakeLedger{}
			o := &Orchestrator{
				Classifier: permissiveClassifier(),
				Spawner:    &fakeWorkerSpawner{RawJSON: `{"lines":["ozet"]}`},
				Delivery:   &fakeDelivery{Sent: true},
				Ledger:     ledger,
				Log:        log,
				Now:        time.Now,
			}
			tc.mutate(o)
			traceID := "trace-p8-" + tc.name
			if _, err := o.Run(context.Background(), traceID); err == nil {
				t.Fatal("Run error = nil, want an error")
			}
			if ledger.count(EventStarted) != 1 || ledger.count(EventDelivered) != 0 {
				t.Fatalf("started/delivered = %d/%d, want 1/0", ledger.count(EventStarted), ledger.count(EventDelivered))
			}
			var reasons []any
			for _, e := range ledger.events {
				if e.kind == EventFailed {
					reasons = append(reasons, e.payload["reason"])
				}
			}
			if len(reasons) != 1 || reasons[0] != tc.wantReason {
				t.Fatalf("%s reasons = %v, want [%s]", EventFailed, reasons, tc.wantReason)
			}
			if n := ledger.count(EventGHUnavailable); n != tc.wantGH {
				t.Errorf("%s rows = %d, want %d", EventGHUnavailable, n, tc.wantGH)
			}
			if tc.wantGH > 0 {
				var sawGHLine bool
				for _, l := range readJSONLLinesForTrace(t, logDir, traceID) {
					if l["event"] == "briefing_gh_collect_error" && strings.Contains(l["err"].(string), "deadline exceeded") {
						sawGHLine = true
					}
				}
				if !sawGHLine {
					t.Error("no briefing_gh_collect_error JSONL line carrying the gh deadline error")
				}
			}
		})
	}
}
