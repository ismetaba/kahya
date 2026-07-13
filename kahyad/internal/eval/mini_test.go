package eval

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// fakeEventStore is a trivial in-memory EventLogger+EventReader - mirrors
// kahyad/internal/consolidation's own fakeEventStore test double (same
// package-local EventRow shape, no brain.db anywhere in this file).
type fakeEventStore struct {
	rows []EventRow
	next int64
}

func (f *fakeEventStore) LogEvent(ctx context.Context, traceID, kind string, payload map[string]any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	f.next++
	f.rows = append(f.rows, EventRow{ID: f.next, Payload: string(b)})
	return nil
}

func (f *fakeEventStore) ListEventsByKind(ctx context.Context, kind string) ([]EventRow, error) {
	// This fake only ever stores ONE kind (EventMiniRun), matching every
	// test below - a real StoreEventReader filters by kind itself.
	out := make([]EventRow, len(f.rows))
	copy(out, f.rows)
	return out, nil
}

// fakeSearcher answers Search from a fixed map[query][]Hit - queries not
// present in the map return an empty (abstention) result, never an error.
type fakeSearcher struct {
	byQuery map[string][]Hit
	err     error
}

func (f *fakeSearcher) Search(ctx context.Context, traceID, query string, k int) ([]Hit, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.byQuery[query], nil
}

// --- ParseBaseline ---

func TestParseBaselineHappyPath(t *testing.T) {
	in := "{\"q\": \"evlerimizden\", \"expect_substring\": \"ev\", \"k\": 5}\n" +
		"\n" + // blank line must be skipped
		"{\"q\": \"soru2\", \"expect_substring\": \"cevap2\", \"k\": 3}\n"
	qs, err := ParseBaseline(strings.NewReader(in))
	if err != nil {
		t.Fatalf("ParseBaseline: %v", err)
	}
	if len(qs) != 2 {
		t.Fatalf("len(qs) = %d, want 2 (blank line must be skipped)", len(qs))
	}
	if qs[0] != (Question{Q: "evlerimizden", ExpectSubstring: "ev", K: 5}) {
		t.Errorf("qs[0] = %+v, want the byte-exact morphology probe", qs[0])
	}
	if qs[1].Q != "soru2" || qs[1].K != 3 {
		t.Errorf("qs[1] = %+v", qs[1])
	}
}

func TestParseBaselineMalformedLineErrors(t *testing.T) {
	_, err := ParseBaseline(strings.NewReader("not json at all\n"))
	if err == nil {
		t.Fatal("ParseBaseline(malformed line) = nil error, want an error (never silently drop a question)")
	}
}

// TestMiniBaselineFileShape is the acceptance-criterion regression test for
// eval/mini-baseline.jsonl itself: >=20 lines, and line 1 is BYTE-EXACT the
// W1-2 morphology probe (task spec, verbatim) - not merely "parses to the
// same values" but the identical bytes, since the task spec calls this out
// as a byte-exact requirement (tasks/README.md's Turkish-fixture rule
// applies equally to this canonical probe line).
func TestMiniBaselineFileShape(t *testing.T) {
	const path = "../../../eval/mini-baseline.jsonl"
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	var nonBlank []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			nonBlank = append(nonBlank, l)
		}
	}
	if len(nonBlank) < 20 {
		t.Fatalf("%s has %d non-blank lines, want >= 20", path, len(nonBlank))
	}
	const wantLine1 = `{"q": "evlerimizden", "expect_substring": "ev", "k": 5}`
	if nonBlank[0] != wantLine1 {
		t.Fatalf("line 1 = %q, want byte-exact %q", nonBlank[0], wantLine1)
	}

	qs, err := ParseBaseline(strings.NewReader(string(b)))
	if err != nil {
		t.Fatalf("ParseBaseline(%s): %v", path, err)
	}
	if len(qs) != len(nonBlank) {
		t.Fatalf("ParseBaseline returned %d questions, want %d", len(qs), len(nonBlank))
	}
	for i, q := range qs {
		if strings.TrimSpace(q.Q) == "" || strings.TrimSpace(q.ExpectSubstring) == "" {
			t.Errorf("line %d: empty q/expect_substring: %+v", i+1, q)
		}
		if q.K <= 0 {
			t.Errorf("line %d: k = %d, want > 0", i+1, q.K)
		}
	}
}

