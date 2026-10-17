package extract

import (
	"context"
	"testing"
	"time"

	"kahya/kahyad/internal/factengine"
	"kahya/kahyad/internal/graph"
	"kahya/kahyad/internal/reader"
)

// TestExtractLocalLaneRetractionIsNeverHistory (FIX-FINAL fact-history-
// ended-vs-corrected, review): a retraction made by a LOCAL-lane call - a
// secret-lane path, or secret content on an ordinary path - is recorded
// corrected, never ended, whatever its quote says: an ended fact is
// history every as_of / known_as_of read shows (its window, its retraction
// time), and that history would come only from the secret note. The quote
// of a local call is never consulted. A cloud-lane call's retraction (the
// control) stays ended and is shown.
func TestExtractLocalLaneRetractionIsNeverHistory(t *testing.T) {
	cases := []struct {
		name, path, text, marker string
		local                    bool
	}{
		{"secret path", "finans-emre.md", "Emre artık BankX'te değil, kredi borcu kaldı.", "", true},
		{"secret content on an ordinary path", "people/emre-tahlil.md", "Emre artık BankX'te değil, tahlil sonucu geldi.", "tahlil", true},
		{"cloud lane (control)", "people/emre-is.md", "Emre artık BankX'te değil.", "", false},
	}
	const stated = "Emre BankX'te çalışıyor."
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, func(_ int, chunks []reader.ExtractChunk, forceSecret bool) (reader.FactExtractV1, string, error) {
				fct := triple(chunks[0].ID, "Emre", "works_at", "BankX")
				switch chunks[0].Text {
				case stated:
					return reader.FactExtractV1{Entities: []reader.ExtractEntityV1{ent("Emre", factengine.EntityKindPerson)}, Facts: []reader.ExtractFactV1{fct}}, laneOf(forceSecret), nil
				case c.text:
					// An "artık ... değil" quote: an ending if a cloud call said it.
					fct.Retraction, fct.Quote = true, "Emre artık BankX'te değil"
					return reader.FactExtractV1{Facts: []reader.ExtractFactV1{fct}}, laneOf(forceSecret), nil
				}
				t.Fatalf("unexpected chunk %q", chunks[0].Text)
				return reader.FactExtractV1{}, "", nil
			})
			f.secretOn = c.marker
			ctx := context.Background()
			at := func(s string) {
				ts, _ := time.Parse(time.RFC3339, s)
				f.engine.SetClock(func() time.Time { return ts })
			}
			at("2026-02-01T00:00:00Z")
			ep, _ := f.seedEpisode("people/emre.md", stated)
			if _, err := f.runner.ExtractEpisode(ctx, "tr-state", ep, factengine.ProvenanceAgentDerived); err != nil {
				t.Fatal(err)
			}
			at("2026-05-01T00:00:00Z")
			ep, _ = f.seedEpisode(c.path, c.text)
			st, err := f.runner.ExtractEpisode(ctx, "tr-retract", ep, factengine.ProvenanceAgentDerived)
			if err != nil || st.Retractions != 1 {
				t.Fatalf("retraction run = %+v, %v; want one retraction", st, err)
			}
			if f.reader.call(0).forceSecret || f.reader.call(1).forceSecret != c.local {
				t.Fatalf("scenario: the stating note goes to the cloud, the retracting one local=%v", c.local)
			}

			fact := f.onlyFact()
			want := factengine.RetractionEnded
			if c.local {
				want = factengine.RetractionCorrected
			}
			if fact.Status != "retracted" || fact.RetractionKind.String != string(want) {
				t.Fatalf("fact status/kind = %q/%v, want retracted %s", fact.Status, fact.RetractionKind, want)
			}

			g := &graph.Graph{Store: f.st.Queries, DB: f.st.DB(), Lane: f.runner.Lane}
			march, _ := time.Parse(time.RFC3339, "2026-03-01T00:00:00Z")
			for _, cloud := range []bool{true, false} {
				for _, q := range []graph.FactQuery{
					{Subject: "Emre", AsOf: march, IncludeQuarantined: true, CloudLane: cloud},
					{Subject: "Emre", KnownAsOf: march, IncludeQuarantined: true, CloudLane: cloud},
				} {
					got, err := g.QueryFacts(ctx, "tr-fq", q)
					if err != nil {
						t.Fatal(err)
					}
					if c.local && len(got) != 0 {
						t.Fatalf("query %+v = %+v: a local-lane retraction became history", q, got)
					}
					if !c.local && (len(got) != 1 || !got[0].Ended) {
						t.Fatalf("query %+v = %+v, want the ended fact (cloud-lane retraction)", q, got)
					}
				}
			}
		})
	}
}
