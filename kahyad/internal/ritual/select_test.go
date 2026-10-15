package ritual

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"kahya/kahyad/internal/store"
)

const testMemoryDir = "/home/test/Kahya/memory"

var testSecretGlobs = []string{testMemoryDir + "/gizli/**"}

// TestSelectExcludesSecretLaneFact: a fact whose ONLY evidence cites an
// episode under a secret-lane glob is never selected (HANDOFF S5 safety
// #5 - "gizli-serit etiketli tek bir bayt Telegram'a gonderilmez").
func TestSelectExcludesSecretLaneFact(t *testing.T) {
	st := newTestStore(t)
	ep := seedEpisode(t, st, "gizli/maas.md")
	fact := seedFact(t, st, "Maas", "aylik", "50000 TL", "agent_derived", -0.2, false)
	seedClassifyingEvidence(t, st, fact.ID, ep)

	sampler := NewSampler(st.Queries, testMemoryDir, testSecretGlobs, nil)
	got, err := sampler.Select(context.Background(), "trace-select")
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	for _, f := range got {
		if f.ID == fact.ID {
			t.Fatalf("Select() included secret-lane fact %d", fact.ID)
		}
	}
}

// TestSelectExcludesFactWithNoClassificationRecord: a fact with NO
// evidence citing any resolvable episode path (here: an evidence row with
// no episode_id at all) is excluded fail-closed - "no classification
// record" is treated identically to "secret-lane".
func TestSelectExcludesFactWithNoClassificationRecord(t *testing.T) {
	st := newTestStore(t)
	fact := seedFact(t, st, "Emre", "sever", "kahve", "agent_derived", -0.2, false)
	seedUnclassifiableEvidence(t, st, fact.ID)

	sampler := NewSampler(st.Queries, testMemoryDir, testSecretGlobs, nil)
	got, err := sampler.Select(context.Background(), "trace-select")
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	for _, f := range got {
		if f.ID == fact.ID {
			t.Fatalf("Select() included unclassified fact %d", fact.ID)
		}
	}
}

// TestSelectExcludesFactWithNoEvidenceAtAll: a fact with ZERO evidence
// rows at all (never classified) is excluded fail-closed too.
func TestSelectExcludesFactWithNoEvidenceAtAll(t *testing.T) {
	st := newTestStore(t)
	fact := seedFact(t, st, "Emre", "yasar", "Istanbul", "user_asserted", 1.0, false)

	sampler := NewSampler(st.Queries, testMemoryDir, testSecretGlobs, nil)
	got, err := sampler.Select(context.Background(), "trace-select")
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	for _, f := range got {
		if f.ID == fact.ID {
			t.Fatalf("Select() included fact %d with zero evidence rows", fact.ID)
		}
	}
}

// TestSelectIncludesNonSecretClassifiedFact: a fact classified via a
// normal (non-secret-glob) episode path IS selected.
func TestSelectIncludesNonSecretClassifiedFact(t *testing.T) {
	st := newTestStore(t)
	ep := seedEpisode(t, st, "notlar/gunluk.md")
	fact := seedFact(t, st, "Emre", "sever", "kahve", "user_asserted", 1.0, false)
	seedClassifyingEvidence(t, st, fact.ID, ep)

	sampler := NewSampler(st.Queries, testMemoryDir, testSecretGlobs, nil)
	got, err := sampler.Select(context.Background(), "trace-select")
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	found := false
	for _, f := range got {
		if f.ID == fact.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("Select() did not include eligible fact %d", fact.ID)
	}
}

// TestSelectCapsAtTen: 15 eligible facts seeded, Select never returns
// more than MaxQuestionsPerRun.
func TestSelectCapsAtTen(t *testing.T) {
	st := newTestStore(t)
	ep := seedEpisode(t, st, "notlar/gunluk.md")
	for i := 0; i < 15; i++ {
		fact := seedFact(t, st, fmt.Sprintf("olgu-%d", i), "yuklem", "nesne", "user_asserted", 1.0, false)
		seedClassifyingEvidence(t, st, fact.ID, ep)
	}

	sampler := NewSampler(st.Queries, testMemoryDir, testSecretGlobs, nil)
	got, err := sampler.Select(context.Background(), "trace-select")
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if len(got) != MaxQuestionsPerRun {
		t.Fatalf("Select() returned %d facts, want %d (the cap)", len(got), MaxQuestionsPerRun)
	}
}