// --- RunBaseline scoring ---

func TestRunBaselinePassAbstainAndError(t *testing.T) {
	searcher := &fakeSearcher{byQuery: map[string][]Hit{
		"soru-pass":     {{Path: "a.md", Text: "içinde beklenen alt-dize var"}},
		"soru-abstain":  {}, // present but empty -> abstention
		"soru-no-match": {{Path: "b.md", Text: "alakasız içerik"}},
	}}
	qs := []Question{
		{Q: "soru-pass", ExpectSubstring: "beklenen alt-dize", K: 5},
		{Q: "soru-abstain", ExpectSubstring: "hiç bulunamayacak", K: 5},
		{Q: "soru-no-match", ExpectSubstring: "hiç bulunamayacak", K: 5},
		{Q: "soru-unindexed", ExpectSubstring: "yok", K: 0}, // no map entry at all -> empty -> abstention; K<=0 -> DefaultK
	}

	report, err := RunBaseline(context.Background(), searcher, "trace-1", qs)
	if err != nil {
		t.Fatalf("RunBaseline: %v", err)
	}
	if report.Total != 4 {
		t.Fatalf("Total = %d, want 4", report.Total)
	}
	if report.PassCount != 1 {
		t.Fatalf("PassCount = %d, want 1 (only soru-pass)", report.PassCount)
	}
	if !report.Results[0].Pass {
		t.Errorf("Results[0] (soru-pass) = %+v, want Pass=true", report.Results[0])
	}
	if report.Results[1].Pass || !report.Results[1].Abstained {
		t.Errorf("Results[1] (soru-abstain, empty hits) = %+v, want Pass=false Abstained=true", report.Results[1])
	}
	if report.Results[2].Pass {
		t.Errorf("Results[2] (soru-no-match) = %+v, want Pass=false (hit present but substring absent)", report.Results[2])
	}
	if !report.Results[3].Abstained {
		t.Errorf("Results[3] (soru-unindexed, k=0) = %+v, want Abstained=true (DefaultK must still apply)", report.Results[3])
	}
}

func TestRunBaselineSearchErrorIsAFailNeverAPass(t *testing.T) {
	searcher := &fakeSearcher{err: context.DeadlineExceeded}
	qs := []Question{{Q: "soru", ExpectSubstring: "x", K: 5}}
	report, err := RunBaseline(context.Background(), searcher, "trace-1", qs)
	if err != nil {
		t.Fatalf("RunBaseline: %v", err)
	}
	if report.PassCount != 0 || report.Results[0].Pass {
		t.Fatalf("a Search error must never score as a pass: %+v", report)
	}
	if report.Results[0].Err == "" {
		t.Error("Results[0].Err is empty, want the search error recorded")
	}
}

// --- DetectRegression ---

func TestDetectRegressionNilPreviousNeverRegresses(t *testing.T) {
	curr := Report{Total: 1, PassCount: 0, Results: []QuestionResult{{Q: "a", Pass: false}}}
	regressed, reasons := DetectRegression(nil, curr)
	if regressed || len(reasons) != 0 {
		t.Fatalf("DetectRegression(nil, ...) = (%v, %v), want (false, nil) - first-ever run can never regress", regressed, reasons)
	}
}

func TestDetectRegressionPassCountDrop(t *testing.T) {
	prev := Report{Total: 2, PassCount: 2, Results: []QuestionResult{{Q: "a", Pass: true}, {Q: "b", Pass: true}}}
	curr := Report{Total: 2, PassCount: 1, Results: []QuestionResult{{Q: "a", Pass: true}, {Q: "b", Pass: false}}}
	regressed, reasons := DetectRegression(&prev, curr)
	if !regressed {
		t.Fatal("DetectRegression: pass count dropped 2->1, want regressed=true")
	}
	if len(reasons) == 0 {
		t.Error("reasons is empty, want at least one explanation")
	}
}

