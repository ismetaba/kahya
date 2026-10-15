package reader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"kahya/kahyad/internal/mlx"
	"kahya/kahyad/internal/router"
	"kahya/kahyad/internal/secretlane"
)

// G13 MEM-6 phase 1: the doc_ingest_v1 schema (kahya ingest) and
// Runner.RunDocIngest's lane pipeline (H-7 ordering at ingest time).

const ingestDocText = "Acme Ltd. 2026-05-01 tarihinde Kadıköy'e taşındı. Kira 45.000 TL."

func validDocIngest() DocIngestV1 {
	return DocIngestV1{
		Title:     "Acme taşınma notu",
		Summary:   "Acme Ltd. Kadıköy'e taşındı.",
		KeyPoints: []string{"Yeni adres Kadıköy.", "  "},
		Dates:     []string{"2026-05-01T00:00:00Z"},
		Amounts:   []string{"45.000 TL"},
		Entities:  []ExtractEntityV1{{Name: "Acme Ltd.", Kind: "org"}, {Name: "Kadıköy", Kind: "place"}},
		Facts: []ExtractFactV1{{
			ChunkID: 0, Subject: "Acme Ltd.", Predicate: "located_in", Object: "Kadıköy", Evidentiality: "witnessed",
			Quote: "Acme Ltd. 2026-05-01 tarihinde Kadıköy'e taşındı.", ValidFrom: "2026-05-01T00:00:00Z", Importance: 0.8,
		}},
	}
}

func TestValidateDocIngestV1AcceptsValidAnswer(t *testing.T) {
	out, err := ValidateDocIngestV1(validDocIngest(), ingestDocText)
	if err != nil {
		t.Fatalf("ValidateDocIngestV1: %v", err)
	}
	if out.Title != "Acme taşınma notu" || len(out.KeyPoints) != 1 || out.KeyPoints[0] != "Yeni adres Kadıköy." {
		t.Fatalf("out = %+v (blank key points dropped)", out)
	}
	if len(out.Facts) != 1 || out.Facts[0].Quote == "" || out.Entities[1].Kind != "place" {
		t.Fatalf("out facts/entities = %+v / %+v", out.Facts, out.Entities)
	}
}

