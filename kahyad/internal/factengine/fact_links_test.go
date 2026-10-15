package factengine

import (
	"context"
	"strings"
	"testing"
	"time"
)

func linkedCandidate(subject, object string) Candidate {
	return Candidate{
		Subject: subject, Predicate: "çalışır", Object: object,
		Provenance: ProvenanceAgentDerived, SessionID: "sess-links", ExtractorVer: "fact_extract_v1",
		Evidence: "episode:1,chunk:1",
	}
}

// TestWriteFactStoresEntityLinksQuoteAndValidity (FIX-MEM-2 phase 0): a new
// fact carries the call site's entity links, the cited quote and its validity
// window (normalized to UTC RFC3339Nano) - before 0024 every fact was born
// with NULL valid_from/valid_to whatever the extractor knew.
func TestWriteFactStoresEntityLinksQuoteAndValidity(t *testing.T) {
	e, st := newTestEngine(t)
	ctx := context.Background()
	emre, _, _ := resolve(t, e, "Emre", "person")
	org, _, _ := resolve(t, e, "gold-token", "org")

	c := linkedCandidate("Emre", "gold-token")
	c.SubjectEntityID, c.ObjectEntityID = emre, org
	c.Quote = "Emre gold-token ekibinde çalışıyor"
	c.ValidFrom = "2026-03-01T09:00:00+03:00"
	id, err := e.WriteFact(ctx, "trace-links", c)
	if err != nil {
		t.Fatalf("WriteFact: %v", err)
	}
	f, err := st.Queries.GetFact(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if f.SubjectEntityID.Int64 != emre || f.ObjectEntityID.Int64 != org || f.Quote.String != c.Quote {
		t.Fatalf("links = %+v %+v %q", f.SubjectEntityID, f.ObjectEntityID, f.Quote.String)
	}
	if f.ValidFrom.String != "2026-03-01T06:00:00Z" || f.ValidTo.Valid {
		t.Fatalf("validity = %+v / %+v, want valid_from in UTC and valid_to NULL", f.ValidFrom, f.ValidTo)
	}
}

// TestWriteFactReassertionFillsMissingLinksFirstWins (FIX-MEM-2 phase 0): a
// re-assertion of an existing active triple links it when it had no link yet
// and never overwrites a link, quote or date already set (COALESCE: the first
// value wins); it still adds evidence to the ONE fact.
func TestWriteFactReassertionFillsMissingLinksFirstWins(t *testing.T) {
	e, st := newTestEngine(t)
	ctx := context.Background()
	emre, _, _ := resolve(t, e, "Emre", "person")
	other, _, _ := resolve(t, e, "Başka Emre", "person")

	first := linkedCandidate("Emre", "gold-token")
	id, err := e.WriteFact(ctx, "trace-links", first)
	if err != nil {
		t.Fatal(err)
	}
	second := linkedCandidate("Emre", "gold-token")
	second.SessionID = "sess-links-2"
	second.SubjectEntityID = emre
	second.Quote = "ilk alıntı"
	second.ValidTo = "2026-09-30T00:00:00Z"
	if got, err := e.WriteFact(ctx, "trace-links", second); err != nil || got != id {
		t.Fatalf("re-assertion = %d, %v; want the same fact %d", got, err, id)
	}
	third := linkedCandidate("Emre", "gold-token")
	third.SessionID = "sess-links-3"
	third.SubjectEntityID = other
	third.Quote = "ikinci alıntı"
	third.ValidTo = "2027-01-01T00:00:00Z"
	if _, err := e.WriteFact(ctx, "trace-links", third); err != nil {
		t.Fatal(err)
	}
	f, err := st.Queries.GetFact(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if f.SubjectEntityID.Int64 != emre || f.Quote.String != "ilk alıntı" || f.ValidTo.String != "2026-09-30T00:00:00Z" {
		t.Fatalf("after two re-assertions: subject %+v quote %q valid_to %q; want the first values kept", f.SubjectEntityID, f.Quote.String, f.ValidTo.String)
	}
	if f.ObjectEntityID.Valid {
		t.Fatalf("object link = %+v, want still NULL (nobody linked it)", f.ObjectEntityID)
	}
	if n := countEvidenceRows(t, st, id); n != 3 {
		t.Fatalf("evidence rows = %d, want 3 (one per session)", n)
	}
}

// TestWriteFactWithoutNewFieldsLeavesLinksNull (FIX-MEM-2 phase 0): today's
// call sites (hot-window promotion, ritual answers, red-team) set none of the
// new fields - their facts keep NULL links/quote/validity exactly as before.
func TestWriteFactWithoutNewFieldsLeavesLinksNull(t *testing.T) {
	e, st := newTestEngine(t)
	ctx := context.Background()
	fixed := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	e.SetClock(fixedClock(fixed))
	c := linkedCandidate("episode:7", "kahve")
	id, err := e.WriteFact(ctx, "trace-links", c)
	if err != nil {
		t.Fatal(err)
	}
	c.SessionID = "sess-links-2"
	if _, err := e.WriteFact(ctx, "trace-links", c); err != nil {
		t.Fatal(err)
	}
	f, err := st.Queries.GetFact(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if f.SubjectEntityID.Valid || f.ObjectEntityID.Valid || f.Quote.Valid || f.ValidFrom.Valid || f.ValidTo.Valid {
		t.Fatalf("fact = %+v, want NULL links/quote/validity", f)
	}
}

// TestWriteFactRejectsInvalidLinkFields (FIX-MEM-2 phase 0): the quote is
// free text under the same §5 safety #2 length/charclass cap (300 runes),
// dates must be RFC3339 with valid_from not after valid_to, entity ids are
// never negative - a bad candidate writes nothing.
func TestWriteFactRejectsInvalidLinkFields(t *testing.T) {
	e, st := newTestEngine(t)
	ctx := context.Background()
	for name, mutate := range map[string]func(*Candidate){
		"quote_too_long":       func(c *Candidate) { c.Quote = strings.Repeat("ş", MaxQuoteRunes+1) },
		"quote_control_char":   func(c *Candidate) { c.Quote = "alıntı\x00" },
		"valid_from_not_3339":  func(c *Candidate) { c.ValidFrom = "15.10.2026" },
		"valid_to_not_3339":    func(c *Candidate) { c.ValidTo = "yarın" },
		"from_after_to":        func(c *Candidate) { c.ValidFrom, c.ValidTo = "2026-10-02T00:00:00Z", "2026-10-01T00:00:00Z" },
		"negative_subject_id":  func(c *Candidate) { c.SubjectEntityID = -1 },
		"negative_object_id":   func(c *Candidate) { c.ObjectEntityID = -5 },
		"unknown_entity_id_fk": func(c *Candidate) { c.ObjectEntityID = 424242 },
	} {
		t.Run(name, func(t *testing.T) {
			c := linkedCandidate("Emre", "kahve-"+name)
			mutate(&c)
			if _, err := e.WriteFact(ctx, "trace-links", c); err == nil {
				t.Fatalf("WriteFact accepted %s", name)
			}
			var n int
			if err := st.DB().QueryRow(`SELECT count(*) FROM facts WHERE object = ?`, "kahve-"+name).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Fatalf("a rejected candidate wrote %d fact rows", n)
			}
		})
	}
	ok := linkedCandidate("Emre", "kahve-ok")
	ok.Quote = strings.Repeat("ş", MaxQuoteRunes)
	if _, err := e.WriteFact(ctx, "trace-links", ok); err != nil {
		t.Fatalf("a %d-rune quote must be accepted: %v", MaxQuoteRunes, err)
	}
}