func TestDetectRegressionSamePassCountDifferentQuestionStillRegresses(t *testing.T) {
	// Pass COUNT stays the same (1 == 1) but a DIFFERENT question flipped
	// pass->fail while another flipped fail->pass - the task spec's OTHER
	// regression clause ("a previously-passing question now failing") must
	// catch this even though the aggregate count alone would not.
	prev := Report{Total: 2, PassCount: 1, Results: []QuestionResult{{Q: "a", Pass: true}, {Q: "b", Pass: false}}}
	curr := Report{Total: 2, PassCount: 1, Results: []QuestionResult{{Q: "a", Pass: false}, {Q: "b", Pass: true}}}
	regressed, reasons := DetectRegression(&prev, curr)
	if !regressed {
		t.Fatal("DetectRegression: 'a' regressed pass->fail despite equal pass_count, want regressed=true")
	}
	found := false
	for _, r := range reasons {
		if strings.Contains(r, `"a"`) {
			found = true
		}
	}
	if !found {
		t.Errorf("reasons = %v, want one naming question %q", reasons, "a")
	}
}

func TestDetectRegressionImprovementIsNotARegression(t *testing.T) {
	prev := Report{Total: 1, PassCount: 0, Results: []QuestionResult{{Q: "a", Pass: false}}}
	curr := Report{Total: 1, PassCount: 1, Results: []QuestionResult{{Q: "a", Pass: true}}}
	regressed, reasons := DetectRegression(&prev, curr)
	if regressed {
		t.Fatalf("a fail->pass flip must never itself be a regression, got reasons=%v", reasons)
	}
}

// --- Runner.Run: the end-to-end hermetic gate ---

// TestRunnerRunFirstRunNeverRegressesAndLedgersEvent is the "before" half
// of the acceptance criterion (kahyad/internal/eval/mini_test.go: "the
// baseline runner on a fixture index").
func TestRunnerRunFirstRunNeverRegressesAndLedgersEvent(t *testing.T) {
	searcher := &fakeSearcher{byQuery: map[string][]Hit{
		"soru-1": {{Path: "a.md", Text: "cevap-1 burada"}},
		"soru-2": {{Path: "b.md", Text: "cevap-2 burada"}},
	}}
	qs := []Question{
		{Q: "soru-1", ExpectSubstring: "cevap-1", K: 5},
		{Q: "soru-2", ExpectSubstring: "cevap-2", K: 5},
	}
	store := &fakeEventStore{}
	r := &Runner{Questions: qs, Searcher: searcher, EventLogger: store, EventReader: store}

	out, err := r.Run(context.Background(), "trace-first")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.PreviousFound {
		t.Error("PreviousFound = true on the very first run, want false")
	}
	if out.Regressed {
		t.Errorf("Regressed = true on the very first run, want false (reasons=%v)", out.Reasons)
	}
	if out.Report.PassCount != 2 {
		t.Fatalf("PassCount = %d, want 2", out.Report.PassCount)
	}
	if len(store.rows) != 1 {
		t.Fatalf("ledgered %d events, want exactly 1", len(store.rows))
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(store.rows[0].Payload), &payload); err != nil {
		t.Fatalf("decode ledgered payload: %v", err)
	}
	if payload["trace_id"] != "trace-first" {
		t.Errorf("ledgered trace_id = %v, want %q", payload["trace_id"], "trace-first")
	}
	if pc, ok := payload["pass_count"].(float64); !ok || int(pc) != 2 {
		t.Errorf("ledgered pass_count = %v, want 2", payload["pass_count"])
	}
}