// TestSelectPrioritizesQuarantinedAgentDerivedFirst: a quarantined
// (unconfirmed) agent_derived fact outranks an otherwise-identical
// confirmed/non-agent_derived fact.
func TestSelectPrioritizesQuarantinedAgentDerivedFirst(t *testing.T) {
	st := newTestStore(t)
	ep := seedEpisode(t, st, "notlar/gunluk.md")

	plain := seedFact(t, st, "A", "b", "c", "user_asserted", 2.9, false)
	seedClassifyingEvidence(t, st, plain.ID, ep)

	quarantined := seedFact(t, st, "D", "e", "f", "agent_derived", -0.2, false)
	seedClassifyingEvidence(t, st, quarantined.ID, ep)

	sampler := NewSampler(st.Queries, testMemoryDir, testSecretGlobs, nil)
	got, err := sampler.Select(context.Background(), "trace-select")
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if len(got) < 2 {
		t.Fatalf("Select() returned %d facts, want >= 2", len(got))
	}
	if got[0].ID != quarantined.ID {
		t.Fatalf("Select()[0].ID = %d, want the quarantined fact %d first", got[0].ID, quarantined.ID)
	}
}

// TestSelectIsDeterministic: two runs against the SAME unchanged data
// return the identical order.
func TestSelectIsDeterministic(t *testing.T) {
	st := newTestStore(t)
	ep := seedEpisode(t, st, "notlar/gunluk.md")
	for i := 0; i < 5; i++ {
		fact := seedFact(t, st, fmt.Sprintf("olgu-%d", i), "yuklem", "nesne", "user_asserted", 1.0, false)
		seedClassifyingEvidence(t, st, fact.ID, ep)
	}

	sampler := NewSampler(st.Queries, testMemoryDir, testSecretGlobs, nil)
	first, err := sampler.Select(context.Background(), "trace-select")
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	second, err := sampler.Select(context.Background(), "trace-select")
	if err != nil {
		t.Fatalf("Select() (second call) error = %v", err)
	}
	if len(first) != len(second) {
		t.Fatalf("len(first)=%d != len(second)=%d", len(first), len(second))
	}
	for i := range first {
		if first[i].ID != second[i].ID {
			t.Fatalf("Select() order changed at index %d: %d vs %d", i, first[i].ID, second[i].ID)
		}
	}
}

// ---- FIX-P-17: content classification of the fact TEXT ----

type fakeFactClassifier struct {
	secret bool
	err    error
}

func (f fakeFactClassifier) ClassifySecretLane(context.Context, string) (bool, error) {
	return f.secret, f.err
}

type excludedRow struct {
	traceID string
	factID  int64
	reason  string
}

