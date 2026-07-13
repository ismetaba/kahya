package briefing

import (
	"context"
	"errors"
	"testing"
	"time"

	"kahya/kahyad/internal/logx"
)

// TestGateItemKeepsCleanText proves an item that matches no path glob and
// classifies non-secret is kept verbatim.
func TestGateItemKeepsCleanText(t *testing.T) {
	c := &fakeClassifier{}
	item := CollectedItem{Section: "gh_pr", Text: "kahya/gold-token#12: bump deps"}

	out := gateItem(context.Background(), c, nil, item)
	if out.Dropped {
		t.Fatalf("Dropped = true, want false (clean item)")
	}
	if out.Line != item.Text {
		t.Errorf("Line = %q, want %q", out.Line, item.Text)
	}
}

// TestGateItemDropsOnPathGlobBeforeEverClassifying is the ordering-
// invariant's file-path-glob half: a file item whose path matches
// policy.yaml's secret_lane_globs is dropped WITHOUT ever reaching the
// classifier - checked here by asserting the fake classifier's Calls slice
// stays empty.
func TestGateItemDropsOnPathGlobBeforeEverClassifying(t *testing.T) {
	c := &fakeClassifier{}
	globs := fakeGlobMatcher{Paths: map[string]bool{"/Users/x/Documents/saglik/notes.md": true}}
	item := CollectedItem{Section: "file", Text: "notes.md (2026-07-12T08:00:00Z)", Path: "/Users/x/Documents/saglik/notes.md"}

	out := gateItem(context.Background(), c, globs, item)
	if !out.Dropped {
		t.Fatal("Dropped = false, want true (path matched a secret-lane glob)")
	}
	if out.DropReason != DropReasonPathGlob {
		t.Errorf("DropReason = %q, want %q", out.DropReason, DropReasonPathGlob)
	}
	if out.Line != PlaceholderSecretLane {
		t.Errorf("Line = %q, want the byte-exact placeholder", out.Line)
	}
	if len(c.Calls) != 0 {
		t.Errorf("classifier.Calls = %v, want empty (glob check must short-circuit before content classification)", c.Calls)
	}
}

// TestGateItemDropsOnSecretLaneVerdict proves a positive content-
// classification verdict drops the item and substitutes the placeholder.
func TestGateItemDropsOnSecretLaneVerdict(t *testing.T) {
	c := &fakeClassifier{Marks: []string{"IBAN"}}
	item := CollectedItem{Section: "gh_pr", Text: "kahya/x#3: rotate IBAN in docs"}

	out := gateItem(context.Background(), c, nil, item)
	if !out.Dropped {
		t.Fatal("Dropped = false, want true (secret-lane verdict)")
	}
	if out.DropReason != DropReasonSecretLane {
		t.Errorf("DropReason = %q, want %q", out.DropReason, DropReasonSecretLane)
	}
	if out.Line != PlaceholderSecretLane {
		t.Errorf("Line = %q, want the byte-exact placeholder", out.Line)
	}
}

// TestGateItemFailClosedOnClassifierError is the FAIL-CLOSED regression
// test: a classifier error (model/memory unavailable) drops the item
// exactly like a positive secret-lane verdict - never lets unclassified
// bytes through.
func TestGateItemFailClosedOnClassifierError(t *testing.T) {
	c := &fakeClassifier{Err: errors.New("qwen unavailable (simulated)")}
	item := CollectedItem{Section: "gh_pr", Text: "kahya/x#4: totally ordinary PR title"}

	out := gateItem(context.Background(), c, nil, item)
	if !out.Dropped {
		t.Fatal("Dropped = false, want true (classifier error must fail closed)")
	}
	if out.DropReason != DropReasonClassifyFailed {
		t.Errorf("DropReason = %q, want %q", out.DropReason, DropReasonClassifyFailed)
	}
	if out.Line != PlaceholderSecretLane {
		t.Errorf("Line = %q, want the byte-exact placeholder", out.Line)
	}
}

// TestGateItemFailClosedOnNilClassifier proves a completely unwired
// classifier ALSO fails closed - "cannot verify" must never be treated as
// "assume safe".
func TestGateItemFailClosedOnNilClassifier(t *testing.T) {
	item := CollectedItem{Section: "gh_pr", Text: "anything"}
	out := gateItem(context.Background(), nil, nil, item)
	if !out.Dropped || out.DropReason != DropReasonClassifierUnavailable {
		t.Fatalf("gateItem(nil classifier) = %+v, want Dropped=true reason=%q", out, DropReasonClassifierUnavailable)
	}
}

