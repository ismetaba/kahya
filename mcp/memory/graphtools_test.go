package memory

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeGraphReader struct {
	lastName string
	lastK    int
	lastQ    FactQuery
	facts    []FactRecord
	ents     []EntityRecord
	err      error
}

func (g *fakeGraphReader) LookupEntities(_ context.Context, _, name string, k int) ([]EntityRecord, error) {
	g.lastName, g.lastK = name, k
	return g.ents, g.err
}

func (g *fakeGraphReader) QueryFacts(_ context.Context, _ string, q FactQuery) ([]FactRecord, error) {
	g.lastQ = q
	return g.facts, g.err
}

// TestGraphNilIsTurkishError (FIX-MEM-4): with no graph wired both tools
// answer the byte-exact Turkish error.
func TestGraphNilIsTurkishError(t *testing.T) {
	s := New(t.TempDir(), &fakeSearcher{}, &fakeIndexer{}, nil, nil)
	_, err1 := s.HandleEntityLookup(context.Background(), "tr", EntityLookupArgs{Name: "Emre"})
	_, err2 := s.HandleFactQuery(context.Background(), "tr", FactQueryArgs{Predicate: "works_at"})
	for _, err := range []error{err1, err2} {
		if !errors.Is(err, ErrGraphUnavailable) || err.Error() != "graf katmanı bu kurulumda kullanılamıyor" {
			t.Fatalf("err = %v, want ErrGraphUnavailable", err)
		}
	}
}

// TestFactQueryArgsParsedAndClamped: the four times are parsed RFC3339,
// strings trimmed, k defaulted to 20 and capped at 100; an empty answer is
// an empty list, never null.
func TestFactQueryArgsParsedAndClamped(t *testing.T) {
	g := &fakeGraphReader{}
	s := New(t.TempDir(), &fakeSearcher{}, &fakeIndexer{}, nil, nil)
	s.SetGraph(g)
	out, err := s.HandleFactQuery(context.Background(), "tr", FactQueryArgs{
		Subject: " Emre ", Predicate: "works_at", EntityID: 4, Since: "2026-01-01T00:00:00Z", Until: "2026-06-01T00:00:00+03:00",
		AsOf: "2026-03-01T12:00:00Z", KnownAsOf: "2026-05-01T00:00:00Z", MinTier: "user_asserted", IncludeQuarantined: true,
	})
	if err != nil {
		t.Fatalf("HandleFactQuery: %v", err)
	}
	q := g.lastQ
	if q.Subject != "Emre" || q.Predicate != "works_at" || q.EntityID != 4 || q.MinTier != "user_asserted" || !q.IncludeQuarantined || q.K != DefaultFactQueryK {
		t.Fatalf("query = %+v", q)
	}
	if !q.AsOf.Equal(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)) || !q.Until.Equal(time.Date(2026, 5, 31, 21, 0, 0, 0, time.UTC)) || q.Since.IsZero() || q.KnownAsOf.IsZero() {
		t.Fatalf("times = %+v", q)
	}
	if out.Facts == nil {
		t.Fatalf("an empty answer must be [] (got nil)")
	}
	if _, err := s.HandleFactQuery(context.Background(), "tr", FactQueryArgs{K: 1000}); err != nil || g.lastQ.K != MaxFactQueryK {
		t.Fatalf("k 1000 -> %d (err %v), want %d", g.lastQ.K, err, MaxFactQueryK)
	}
	for _, bad := range []FactQueryArgs{{AsOf: "2026-03-01"}, {KnownAsOf: "dün"}, {Since: "x"}, {Until: "2026"}} {
		if _, err := s.HandleFactQuery(context.Background(), "tr", bad); !errors.Is(err, ErrFactQueryBadTime) {
			t.Fatalf("args %+v: err = %v, want ErrFactQueryBadTime", bad, err)
		}
	}
}

func TestEntityLookupArgsValidatedAndClamped(t *testing.T) {
	g := &fakeGraphReader{}
	s := New(t.TempDir(), &fakeSearcher{}, &fakeIndexer{}, nil, nil)
	s.SetGraph(g)
	if _, err := s.HandleEntityLookup(context.Background(), "tr", EntityLookupArgs{Name: "  "}); err == nil {
		t.Fatalf("an empty name must be refused")
	}
	out, err := s.HandleEntityLookup(context.Background(), "tr", EntityLookupArgs{Name: " Emre ", K: 500})
	if err != nil || g.lastName != "Emre" || g.lastK != MaxEntityLookupK || out.Entities == nil {
		t.Fatalf("lookup -> name %q k %d out %+v err %v", g.lastName, g.lastK, out, err)
	}
	if _, err := s.HandleEntityLookup(context.Background(), "tr", EntityLookupArgs{Name: "Emre"}); err != nil || g.lastK != DefaultEntityLookupK {
		t.Fatalf("default k = %d, want %d", g.lastK, DefaultEntityLookupK)
	}
}

// TestFactQueryLabelsEndedFactsSonaErdi (FIX-FINAL fact-history-ended-vs-
// corrected): an ended fact always carries the byte-exact Turkish label
// "sona erdi"; any other fact carries none, whatever the reader set.
func TestFactQueryLabelsEndedFactsSonaErdi(t *testing.T) {
	if FactEndedLabel != "sona erdi" {
		t.Fatalf("FactEndedLabel = %q, want the byte-exact \"sona erdi\"", FactEndedLabel)
	}
	g := &fakeGraphReader{facts: []FactRecord{{FactID: 1}, {FactID: 2, Ended: true, RetractedAt: "2026-08-01T00:00:00Z"}, {FactID: 3, Label: "stale"}}}
	s := New(t.TempDir(), &fakeSearcher{}, &fakeIndexer{}, nil, nil)
	s.SetGraph(g)
	out, err := s.HandleFactQuery(context.Background(), "tr", FactQueryArgs{AsOf: "2026-06-01T00:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[int64]string{1: "", 2: "sona erdi", 3: ""}
	for _, f := range out.Facts {
		if f.Label != want[f.FactID] {
			t.Errorf("fact %d label = %q, want %q", f.FactID, f.Label, want[f.FactID])
		}
	}
}

// TestFactQueryToolDescriptionStatesTheHistoryRule: the tool description
// keeps the FIX-MEM-4 text and states the history rule byte-exact.
func TestFactQueryToolDescriptionStatesTheHistoryRule(t *testing.T) {
	const want = "Yapılandırılmış olguları (özne-yüklem-nesne) varlık, yüklem ve zaman (as_of / known_as_of / since / until) filtresiyle sorgular; onaysız ajan çıkarımları yalnız include_quarantined=true ile ve 'quarantined' etiketiyle döner. as_of / known_as_of ile o anda geçerli olup sonradan sona ermiş olgular da 'sona erdi' etiketiyle döner; yanlış olduğu için düzeltilmiş olgular hiç dönmez."
	if FactQueryToolDescription != want {
		t.Fatalf("FactQueryToolDescription = %q, want %q", FactQueryToolDescription, want)
	}
}
