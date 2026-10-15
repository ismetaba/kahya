package extract

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"kahya/kahyad/internal/factengine"
	"kahya/kahyad/internal/reader"
)

// TestExtractLocalLaneCallNeverLendsItsQuoteToACloudEligibleFact (FIX-MEM-3
// review): a call that ran on the local model writes the triple and the
// raw citation only - never its verbatim quote or its validity window -
// because the fact row it lands on can become cloud-eligible through ANOTHER
// note's citation (memlane classifies the triple, and the MEM-4 fact tools
// return the row's quote with it). Each case states "Emre works_at Acme"
// once from a local-lane call (secret content under an ordinary path, a
// path-less episode, a path made secret after a content-secret run) and
// once from an ordinary note, in both orders and within one note: the fact
// ends up cloud-eligible, and its quote / valid_from are the ordinary
// call's or NULL - never the local call's.
func TestExtractLocalLaneCallNeverLendsItsQuoteToACloudEligibleFact(t *testing.T) {
	const (
		marker       = "ozel-isaret-77" // only the local classifier calls it secret
		secretText   = "Emre Acme'deki işinde panik atak geçirdi, " + marker + ". Doktora gidiyor."
		secretQuote  = "Emre Acme'deki işinde panik atak geçirdi, " + marker
		secretFrom   = "2026-03-01T00:00:00Z"
		ordinaryText = "Emre Acme'de çalışıyor. Toplantı perşembe."
		ordQuote     = "Emre Acme'de çalışıyor"
	)
	type step struct {
		path, text string
		quote      bool // the ordinary call cites its quote (the local call always does)
		globAfter  bool // add a secret glob for path, then re-extract it (--all)
	}
	for _, tc := range []struct {
		name      string
		steps     []step
		perCall   int
		wantQuote string
	}{
		{name: "content-secret note first", steps: []step{{path: "inbox/saglik-gunlugu.md", text: secretText}, {path: "inbox/is.md", text: ordinaryText, quote: true}}, wantQuote: ordQuote},
		{name: "ordinary note first without a quote", steps: []step{{path: "inbox/is.md", text: ordinaryText}, {path: "inbox/saglik-gunlugu.md", text: secretText}}},
		{name: "path-less episode first", steps: []step{{path: "", text: secretText}, {path: "inbox/is.md", text: ordinaryText, quote: true}}, wantQuote: ordQuote},
		{name: "one note, ordinary chunk then secret chunk", steps: []step{{path: "inbox/2026-10-15.md", text: ordinaryText + "\x00" + secretText}}, perCall: 1},
		{name: "path made secret after a content-secret run", steps: []step{{path: "inbox/saglik-gunlugu.md", text: secretText, globAfter: true}, {path: "inbox/is.md", text: ordinaryText}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cur step
			f := newFixture(t, func(n int, chunks []reader.ExtractChunk, forceSecret bool) (reader.FactExtractV1, string, error) {
				var facts []reader.ExtractFactV1
				for _, c := range chunks {
					fact := reader.ExtractFactV1{ChunkID: c.ID, Subject: "Emre", Predicate: "works_at", Object: "Acme", Evidentiality: factengine.Reported, Importance: 0.5}
					if strings.Contains(c.Text, marker) {
						fact.Quote, fact.ValidFrom = secretQuote, secretFrom
					} else if cur.quote {
						fact.Quote = ordQuote
					}
					facts = append(facts, fact)
				}
				return reader.FactExtractV1{
					Entities: []reader.ExtractEntityV1{{Name: "Emre", Kind: factengine.EntityKindPerson}, {Name: "Acme", Kind: factengine.EntityKindOrg}},
					Facts:    facts,
				}, laneOf(forceSecret), nil
			})
			f.secretOn = marker
			f.runner.Cfg.ChunksPerCall = tc.perCall
			ctx := context.Background()
			for _, s := range tc.steps {
				cur = s
				epID, _ := f.seedEpisode(s.path, strings.Split(s.text, "\x00")...)
				if _, err := f.runner.ExtractEpisode(ctx, "tr-lane", epID, factengine.ProvenanceAgentDerived); err != nil {
					t.Fatalf("ExtractEpisode(%q): %v", s.path, err)
				}
				if s.globAfter {
					f.runner.Lane.SecretGlobs = append(f.runner.Lane.SecretGlobs, filepath.Join(f.memDir, filepath.FromSlash(s.path)))
					if _, err := f.runner.ExtractAll(ctx, "tr-lane-all"); err != nil {
						t.Fatalf("ExtractAll after the glob change: %v", err)
					}
					if !f.reader.call(f.reader.callCount() - 1).forceSecret {
						t.Fatal("the re-extraction after the glob change was not forced local")
					}
				}
			}
			fact := f.onlyFact()
			ok, err := f.runner.Lane.CloudEligible(ctx, fact)
			if err != nil || !ok {
				t.Fatalf("CloudEligible = %v, %v; want true - the ordinary note states the triple (the case must exercise a cloud-eligible row)", ok, err)
			}
			if strings.Contains(fact.Quote.String, marker) || fact.Quote.String != tc.wantQuote || fact.Quote.Valid != (tc.wantQuote != "") {
				t.Fatalf("cloud-eligible fact quote = %+v, want %q: a local-lane call's quote never lands on the row", fact.Quote, tc.wantQuote)
			}
			if fact.ValidFrom.Valid || fact.ValidTo.Valid {
				t.Fatalf("cloud-eligible fact validity = %+v / %+v, want NULL: a local-lane call's window never lands on the row", fact.ValidFrom, fact.ValidTo)
			}
		})
	}
}