// TestW5GateSingleNotificationTraceIDThenDuplicateSkipped is W5-05's own
// phase-gate test (HANDOFF §6 W5 acceptance, verbatim): "08:30 brifingi
// tek bildirim + trace_id". It composes two invariants this package's own
// W5-01 tests already prove individually
// (TestRunEmitsCollectorWorkerAndDeliveryJSONLLinesUnderOneTraceID in
// jsonl_test.go; TestRunOncePerDaySecondRunSkipsDuplicate in
// orchestrator_test.go) into the single, canonical assertion the W5
// acceptance gate names: a real Orchestrator.Run delivers EXACTLY ONE
// Telegram notification, every JSONL line the run emits (collector,
// worker, delivery) shares that one run's trace_id, and re-running on the
// same calendar date sends NOTHING further and ledgers
// briefing.skipped_duplicate instead.
func TestW5GateSingleNotificationTraceIDThenDuplicateSkipped(t *testing.T) {
	st := newTestStore(t)
	dedupe := StoreDedupeChecker{Store: st.Queries}
	logDir := t.TempDir()
	log, err := logx.New(logDir, "boot-w5-gate")
	if err != nil {
		t.Fatalf("logx.New: %v", err)
	}
	t.Cleanup(func() { log.Close() })

	classifier := permissiveClassifier()
	spawner := &fakeWorkerSpawner{RawJSON: `{"lines":["ozet"]}`}
	delivery := &fakeDelivery{Sent: true}

	realToday := time.Now().UTC()
	now := fixedNow(time.Date(realToday.Year(), realToday.Month(), realToday.Day(), 8, 30, 0, 0, time.UTC))

	o := &Orchestrator{
		Classifier: classifier,
		GH:         GHCollector{Runner: &fakeGHRunner{PRJSON: []byte(`[{"number":1,"title":"bump deps"}]`)}, Repos: []string{"kahya/x"}},
		Spawner:    spawner,
		Delivery:   delivery,
		Ledger:     st,
		Dedupe:     dedupe,
		Log:        log,
		Now:        now,
	}

	const traceID = "trace-w5-gate-briefing"
	first, err := o.Run(context.Background(), traceID)
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if !first.Delivered || first.SkippedDuplicate {
		t.Fatalf("first Run result = %+v, want Delivered=true SkippedDuplicate=false", first)
	}
	if len(delivery.Calls) != 1 {
		t.Fatalf("Telegram sends after first Run = %d, want exactly 1 (single notification)", len(delivery.Calls))
	}

	// Every JSONL line this run emitted - collector, worker, delivery -
	// must share the SAME trace_id (`kahya log --trace <id>` reads exactly
	// these files; see jsonl_test.go's readJSONLLinesForTrace).
	lines := readJSONLLinesForTrace(t, logDir, traceID)
	var sawCollector, sawWorker, sawDelivery bool
	for _, l := range lines {
		switch l["event"] {
		case "briefing_collected":
			sawCollector = true
		case "briefing_worker_spawn", "briefing_worker_done":
			sawWorker = true
		case "briefing_delivered":
			sawDelivery = true
		}
		if l["trace_id"] != traceID {
			t.Errorf("line %+v has trace_id %v, want %q", l, l["trace_id"], traceID)
		}
	}
	if !sawCollector || !sawWorker || !sawDelivery {
		t.Fatalf("missing expected JSONL line(s): collector=%v worker=%v delivery=%v", sawCollector, sawWorker, sawDelivery)
	}

	// Re-run on the SAME calendar date: nothing further is sent, and
	// briefing.skipped_duplicate is ledgered.
	second, err := o.Run(context.Background(), "trace-w5-gate-briefing-2")
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if second.Delivered || !second.SkippedDuplicate {
		t.Fatalf("second Run result = %+v, want Delivered=false SkippedDuplicate=true", second)
	}
	if len(delivery.Calls) != 1 {
		t.Fatalf("Telegram sends after second (same-day) Run = %d, want still exactly 1", len(delivery.Calls))
	}
	var skipped int
	if err := st.DB().QueryRow(`SELECT count(*) FROM events WHERE kind = ?`, EventSkippedDuplicate).Scan(&skipped); err != nil {
		t.Fatal(err)
	}
	if skipped != 1 {
		t.Errorf("%s events = %d, want exactly 1", EventSkippedDuplicate, skipped)
	}
}
