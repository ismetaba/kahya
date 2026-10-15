package extract

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"kahya/kahyad/internal/factengine"
	"kahya/kahyad/internal/reader"
)

// TestWriteValidatedWritesIngestFactsUnderTheHold (G13 MEM-6): an ingest
// writes its already-validated answer through the extractor's own mapping
// while it holds the extraction lock - external_doc facts (the call site's
// provenance), evidence citing the whole episode ("episode:<id>"), the
// quote kept for a cloud-lane answer, entities linked, the episode stamped
// so the nightly job does not re-extract it - and no run can start while
// the lock is held.
func TestWriteValidatedWritesIngestFactsUnderTheHold(t *testing.T) {
	f := newFixture(t, func(int, []reader.ExtractChunk, bool) (reader.FactExtractV1, string, error) {
		t.Fatal("WriteValidated must not call the Reader")
		return reader.FactExtractV1{}, "", nil
	})
	ep, _ := f.seedEpisode("ingest/2026-10-08-acme.md", "Acme Kadıköy'e taşındı.")
	hold, err := f.runner.TryHold()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.runner.TryHold(); !errors.Is(err, ErrInProgress) {
		t.Fatalf("second TryHold = %v, want ErrInProgress", err)
	}
	if _, err := f.runner.RunPending(context.Background(), "tr-run"); !errors.Is(err, ErrInProgress) {
		t.Fatalf("RunPending during a hold = %v, want ErrInProgress", err)
	}
	st, err := hold.WriteValidated(context.Background(), "tr-ingest", ValidatedWrite{
		EpisodeID: ep, Provenance: factengine.ProvenanceExternalDoc, SessionID: "ingest:tr-ingest", CloudLane: true,
		Entities: []reader.ExtractEntityV1{{Name: "Acme", Kind: factengine.EntityKindOrg}, {Name: "Kadıköy", Kind: factengine.EntityKindPlace}},
		Facts: []reader.ExtractFactV1{{ChunkID: reader.DocIngestChunkID, Subject: "Acme", Predicate: "located_in", Object: "Kadıköy",
			Evidentiality: factengine.Witnessed, Quote: "Acme Kadıköy'e taşındı.", Importance: 0.8}},
	})
	hold.Release()
	hold.Release() // idempotent
	if err != nil || st.Facts != 1 || st.Entities != 2 || st.Episodes != 1 {
		t.Fatalf("WriteValidated = %+v %v", st, err)
	}
	fact := f.onlyFact()
	if fact.SourceTier != factengine.TierExternalDoc || fact.Evidence.String != fmt.Sprintf("episode:%d", ep) || fact.Quote.String != "Acme Kadıköy'e taşındı." ||
		!fact.SubjectEntityID.Valid || !fact.ObjectEntityID.Valid || fact.ExtractorVer.String != ExtractorVersion || !factengine.FactQuarantined(fact) {
		t.Fatalf("fact = %+v", fact)
	}
	if n := f.count(`SELECT count(*) FROM evidence WHERE fact_id = ? AND episode_id = ? AND session_id = 'ingest:tr-ingest'`, fact.ID, ep); n != 1 {
		t.Fatalf("evidence rows = %d, want 1 citing the episode", n)
	}
	if e := f.episode(ep); !e.ExtractedAt.Valid || e.ExtractorVer.String != ExtractorVersion {
		t.Fatalf("episode not stamped: %+v", e)
	}
	if _, err := f.runner.TryHold(); err != nil {
		t.Fatalf("TryHold after Release = %v", err)
	}
}

// TestWriteValidatedLocalLaneKeepsNoQuote: a local-lane (secret) ingest
// answer writes the triple but never its free text on the shared fact row.
func TestWriteValidatedLocalLaneKeepsNoQuote(t *testing.T) {
	f := newFixture(t, nil)
	ep, _ := f.seedEpisode("gizli/ingest/2026-10-08-tahlil.md", "x")
	hold, err := f.runner.TryHold()
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Release()
	if _, err := hold.WriteValidated(context.Background(), "tr", ValidatedWrite{
		EpisodeID: ep, Provenance: factengine.ProvenanceExternalDoc, SessionID: "ingest:tr",
		Facts: []reader.ExtractFactV1{{ChunkID: 0, Subject: "kullanıcı", Predicate: "other:test", Object: "x", Evidentiality: factengine.Witnessed, Quote: "x", ValidFrom: "2026-01-01T00:00:00Z"}},
	}); err != nil {
		t.Fatal(err)
	}
	if fact := f.onlyFact(); fact.Quote.Valid || fact.ValidFrom.Valid {
		t.Fatalf("local-lane fact carries free text: %+v", fact)
	}
	if _, err := hold.WriteValidated(context.Background(), "tr", ValidatedWrite{EpisodeID: 424242}); !errors.Is(err, ErrEpisodeNotFound) {
		t.Fatalf("unknown episode = %v, want ErrEpisodeNotFound", err)
	}
}
