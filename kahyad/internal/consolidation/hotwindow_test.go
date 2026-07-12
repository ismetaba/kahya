package consolidation

import (
	"context"
	"testing"
	"time"
)

func TestExtractDetailAtomsFindsEveryCategory(t *testing.T) {
	text := "Toplantida 1500 TL odedik. Tarih: 12.07.2026 olarak belirlendi. " +
		"O sirada \"tam olarak bunu istiyorum\" dedi. Ben de karar verdim ki bu sekilde devam edecegiz."

	atoms := ExtractDetailAtoms(text)
	if len(atoms) == 0 {
		t.Fatal("ExtractDetailAtoms() returned nothing")
	}

	var haveNumber, haveDate, haveQuote, haveDecision bool
	for _, a := range atoms {
		switch a.Kind {
		case AtomNumber:
			haveNumber = true
		case AtomDate:
			haveDate = true
		case AtomQuote:
			haveQuote = true
		case AtomDecisionOrPromise:
			haveDecision = true
		}
	}
	if !haveNumber {
		t.Error("missing AtomNumber")
	}
	if !haveDate {
		t.Error("missing AtomDate")
	}
	if !haveQuote {
		t.Error("missing AtomQuote")
	}
	if !haveDecision {
		t.Error("missing AtomDecisionOrPromise")
	}
}

func TestValidateSummaryEvidenceRejectsSummary(t *testing.T) {
	refs := []EvidenceRef{{Kind: EvidenceEpisode, ID: 1}, {Kind: EvidenceSummary, ID: 2}}
	if err := ValidateSummaryEvidence(refs); err == nil {
		t.Fatal("ValidateSummaryEvidence() = nil, want ErrSummaryFromSummary")
	} else if err != ErrSummaryFromSummary {
		t.Fatalf("ValidateSummaryEvidence() error = %v, want ErrSummaryFromSummary", err)
	}
}

func TestValidateSummaryEvidenceAcceptsRawEvidence(t *testing.T) {
	refs := []EvidenceRef{{Kind: EvidenceEpisode, ID: 1}, {Kind: EvidenceChunk, ID: 7}}
	if err := ValidateSummaryEvidence(refs); err != nil {
		t.Fatalf("ValidateSummaryEvidence() error = %v, want nil", err)
	}
}

// fakeFactStore is an in-memory FactStore fake - PromoteHotWindow's own
// logic tests use this instead of a real brain.db (a SEPARATE integration
// test, TestPromoteHotWindowAgainstRealStore in hotwindow_storewriter_test.go,
// proves StoreFactWriter's own SQL wiring against a real temp SQLite db).
type fakeFactStore struct {
	episodes       []Episode
	chunksByEp     map[int64][]Chunk
	inserted       []CandidateFact
	cooledEpisodes map[int64]time.Time
}

func (f *fakeFactStore) ListUncooledEpisodesOlderThan(ctx context.Context, cutoff time.Time) ([]Episode, error) {
	var out []Episode
	for _, ep := range f.episodes {
		if f.cooledEpisodes != nil {
			if _, cooled := f.cooledEpisodes[ep.ID]; cooled {
				continue
			}
		}
		if !ep.CreatedAt.After(cutoff) {
			out = append(out, ep)
		}
	}
	return out, nil
}

func (f *fakeFactStore) ListChunksByEpisode(ctx context.Context, episodeID int64) ([]Chunk, error) {
	return f.chunksByEp[episodeID], nil
}

func (f *fakeFactStore) InsertFact(ctx context.Context, fact CandidateFact) (int64, error) {
	f.inserted = append(f.inserted, fact)
	return int64(len(f.inserted)), nil
}

func (f *fakeFactStore) MarkEpisodeCooled(ctx context.Context, episodeID int64, at time.Time) error {
	if f.cooledEpisodes == nil {
		f.cooledEpisodes = map[int64]time.Time{}
	}
	f.cooledEpisodes[episodeID] = at
	return nil
}

// TestPromoteHotWindowPromotesBeforeCooling is the acceptance test: a
// synthetic 91-day-old episode's detail atoms are promoted to
// source_tier=agent_derived facts BEFORE the episode is marked cooled.
func TestPromoteHotWindowPromotesBeforeCooling(t *testing.T) {
	now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -91)

	store := &fakeFactStore{
		episodes: []Episode{{ID: 42, CreatedAt: old}},
		chunksByEp: map[int64][]Chunk{
			42: {{ID: 100, Text: "Karar verdim ki bu proje 5000 TL butce ile 15.03.2026 tarihinde baslayacak."}},
		},
	}

	promoted, err := PromoteHotWindow(context.Background(), store, now)
	if err != nil {
		t.Fatalf("PromoteHotWindow() error = %v", err)
	}
	if promoted == 0 {
		t.Fatal("PromoteHotWindow() promoted 0 facts, want > 0")
	}
	for _, f := range store.inserted {
		if f.SourceTier != "agent_derived" {
			t.Errorf("fact SourceTier = %q, want agent_derived", f.SourceTier)
		}
		if f.Evidentiality != "inferred" {
			t.Errorf("fact Evidentiality = %q, want inferred", f.Evidentiality)
		}
		if f.Evidence == "" {
			t.Errorf("fact Evidence is empty, want a raw episode/chunk citation")
		}
	}
	if _, cooled := store.cooledEpisodes[42]; !cooled {
		t.Fatal("episode 42 was never marked cooled")
	}
}

// TestPromoteHotWindowSkipsFreshEpisode proves the <90-day window is left
// alone (not yet eligible, not cooled).
func TestPromoteHotWindowSkipsFreshEpisode(t *testing.T) {
	now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	fresh := now.AddDate(0, 0, -10)

	store := &fakeFactStore{
		episodes:   []Episode{{ID: 7, CreatedAt: fresh}},
		chunksByEp: map[int64][]Chunk{7: {{ID: 1, Text: "1000 TL, 01.01.2026, \"alinti\", karar verdim."}}},
	}
	promoted, err := PromoteHotWindow(context.Background(), store, now)
	if err != nil {
		t.Fatalf("PromoteHotWindow() error = %v", err)
	}
	if promoted != 0 {
		t.Fatalf("PromoteHotWindow() promoted %d facts for a fresh episode, want 0", promoted)
	}
	if len(store.cooledEpisodes) != 0 {
		t.Fatalf("a fresh episode should never be cooled: %+v", store.cooledEpisodes)
	}
}

// TestPromoteHotWindowIdempotentAcrossRuns proves an already-cooled
// episode is never re-scanned on a subsequent run.
func TestPromoteHotWindowIdempotentAcrossRuns(t *testing.T) {
	now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -91)
	store := &fakeFactStore{
		episodes:   []Episode{{ID: 5, CreatedAt: old}},
		chunksByEp: map[int64][]Chunk{5: {{ID: 9, Text: "Karar verdim ki 200 TL odenecek 01.01.2026 tarihinde."}}},
	}
	if _, err := PromoteHotWindow(context.Background(), store, now); err != nil {
		t.Fatalf("first PromoteHotWindow() error = %v", err)
	}
	firstCount := len(store.inserted)

	if _, err := PromoteHotWindow(context.Background(), store, now); err != nil {
		t.Fatalf("second PromoteHotWindow() error = %v", err)
	}
	if len(store.inserted) != firstCount {
		t.Fatalf("second run inserted %d more facts, want 0 more (episode already cooled)", len(store.inserted)-firstCount)
	}
}
