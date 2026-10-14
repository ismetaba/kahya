package briefing

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/secretlane"
)

// fakeWarmer is a Warmer double (FIX-P-4): counts EnsureRunning calls,
// optionally fails (err, or errOnCall for one specific call number), runs
// onWarm after a successful warm-up, and - when hang is set - blocks until
// the caller's ctx ends (a cold load that never becomes healthy).
type fakeWarmer struct {
	mu        sync.Mutex
	calls     int
	err       error
	errOnCall int
	hang      bool
	onWarm    func()
}

func (w *fakeWarmer) EnsureRunning(ctx context.Context) error {
	w.mu.Lock()
	w.calls++
	n := w.calls
	w.mu.Unlock()
	if w.hang {
		<-ctx.Done()
		return ctx.Err()
	}
	if w.err != nil {
		return w.err
	}
	if w.errOnCall != 0 && n == w.errOnCall {
		return errors.New("yerel model için bellek yok")
	}
	if w.onWarm != nil {
		w.onWarm()
	}
	return nil
}

func (w *fakeWarmer) callCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls
}

// coldThenWarmClassifier models the local Qwen as the production
// classifier sees it: until warmed is set it behaves like a cold server
// (blocks up to 1500ms, the way mlxsup.EnsureRunning polls /v1/models) and
// flags the call as a bug - a classify call before the warm-up; afterwards
// it answers a clean verdict at once.
type coldThenWarmClassifier struct {
	t      *testing.T
	warmed atomic.Bool
	calls  atomic.Int32
}

func (c *coldThenWarmClassifier) Classify(ctx context.Context, _ string) (secretlane.Verdict, error) {
	c.calls.Add(1)
	if !c.warmed.Load() {
		c.t.Errorf("Classify called before the Warmer brought the model up")
		select {
		case <-time.After(1500 * time.Millisecond):
		case <-ctx.Done():
			return secretlane.Verdict{SecretLane: true, Category: secretlane.CategoryUnknown}, ctx.Err()
		}
	}
	return secretlane.Verdict{SecretLane: false, Category: secretlane.CategoryNone}, nil
}

func dropReasons(l *fakeLedger) []any {
	var out []any
	for _, e := range l.events {
		if e.kind == EventItemDropped {
			out = append(out, e.payload["reason"])
		}
	}
	return out
}

const threePRs = `[{"number":1,"title":"first pr"},{"number":2,"title":"second pr"},{"number":3,"title":"third pr"}]`

// TestRunWarmsClassifierOnceBeforeGateLoop (FIX-P-4): Run brings the local
// classifier up BEFORE the first gate call, so a model that is cold at
// 08:30 classifies every item instead of dropping them fail-closed one
// after another under a warm-sized budget.
func TestRunWarmsClassifierOnceBeforeGateLoop(t *testing.T) {
	classifier := &coldThenWarmClassifier{t: t}
	warmer := &fakeWarmer{onWarm: func() { classifier.warmed.Store(true) }}
	spawner := &fakeWorkerSpawner{RawJSON: `{"lines":["ozet"]}`}
	delivery := &fakeDelivery{Sent: true}
	ledger := &fakeLedger{}

	o := &Orchestrator{
		Classifier: classifier,
		Warmer:     warmer,
		GH:         GHCollector{Runner: &fakeGHRunner{PRJSON: []byte(threePRs)}, Repos: []string{"kahya/x"}},
		Spawner:    spawner,
		Delivery:   delivery,
		Ledger:     ledger,
		Now:        time.Now,
	}
	result, err := o.Run(context.Background(), "trace-p4-warm")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Delivered || result.ClassifierUnavailable {
		t.Fatalf("result = %+v, want Delivered=true ClassifierUnavailable=false", result)
	}
	if warmer.callCount() < 1 {
		t.Fatal("Warmer.EnsureRunning was never called")
	}
	if n := classifier.calls.Load(); n < 3 {
		t.Fatalf("classifier calls = %d, want >= 3 (every item gated)", n)
	}
	if n := ledger.count(EventItemDropped); n != 0 {
		t.Fatalf("%s events = %d, want 0 (reasons %v)", EventItemDropped, n, dropReasons(ledger))
	}
	if len(spawner.Envs) != 1 {
		t.Fatalf("spawns = %d, want 1", len(spawner.Envs))
	}
	for _, title := range []string{"first pr", "second pr", "third pr"} {
		if !strings.Contains(spawner.Envs[0].Prompt, title) {
			t.Errorf("worker prompt lacks %q:\n%s", title, spawner.Envs[0].Prompt)
		}
	}
	if len(delivery.Calls) != 1 || strings.Contains(delivery.Calls[0], PlaceholderSecretLane) {
		t.Fatalf("delivery = %q, want one briefing without the placeholder", delivery.Calls)
	}
}