// TestRunnerRunDetectsInjectedRegression is THIS package's own gate test
// (task spec, verbatim): run the baseline once (all pass), then INJECT a
// regression (a question that passed before now returns nothing at all -
// simulating a consolidation/reindex/fusion change that silently broke
// retrieval for it) and prove the second Run reports Regressed=true - the
// exit-non-zero decision `kahya eval mini` (and the /v1/eval/mini/run
// handler) base their behavior on.
func TestRunnerRunDetectsInjectedRegression(t *testing.T) {
	qs := []Question{
		{Q: "soru-1", ExpectSubstring: "cevap-1", K: 5},
		{Q: "soru-2", ExpectSubstring: "cevap-2", K: 5},
		{Q: "soru-3", ExpectSubstring: "cevap-3", K: 5},
	}
	store := &fakeEventStore{}

	firstSearcher := &fakeSearcher{byQuery: map[string][]Hit{
		"soru-1": {{Path: "a.md", Text: "cevap-1 burada"}},
		"soru-2": {{Path: "b.md", Text: "cevap-2 burada"}},
		"soru-3": {{Path: "c.md", Text: "cevap-3 burada"}},
	}}
	r := &Runner{Questions: qs, Searcher: firstSearcher, EventLogger: store, EventReader: store}
	first, err := r.Run(context.Background(), "trace-before")
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if first.Regressed || first.Report.PassCount != 3 {
		t.Fatalf("first Run = %+v, want all 3 passing, Regressed=false", first)
	}

	// INJECTED REGRESSION: soru-2's index silently lost its match (e.g. a
	// consolidation run corrupted/dropped the note) - everything else still
	// passes.
	secondSearcher := &fakeSearcher{byQuery: map[string][]Hit{
		"soru-1": {{Path: "a.md", Text: "cevap-1 burada"}},
		"soru-2": {}, // now abstains - was passing before
		"soru-3": {{Path: "c.md", Text: "cevap-3 burada"}},
	}}
	r.Searcher = secondSearcher
	second, err := r.Run(context.Background(), "trace-after")
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if !second.PreviousFound {
		t.Fatal("second Run: PreviousFound = false, want true")
	}
	if !second.Regressed {
		t.Fatalf("second Run: Regressed = false, want true (soru-2 passed before, fails now); reasons=%v", second.Reasons)
	}
	foundReason := false
	for _, r := range second.Reasons {
		if strings.Contains(r, "soru-2") {
			foundReason = true
		}
	}
	if !foundReason {
		t.Errorf("Reasons = %v, want one naming soru-2", second.Reasons)
	}
	if len(store.rows) != 2 {
		t.Fatalf("ledgered %d events across both runs, want exactly 2 (one per run)", len(store.rows))
	}
	// The regressed run must ALSO be ledgered (never swallowed) - it
	// becomes the next run's own comparison baseline.
	var secondPayload map[string]any
	if err := json.Unmarshal([]byte(store.rows[1].Payload), &secondPayload); err != nil {
		t.Fatalf("decode second ledgered payload: %v", err)
	}
	if regressedField, _ := secondPayload["regressed"].(bool); !regressedField {
		t.Errorf("ledgered payload.regressed = %v, want true", secondPayload["regressed"])
	}
}

// TestEventMiniRunNeverEqualsEvalMiniPass is a deliberate, permanent
// regression guard (task spec's HARD CONSTRAINT, verbatim): this package's
// ledger event kind must never collide with W78-01's "eval.mini.pass",
// which is what unlocks kahyad/internal/consolidation's auto-commit guard.
func TestEventMiniRunNeverEqualsEvalMiniPass(t *testing.T) {
	if EventMiniRun != "eval.mini.run" {
		t.Fatalf("EventMiniRun = %q, want %q", EventMiniRun, "eval.mini.run")
	}
	const forbiddenAutoCommitUnlockEvent = "eval.mini.pass"
	if EventMiniRun == forbiddenAutoCommitUnlockEvent {
		t.Fatalf("EventMiniRun must never equal %q (W78-01's auto-commit-unlock event)", forbiddenAutoCommitUnlockEvent)
	}
}

// TestRunnerRunRequiresQuestionsOrPath proves Run fails loudly (never
// silently runs zero questions and reports a hollow "pass") when
// misconfigured.
func TestRunnerRunRequiresQuestionsOrPath(t *testing.T) {
	r := &Runner{Searcher: &fakeSearcher{}}
	if _, err := r.Run(context.Background(), "trace-x"); err == nil {
		t.Fatal("Run() with no Questions and no BaselinePath = nil error, want an error")
	}
}