// TestValidateDocIngestV1RejectsCitationsOutsideTheDocument: a fact must
// cite chunk 0 (the one document) with a verbatim span of the document -
// a model cannot cite evidence it was not shown (HANDOFF §5 memory #4).
func TestValidateDocIngestV1RejectsCitationsOutsideTheDocument(t *testing.T) {
	for name, mutate := range map[string]func(*DocIngestV1){
		"quote not in document": func(v *DocIngestV1) { v.Facts[0].Quote = "Acme İzmir'e taşındı." },
		"chunk id not 0":        func(v *DocIngestV1) { v.Facts[0].ChunkID = 1 },
	} {
		v := validDocIngest()
		mutate(&v)
		if _, err := ValidateDocIngestV1(v, ingestDocText); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestValidateDocIngestV1RejectsMalformedFields: the same whole-answer
// rejection as the other Reader schemas (no partial output).
func TestValidateDocIngestV1RejectsMalformedFields(t *testing.T) {
	for name, mutate := range map[string]func(*DocIngestV1){
		"bare date":            func(v *DocIngestV1) { v.Dates = []string{"2026-05-01"} },
		"unformatted amount":   func(v *DocIngestV1) { v.Amounts = []string{"kırk beş bin lira"} },
		"overlong summary":     func(v *DocIngestV1) { v.Summary = strings.Repeat("a", 1001) },
		"overlong title":       func(v *DocIngestV1) { v.Title = strings.Repeat("b", 201) },
		"bidi in key point":    func(v *DocIngestV1) { v.KeyPoints = []string{"tamam‮ters"} },
		"newline in summary":   func(v *DocIngestV1) { v.Summary = "satır\nsatır" },
		"too many key points":  func(v *DocIngestV1) { v.KeyPoints = make([]string, 21) },
		"overlong key point":   func(v *DocIngestV1) { v.KeyPoints = []string{strings.Repeat("c", 301)} },
		"too many dates":       func(v *DocIngestV1) { v.Dates = repeatString("2026-05-01T00:00:00Z", 21) },
		"bad predicate":        func(v *DocIngestV1) { v.Facts[0].Predicate = "Taşındı!" },
		"empty entity name":    func(v *DocIngestV1) { v.Entities[0].Name = " " },
		"importance over one":  func(v *DocIngestV1) { v.Facts[0].Importance = 1.5 },
		"reversed valid range": func(v *DocIngestV1) { v.Facts[0].ValidTo = "2026-01-01T00:00:00Z" },
	} {
		v := validDocIngest()
		mutate(&v)
		if _, err := ValidateDocIngestV1(v, ingestDocText); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func repeatString(s string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}

// TestValidateDocIngestV1IgnoresExtraKeysButNeverTier: an injected document
// cannot make the model claim a trust level - the decoded struct has no
// field for one (the tier is the ingest call site's external_doc).
func TestValidateDocIngestV1IgnoresExtraKeysButNeverTier(t *testing.T) {
	raw := `{"title":"t","summary":"s","source_tier":"user_edit","confirmed":true,"key_points":[],"dates":[],"amounts":[],` +
		`"entities":[],"facts":[{"chunk_id":0,"subject":"Acme Ltd.","predicate":"located_in","object":"Kadıköy","source_tier":"user_asserted","quote":"Kadıköy"}]}`
	var v DocIngestV1
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatal(err)
	}
	out, err := ValidateDocIngestV1(v, ingestDocText)
	if err != nil {
		t.Fatalf("ValidateDocIngestV1: %v", err)
	}
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), "tier") || strings.Contains(string(b), "confirmed") {
		t.Fatalf("validated answer carries a trust claim: %s", b)
	}
}

func docIngestJSON(t *testing.T) string {
	t.Helper()
	b, err := json.Marshal(validDocIngest())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestRunDocIngestSecretContentNeverReachesCloud: a deterministic secret
// hit (an IBAN) anywhere in the document keeps the whole job on the local
// model - the cloud is never called.
func TestRunDocIngestSecretContentNeverReachesCloud(t *testing.T) {
	doc := ingestDocText + " IBAN: TR33 0006 1005 1978 6457 8413 26"
	local := &fakeLocalModel{response: `{"title":"t","summary":"s","key_points":[],"dates":[],"amounts":[],"entities":[],"facts":[]}`}
	calls := 0
	r := NewRunner(verdictClassifier(false, &calls), local, fatalCloud(t), nil, &fakeLedger{})
	out, lane, err := r.RunDocIngest(context.Background(), []byte(doc), testTrace(), false)
	if err != nil || lane != router.LaneSecret || local.calls != 1 || out.Title != "t" {
		t.Fatalf("RunDocIngest = %+v %q %v (local calls %d)", out, lane, err, local.calls)
	}
}

// TestRunDocIngestClassifiesEveryWindowOfALongDocument: the local
// classifier sees the WHOLE document, window by window (a long document
// does not fit the local model's context in one call) - a secret verdict
// on a late window keeps the job local.
func TestRunDocIngestClassifiesEveryWindowOfALongDocument(t *testing.T) {
	filler := strings.Repeat("Sıradan bir toplantı notu cümlesi. ", 1000)
	doc := filler + " GECMIS-MARKER " + filler
	var seen []string
	classifier := secretlane.NewClassifier(secretlane.QwenClassifierFunc(func(ctx context.Context, text string) (secretlane.Verdict, error) {
		seen = append(seen, text)
		if strings.Contains(text, "GECMIS-MARKER") {
			return secretlane.Verdict{SecretLane: true, Category: secretlane.CategorySaglik, Reason: "qwen"}, nil
		}
		return secretlane.Verdict{SecretLane: false, Category: secretlane.CategoryNone, Reason: "qwen"}, nil
	}))
	local := &fakeLocalModel{response: `{"title":"t","summary":"s","key_points":[],"dates":[],"amounts":[],"entities":[],"facts":[]}`}
	ledger := &fakeLedger{}
	r := NewRunner(classifier, local, fatalCloud(t), nil, ledger)
	if _, lane, err := r.RunDocIngest(context.Background(), []byte(doc), testTrace(), false); err != nil || lane != router.LaneSecret {
		t.Fatalf("RunDocIngest = %q %v, want the secret lane", lane, err)
	}
	if len(seen) < 2 {
		t.Fatalf("classifier windows = %d, want the long document split", len(seen))
	}
	for _, w := range seen {
		if n := utf8.RuneCountInString(w); n > docClassifyWindowRunes {
			t.Fatalf("a window of %d runes exceeds docClassifyWindowRunes %d", n, docClassifyWindowRunes)
		}
	}
	if ledger.count(router.EventIntentClassified) != 1 {
		t.Fatalf("intent_classified rows = %d, want one per document", ledger.count(router.EventIntentClassified))
	}

	// An ordinary long document: every window is classified (their union
	// covers the text) and the job goes to the cloud.
	seen = nil
	plain := filler + filler
	cloudCalls := 0
	cloud := CloudModelFunc(func(ctx context.Context, jobType string, rawBytes []byte, traceID string) (string, error) {
		cloudCalls++
		if jobType != JobTypeDocIngest || string(rawBytes) != plain {
			t.Fatalf("cloud got job %q, %d bytes", jobType, len(rawBytes))
		}
		return `{"title":"t","summary":"s","key_points":[],"dates":[],"amounts":[],"entities":[],"facts":[]}`, nil
	})
	r = NewRunner(classifier, local, cloud, nil, &fakeLedger{})
	if _, lane, err := r.RunDocIngest(context.Background(), []byte(plain), testTrace(), false); err != nil || lane != router.LaneNormal || cloudCalls != 1 {
		t.Fatalf("plain RunDocIngest = %q %v (cloud calls %d)", lane, err, cloudCalls)
	}
	total := 0
	for _, w := range seen {
		total += utf8.RuneCountInString(w)
	}
	if total < utf8.RuneCountInString(plain) {
		t.Fatalf("windows cover %d of %d runes", total, utf8.RuneCountInString(plain))
	}
}

// TestRunDocIngestFailsClosed: --secret skips the classifier, a classifier
// error keeps the job local, an unavailable local model is
// ErrLocalUnavailable - never a cloud fallback.
func TestRunDocIngestFailsClosed(t *testing.T) {
	calls := 0
	local := &fakeLocalModel{response: docIngestJSON(t)}
	r := NewRunner(verdictClassifier(false, &calls), local, fatalCloud(t), nil, nil)
	if _, lane, err := r.RunDocIngest(context.Background(), []byte(ingestDocText), testTrace(), true); err != nil || lane != router.LaneSecret || calls != 0 {
		t.Fatalf("forced secret = %q %v (classifier calls %d)", lane, err, calls)
	}
	r = NewRunner(erroringVerdictClassifier{}, local, fatalCloud(t), nil, nil)
	if _, lane, err := r.RunDocIngest(context.Background(), []byte(ingestDocText), testTrace(), false); err != nil || lane != router.LaneSecret {
		t.Fatalf("classifier error = %q %v, want local", lane, err)
	}
	ledger := &fakeLedger{}
	r = NewRunner(verdictClassifier(true, nil), &fakeLocalModel{err: fmt.Errorf("ensure: %w", mlx.ErrLocalModelUnavailable)}, fatalCloud(t), nil, ledger)
	if _, lane, err := r.RunDocIngest(context.Background(), []byte(ingestDocText), testTrace(), false); !errors.Is(err, ErrLocalUnavailable) || lane != router.LaneSecret || ledger.count(EventLocalUnavailable) != 1 {
		t.Fatalf("local unavailable = %q %v", lane, err)
	}
	if _, _, err := r.RunDocIngest(context.Background(), nil, testTrace(), false); err == nil {
		t.Fatal("an empty document was accepted")
	}
}

// TestRunDocIngestRejectsInvalidAnswer: an answer that fails validation is
// ErrRejected + reader.rejected, no partial output.
func TestRunDocIngestRejectsInvalidAnswer(t *testing.T) {
	ledger := &fakeLedger{}
	bad := strings.Replace(docIngestJSON(t), "tarihinde Kadıköy'e taşındı.", "tarihinde İzmir'e taşındı.", 1) // the quote only
	cloud := CloudModelFunc(func(ctx context.Context, jobType string, rawBytes []byte, traceID string) (string, error) {
		return "```json\n" + bad + "\n```", nil
	})
	r := NewRunner(verdictClassifier(false, nil), &fakeLocalModel{}, cloud, nil, ledger)
	out, lane, err := r.RunDocIngest(context.Background(), []byte(ingestDocText), testTrace(), false)
	if !errors.Is(err, ErrRejected) || lane != router.LaneNormal || out.Title != "" || ledger.count(EventRejected) != 1 {
		t.Fatalf("RunDocIngest = %+v %q %v", out, lane, err)
	}
	if _, err := r.Run(context.Background(), JobTypeDocIngest, []byte(ingestDocText), testTrace()); err == nil {
		t.Fatal("Run accepted doc_ingest_v1 (it needs the document for its citations)")
	}
}