// TestRunClassifierWarmupFailureDeliversNoticeNotEmptyBriefing (FIX-P-4,
// owner decision G08-2): when the local classifier cannot be brought up
// (memory pressure), no item is classified, so NOTHING goes to the cloud:
// no worker spawn, one Turkish notice (title + trace_id +
// MsgClassifierUnavailable, plus the calendar line), the ledger carries
// briefing.classifier_unavailable + briefing.failed and NO
// briefing.delivered, and Run returns an error - all without waiting out
// a per-item budget on a classifier that is known to be down.
func TestRunClassifierWarmupFailureDeliversNoticeNotEmptyBriefing(t *testing.T) {
	spawner := &fakeWorkerSpawner{RawJSON: `{"lines":["ozet"]}`}
	delivery := &fakeDelivery{Sent: true}
	ledger := &fakeLedger{}
	o := &Orchestrator{
		Classifier: hangingClassifier{},
		Warmer:     &fakeWarmer{err: errors.New("yerel model için bellek yok")},
		GH:         GHCollector{Runner: &fakeGHRunner{PRJSON: []byte(`[{"number":1,"title":"first pr"},{"number":2,"title":"second pr"}]`)}, Repos: []string{"kahya/x"}},
		Calendar:   fakeCalendarRunner{Err: ErrCalendarNoAccess},
		Spawner:    spawner,
		Delivery:   delivery,
		Ledger:     ledger,
		Now:        time.Now,
	}

	start := time.Now()
	result, err := o.Run(context.Background(), "trace-p4-unavailable")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Run error = nil, want an error (the scheduler must ledger job_failed)")
	}
	if !result.ClassifierUnavailable || result.Delivered {
		t.Fatalf("result = %+v, want ClassifierUnavailable=true Delivered=false", result)
	}
	if elapsed >= 2*time.Second {
		t.Errorf("Run took %v, want < 2s (no per-item wait on a classifier known to be down)", elapsed)
	}
	if len(spawner.Envs) != 0 {
		t.Fatalf("worker spawned %d time(s) - nothing was classified, so nothing may reach the cloud", len(spawner.Envs))
	}
	if len(delivery.Calls) != 1 {
		t.Fatalf("delivery calls = %d, want exactly 1 notice", len(delivery.Calls))
	}
	text := delivery.Calls[0]
	for _, want := range []string{TelegramTitle, "trace_id: trace-p4-unavailable", MsgClassifierUnavailable, MsgCalendarNoAccess} {
		if !strings.Contains(text, want) {
			t.Errorf("notice lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, PlaceholderSecretLane) {
		t.Errorf("notice carries the per-item placeholder; want only the classifier notice:\n%s", text)
	}
	if n := ledger.count(EventClassifierUnavailable); n != 1 {
		t.Errorf("%s events = %d, want 1", EventClassifierUnavailable, n)
	}
	if n := ledger.count(EventFailed); n != 1 {
		t.Errorf("%s events = %d, want 1", EventFailed, n)
	}
	if n := ledger.count(EventDelivered); n != 0 {
		t.Errorf("%s events = %d, want 0 (a later run must still deliver today)", EventDelivered, n)
	}
	reasons := dropReasons(ledger)
	if len(reasons) != 2 {
		t.Fatalf("%s events = %v, want 2", EventItemDropped, reasons)
	}
	for _, r := range reasons {
		if r != DropReasonClassifierUnavailable {
			t.Errorf("drop reason = %v, want %q", r, DropReasonClassifierUnavailable)
		}
	}
	for _, e := range ledger.events {
		if e.kind == EventFailed && e.payload["reason"] != "classifier_unavailable" {
			t.Errorf("%s payload = %v, want reason=classifier_unavailable", EventFailed, e.payload)
		}
	}
}

// TestRunHangingWarmupIsBoundedAndFailsClosed (FIX-P-4): a warm-up that
// never becomes healthy is cut by the warm-up budget and handled exactly
// like a failed one - the run returns, nothing reaches the cloud.
func TestRunHangingWarmupIsBoundedAndFailsClosed(t *testing.T) {
	spawner := &fakeWorkerSpawner{RawJSON: `{"lines":["ozet"]}`}
	delivery := &fakeDelivery{Sent: true}
	o := &Orchestrator{
		Classifier:   permissiveClassifier(),
		Warmer:       &fakeWarmer{hang: true},
		WarmupBudget: 100 * time.Millisecond,
		GH:           GHCollector{Runner: &fakeGHRunner{PRJSON: []byte(`[{"number":1,"title":"first pr"}]`)}, Repos: []string{"kahya/x"}},
		Spawner:      spawner,
		Delivery:     delivery,
		Now:          time.Now,
	}
	done := make(chan Result, 1)
	go func() {
		r, _ := o.Run(context.Background(), "trace-p4-hang")
		done <- r
	}()
	select {
	case r := <-done:
		if !r.ClassifierUnavailable || len(spawner.Envs) != 0 {
			t.Fatalf("result = %+v spawns = %d, want ClassifierUnavailable and no spawn", r, len(spawner.Envs))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a hanging warm-up wedged the briefing - the warm-up budget did not fire")
	}
}

// TestRunRewarmsBeforeDeliveryRedaction (FIX-P-4): the delivery-time
// redaction backstop re-warms first (the idle TTL may have unloaded the
// model during the cloud call); JSONL records one briefing_classifier_warm
// line per phase and a briefing_gated line carrying max_classify_ms, all
// under the run's trace_id.
func TestRunRewarmsBeforeDeliveryRedaction(t *testing.T) {
	logDir := t.TempDir()
	log, err := logx.New(logDir, "boot-p4-rewarm")
	if err != nil {
		t.Fatalf("logx.New: %v", err)
	}
	t.Cleanup(func() { log.Close() })

	warmer := &fakeWarmer{}
	o := &Orchestrator{
		Classifier: permissiveClassifier(),
		Warmer:     warmer,
		GH:         GHCollector{Runner: &fakeGHRunner{PRJSON: []byte(`[{"number":1,"title":"bump deps"}]`)}, Repos: []string{"kahya/x"}},
		Spawner:    &fakeWorkerSpawner{RawJSON: `{"lines":["ozet"]}`},
		Delivery:   &fakeDelivery{Sent: true},
		Log:        log,
		Now:        time.Now,
	}
	const traceID = "trace-p4-rewarm"
	if _, err := o.Run(context.Background(), traceID); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := warmer.callCount(); n != 2 {
		t.Fatalf("EnsureRunning calls = %d, want 2 (gate + redaction)", n)
	}
	phases := map[any]int{}
	var gated map[string]any
	for _, l := range readJSONLLinesForTrace(t, logDir, traceID) {
		switch l["event"] {
		case "briefing_classifier_warm":
			phases[l["phase"]]++
		case "briefing_gated":
			gated = l
		}
	}
	if phases["gate"] != 1 || phases["redaction"] != 1 || len(phases) != 2 {
		t.Errorf("briefing_classifier_warm phases = %v, want gate:1 redaction:1", phases)
	}
	if gated == nil {
		t.Fatal("no briefing_gated JSONL line")
	}
	if _, ok := gated["max_classify_ms"]; !ok || gated["items"] != float64(1) || gated["dropped"] != float64(0) {
		t.Errorf("briefing_gated = %v, want items=1 dropped=0 and a max_classify_ms field", gated)
	}
}

// TestRunRedactionWarmupFailureWithholdsSummary (FIX-P-4): if the model
// cannot be re-warmed for the redaction backstop, the worker's summary is
// withheld (never delivered unredacted): one classifier notice, no
// briefing.delivered, and the result still names the run's task/session.
func TestRunRedactionWarmupFailureWithholdsSummary(t *testing.T) {
	const summaryLine = "bugun uc PR var"
	delivery := &fakeDelivery{Sent: true}
	ledger := &fakeLedger{}
	o := &Orchestrator{
		Classifier: permissiveClassifier(),
		Warmer:     &fakeWarmer{errOnCall: 2},
		GH:         GHCollector{Runner: &fakeGHRunner{PRJSON: []byte(`[{"number":1,"title":"bump deps"}]`)}, Repos: []string{"kahya/x"}},
		Spawner:    &fakeWorkerSpawner{RawJSON: `{"lines":["` + summaryLine + `"]}`},
		Delivery:   delivery,
		Ledger:     ledger,
		Now:        time.Now,
	}
	result, err := o.Run(context.Background(), "trace-p4-redact-warm")
	if err == nil {
		t.Fatal("Run error = nil, want an error")
	}
	if !result.ClassifierUnavailable || result.Delivered || result.TaskID == "" || result.SessionID == "" {
		t.Fatalf("result = %+v, want ClassifierUnavailable with the run's TaskID/SessionID", result)
	}
	if len(delivery.Calls) != 1 || strings.Contains(delivery.Calls[0], summaryLine) || !strings.Contains(delivery.Calls[0], MsgClassifierUnavailable) {
		t.Fatalf("delivery = %q, want one classifier notice and no summary line", delivery.Calls)
	}
	if ledger.count(EventDelivered) != 0 || ledger.count(EventFailed) != 1 || ledger.count(EventClassifierUnavailable) != 1 {
		t.Errorf("ledger delivered/failed/classifier_unavailable = %d/%d/%d, want 0/1/1",
			ledger.count(EventDelivered), ledger.count(EventFailed), ledger.count(EventClassifierUnavailable))
	}
}

// TestRunClassifierUnavailableDoesNotWriteDeliveredSoRetryStillDelivers
// (FIX-P-4, owner decision G08-2) over a real brain.db: a run whose
// warm-up fails writes no task row and no briefing.delivered, so the
// once-per-day dedupe still lets a later run (memory freed) deliver the
// real briefing the same day. Now is the real clock: Store.LogEvent stamps
// created_at from the wall clock, and the dedupe compares against it.
func TestRunClassifierUnavailableDoesNotWriteDeliveredSoRetryStillDelivers(t *testing.T) {
	st := newTestStore(t)
	spawner := &fakeWorkerSpawner{RawJSON: `{"lines":["ozet"]}`}
	delivery := &fakeDelivery{Sent: true}
	o := &Orchestrator{
		Classifier: permissiveClassifier(),
		Warmer:     &fakeWarmer{err: errors.New("yerel model için bellek yok")},
		GH:         GHCollector{Runner: &fakeGHRunner{PRJSON: []byte(`[{"number":1,"title":"bump deps"}]`)}, Repos: []string{"kahya/x"}},
		Spawner:    spawner,
		Delivery:   delivery,
		TaskStore:  st.Queries,
		Ledger:     st,
		Dedupe:     StoreDedupeChecker{Store: st.Queries},
		Now:        time.Now,
	}

	first, err := o.Run(context.Background(), "trace-p4-retry-1")
	if err == nil || !first.ClassifierUnavailable {
		t.Fatalf("first Run = %+v, %v; want ClassifierUnavailable and an error", first, err)
	}
	var tasks int
	if err := st.DB().QueryRow(`SELECT count(*) FROM tasks`).Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if tasks != 0 || len(spawner.Envs) != 0 {
		t.Fatalf("after the failed warm-up: tasks rows = %d, spawns = %d; want 0/0", tasks, len(spawner.Envs))
	}

	o.Warmer = &fakeWarmer{}
	second, err := o.Run(context.Background(), "trace-p4-retry-2")
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if !second.Delivered || second.SkippedDuplicate {
		t.Fatalf("second Run = %+v, want Delivered=true SkippedDuplicate=false", second)
	}

	count := func(kind string) int {
		var n int
		if err := st.DB().QueryRow(`SELECT count(*) FROM events WHERE kind = ?`, kind).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(EventDelivered); n != 1 {
		t.Errorf("%s rows = %d, want 1", EventDelivered, n)
	}
	if n := count(EventFailed); n != 1 {
		t.Errorf("%s rows = %d, want 1", EventFailed, n)
	}
	if n := count(EventClassifierUnavailable); n != 1 {
		t.Errorf("%s rows = %d, want 1", EventClassifierUnavailable, n)
	}
	if len(delivery.Calls) != 2 {
		t.Errorf("delivery calls = %d, want 2 (one notice, one briefing)", len(delivery.Calls))
	}
}
