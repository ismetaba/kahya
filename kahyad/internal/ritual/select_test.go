package ritual

import (
	"context"
	"fmt"
	"testing"
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
	got, err := sampler.Select(context.Background())
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
	got, err := sampler.Select(context.Background())
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
	got, err := sampler.Select(context.Background())
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
	got, err := sampler.Select(context.Background())
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
	got, err := sampler.Select(context.Background())
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
	got, err := sampler.Select(context.Background())
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
	first, err := sampler.Select(context.Background())
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	second, err := sampler.Select(context.Background())
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