// excludedRows returns every ritual.fact_excluded_secret_lane row, in
// insertion order.
func excludedRows(t *testing.T, st *store.Store) []excludedRow {
	t.Helper()
	rows, err := st.DB().Query(`SELECT trace_id, payload FROM events WHERE kind = ? ORDER BY id`, EventFactExcludedSecretLane)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []excludedRow
	for rows.Next() {
		var trace, p string
		if err := rows.Scan(&trace, &p); err != nil {
			t.Fatal(err)
		}
		var m struct {
			FactID int64  `json:"fact_id"`
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal([]byte(p), &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, excludedRow{traceID: trace, factID: m.FactID, reason: m.Reason})
	}
	return out
}

func selectedIDs(t *testing.T, s *Sampler, traceID string) map[int64]bool {
	t.Helper()
	got, err := s.Select(context.Background(), traceID)
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	ids := make(map[int64]bool, len(got))
	for _, f := range got {
		ids[f.ID] = true
	}
	return ids
}

// TestSelectExcludesFactWhoseTextIsSecretLaneContent: an ordinary-path fact
// whose text is an IBAN, or a saglik lexicon term, is excluded; a benign
// sibling on the same path is not.
func TestSelectExcludesFactWhoseTextIsSecretLaneContent(t *testing.T) {
	st := newTestStore(t)
	ep := seedEpisode(t, st, "notlar/genel.md")
	iban := seedFact(t, st, "Ahmet'in IBAN'i", "su", "TR33 0006 1005 1978 6457 8413 26", "user_asserted", 1.0, false)
	seedClassifyingEvidence(t, st, iban.ID, ep)
	doctor := seedFact(t, st, "Emre", "gonderdi", "doktor raporu ekte", "user_asserted", 1.0, false)
	seedClassifyingEvidence(t, st, doctor.ID, ep)
	benign := seedFact(t, st, "Emre", "sever", "kahve", "user_asserted", 1.0, false)
	seedClassifyingEvidence(t, st, benign.ID, ep)

	s := NewSampler(st.Queries, testMemoryDir, testSecretGlobs, nil)
	s.SetLedger(st)
	ids := selectedIDs(t, s, "trace-content")
	if ids[iban.ID] || ids[doctor.ID] || !ids[benign.ID] {
		t.Fatalf("Select() = %v, want only the benign fact %d (not %d IBAN, %d doktor)", ids, benign.ID, iban.ID, doctor.ID)
	}
	for _, r := range excludedRows(t, st) {
		if r.reason != ExcludeReasonContentDeterministic {
			t.Fatalf("exclusion %+v, want reason %s", r, ExcludeReasonContentDeterministic)
		}
	}
	if n := len(excludedRows(t, st)); n != 2 {
		t.Fatalf("exclusion rows = %d, want 2", n)
	}
}

// TestSelectExcludesFactWhenFactClassifierErrors: a wired classifier's
// error excludes the fact (fail-closed) and is ledgered classify_failed.
func TestSelectExcludesFactWhenFactClassifierErrors(t *testing.T) {
	st := newTestStore(t)
	ep := seedEpisode(t, st, "notlar/genel.md")
	f := seedFact(t, st, "olgu", "yuklem", "nesne", "user_asserted", 1.0, false)
	seedClassifyingEvidence(t, st, f.ID, ep)

	s := NewSampler(st.Queries, testMemoryDir, testSecretGlobs, nil)
	s.SetLedger(st)
	s.SetFactClassifier(fakeFactClassifier{err: errors.New("qwen down")})
	if ids := selectedIDs(t, s, "trace-cerr"); ids[f.ID] {
		t.Fatalf("Select() included fact %d although the classifier failed", f.ID)
	}
	if ex := excludedRows(t, st); len(ex) != 1 || ex[0].reason != ExcludeReasonClassifyFailed {
		t.Fatalf("exclusion rows = %+v, want one %s", ex, ExcludeReasonClassifyFailed)
	}
}

func TestSelectExcludesFactWhenFactClassifierSaysSecret(t *testing.T) {
	st := newTestStore(t)
	ep := seedEpisode(t, st, "notlar/genel.md")
	f := seedFact(t, st, "olgu", "yuklem", "nesne", "user_asserted", 1.0, false)
	seedClassifyingEvidence(t, st, f.ID, ep)

	s := NewSampler(st.Queries, testMemoryDir, testSecretGlobs, nil)
	s.SetLedger(st)
	s.SetFactClassifier(fakeFactClassifier{secret: true})
	if ids := selectedIDs(t, s, "trace-csecret"); ids[f.ID] {
		t.Fatalf("Select() included fact %d although the classifier said secret", f.ID)
	}
	if ex := excludedRows(t, st); len(ex) != 1 || ex[0].reason != ExcludeReasonContentClassifier {
		t.Fatalf("exclusion rows = %+v, want one %s", ex, ExcludeReasonContentClassifier)
	}
}

// TestSelectIncludesFactWhenFactClassifierSaysNormal is the positive
// control: an ordinary text passes the deterministic pre-pass and a
// classifier saying normal - included, nothing ledgered.
func TestSelectIncludesFactWhenFactClassifierSaysNormal(t *testing.T) {
	st := newTestStore(t)
	ep := seedEpisode(t, st, "notlar/genel.md")
	f := seedFact(t, st, "olgu", "yuklem", "nesne", "user_asserted", 1.0, false)
	seedClassifyingEvidence(t, st, f.ID, ep)

	s := NewSampler(st.Queries, testMemoryDir, testSecretGlobs, nil)
	s.SetLedger(st)
	s.SetFactClassifier(fakeFactClassifier{secret: false})
	if ids := selectedIDs(t, s, "trace-cnormal"); !ids[f.ID] {
		t.Fatalf("Select() excluded fact %d, want it included", f.ID)
	}
	if ex := excludedRows(t, st); len(ex) != 0 {
		t.Fatalf("exclusion rows = %+v, want none", ex)
	}
}

// TestSelectNilFactClassifierIsDeterministicOnly: production's wiring (no
// classifier, owner decision G02-4) - ordinary text in, IBAN text out.
func TestSelectNilFactClassifierIsDeterministicOnly(t *testing.T) {
	st := newTestStore(t)
	ep := seedEpisode(t, st, "notlar/genel.md")
	plain := seedFact(t, st, "olgu", "yuklem", "nesne", "user_asserted", 1.0, false)
	seedClassifyingEvidence(t, st, plain.ID, ep)
	iban := seedFact(t, st, "hesap", "numarasi", "TR33 0006 1005 1978 6457 8413 26", "user_asserted", 1.0, false)
	seedClassifyingEvidence(t, st, iban.ID, ep)

	s := NewSampler(st.Queries, testMemoryDir, testSecretGlobs, nil)
	ids := selectedIDs(t, s, "trace-nil")
	if !ids[plain.ID] || ids[iban.ID] {
		t.Fatalf("Select() = %v, want %d in and %d out", ids, plain.ID, iban.ID)
	}
}

// TestSelectLedgersPathGlobExclusionWithTrace: the existing path-glob
// exclusion is ledgered too, with the caller's trace_id; so is the
// no-classification-record exclusion.
func TestSelectLedgersPathGlobExclusionWithTrace(t *testing.T) {
	st := newTestStore(t)
	ep := seedEpisode(t, st, "gizli/maas.md")
	secret := seedFact(t, st, "Maas", "aylik", "50000 TL", "agent_derived", -0.2, false)
	seedClassifyingEvidence(t, st, secret.ID, ep)
	unclassified := seedFact(t, st, "Emre", "sever", "kahve", "agent_derived", -0.2, false)
	seedUnclassifiableEvidence(t, st, unclassified.ID)

	s := NewSampler(st.Queries, testMemoryDir, testSecretGlobs, nil)
	s.SetLedger(st)
	if ids := selectedIDs(t, s, "trace-glob-ledger"); ids[secret.ID] || ids[unclassified.ID] {
		t.Fatalf("Select() = %v, want neither %d nor %d", ids, secret.ID, unclassified.ID)
	}
	want := map[int64]string{secret.ID: ExcludeReasonPathGlob, unclassified.ID: ExcludeReasonNoClassification}
	ex := excludedRows(t, st)
	if len(ex) != 2 {
		t.Fatalf("exclusion rows = %+v, want 2", ex)
	}
	for _, r := range ex {
		if r.traceID != "trace-glob-ledger" || want[r.factID] != r.reason {
			t.Fatalf("exclusion row %+v, want trace trace-glob-ledger and reason %q", r, want[r.factID])
		}
	}
}

// erroringGlobMatcher fails every glob evaluation.
type erroringGlobMatcher struct{}

func (erroringGlobMatcher) Match(string, string) (bool, error) {
	return false, errors.New("glob cannot be evaluated")
}

// TestSelectExcludesFactWhenSecretGlobErrors (G13 common ground, memlane): a
// secret-lane glob that cannot be evaluated excludes the fact (fail-closed,
// ledgered path_glob) - before the classification moved to memlane a match
// error counted as "no match" and the fact reached Telegram. The same fact
// is selected when the glob evaluates normally.
func TestSelectExcludesFactWhenSecretGlobErrors(t *testing.T) {
	st := newTestStore(t)
	ep := seedEpisode(t, st, "notlar/gunluk.md")
	fact := seedFact(t, st, "Emre", "sever", "kahve", "user_asserted", 1.0, false)
	seedClassifyingEvidence(t, st, fact.ID, ep)

	if ids := selectedIDs(t, NewSampler(st.Queries, testMemoryDir, testSecretGlobs, nil), "trace-glob-ok"); !ids[fact.ID] {
		t.Fatalf("Select() = %v, want fact %d with a working matcher", ids, fact.ID)
	}
	s := NewSampler(st.Queries, testMemoryDir, testSecretGlobs, erroringGlobMatcher{})
	s.SetLedger(st)
	if ids := selectedIDs(t, s, "trace-glob-err"); ids[fact.ID] {
		t.Fatalf("Select() = %v, a glob error must exclude fact %d", ids, fact.ID)
	}
	ex := excludedRows(t, st)
	if len(ex) != 1 || ex[0].factID != fact.ID || ex[0].reason != ExcludeReasonPathGlob || ex[0].traceID != "trace-glob-err" {
		t.Fatalf("exclusion rows = %+v, want one path_glob row for fact %d", ex, fact.ID)
	}
}
