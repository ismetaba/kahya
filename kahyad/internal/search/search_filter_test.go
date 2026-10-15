package search

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"kahya/kahyad/internal/factengine"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/store/sqlcgen"
)

// filterSeed is one chunk of its own episode.
type filterSeed struct {
	path, text, source, tier, createdAt string
}

// seedFiltered inserts one episode + chunk per seed (FTS-indexed) and
// returns the chunk and episode ids in seed order.
func seedFiltered(t *testing.T, st *store.Store, seeds ...filterSeed) (chunks, episodes []int64) {
	t.Helper()
	ctx := context.Background()
	for i, sd := range seeds {
		tx, err := st.DB().BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("BeginTx: %v", err)
		}
		q := st.Queries.WithTx(tx)
		ep, err := q.InsertEpisode(ctx, sqlcgen.InsertEpisodeParams{
			Source: sd.source, SourcePath: sql.NullString{String: sd.path, Valid: true},
			SourceTier: sd.tier, Status: "active", CreatedAt: sd.createdAt,
		})
		if err != nil {
			t.Fatalf("InsertEpisode: %v", err)
		}
		ch, err := q.InsertChunk(ctx, sqlcgen.InsertChunkParams{
			EpisodeID: ep.ID, Seq: 0, Text: sd.text, ContentHash: fmt.Sprintf("hash-%d", i), CreatedAt: sd.createdAt,
		})
		if err != nil {
			t.Fatalf("InsertChunk: %v", err)
		}
		if err := IndexChunk(tx, ch.ID, sd.text); err != nil {
			t.Fatalf("IndexChunk: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		chunks = append(chunks, ch.ID)
		episodes = append(episodes, ep.ID)
	}
	return chunks, episodes
}

func at(days int) string { return daysBefore(recencyNow, days) }

func hitIDs(hits []Hit) []int64 {
	out := []int64{}
	for _, h := range hits {
		out = append(out, h.ChunkID)
	}
	return out
}

func sameSet(a, b []int64) bool {
	m := map[int64]int{}
	for _, x := range a {
		m[x]++
	}
	for _, x := range b {
		m[x]--
	}
	for _, v := range m {
		if v != 0 {
			return false
		}
	}
	return len(a) == len(b)
}

func searchF(t *testing.T, s *Searcher, q string, k int, f Filter) []Hit {
	t.Helper()
	hits, err := s.SearchFiltered(context.Background(), "", q, k, f)
	if err != nil {
		t.Fatalf("SearchFiltered: %v", err)
	}
	return hits
}

func TestSearchFilteredSinceExcludesOlderEpisodes(t *testing.T) {
	st := newTestStore(t)
	ids, _ := seedFiltered(t, st,
		filterSeed{"inbox/eski.md", "Kadıköy ev notu eski", "memory_file", "user_asserted", at(400)},
		filterSeed{"inbox/orta.md", "Kadıköy ev notu orta", "memory_file", "user_asserted", at(30)},
		filterSeed{"inbox/yeni.md", "Kadıköy ev notu yeni", "memory_file", "user_asserted", at(1)},
	)
	s := newTestSearcher(t, st)
	got := searchF(t, s, "Kadıköy ev", 8, Filter{Since: recencyNow.Add(-60 * 24 * time.Hour)})
	if !sameSet(hitIDs(got), ids[1:]) {
		t.Fatalf("since 60 days = %v, want %v", hitIDs(got), ids[1:])
	}
	got = searchF(t, s, "Kadıköy ev", 8, Filter{Until: recencyNow.Add(-2 * 24 * time.Hour)})
	if !sameSet(hitIDs(got), ids[:2]) {
		t.Fatalf("until 2 days ago = %v, want %v", hitIDs(got), ids[:2])
	}
	// Since inclusive, Until exclusive.
	exact, _ := time.Parse(time.RFC3339Nano, at(30))
	got = searchF(t, s, "Kadıköy ev", 8, Filter{Since: exact, Until: exact.Add(time.Nanosecond)})
	if !reflect.DeepEqual(hitIDs(got), []int64{ids[1]}) {
		t.Fatalf("[t, t+1ns) = %v, want [%d]", hitIDs(got), ids[1])
	}
}

func TestSearchFilteredPathPrefixAndSource(t *testing.T) {
	st := newTestStore(t)
	ids, _ := seedFiltered(t, st,
		filterSeed{"people/emre.md", "Emre toplantı notu", "memory_file", "user_asserted", at(3)},
		filterSeed{"people/ayse.md", "Ayşe toplantı notu", "memory_file", "user_asserted", at(3)},
		filterSeed{"projects/kahya.md", "Kahya toplantı notu", "memory_file", "user_asserted", at(3)},
		filterSeed{"ingest/rapor.md", "rapor toplantı notu", "external_doc", "external_doc", at(3)},
	)
	s := newTestSearcher(t, st)
	if got := searchF(t, s, "toplantı", 8, Filter{PathPrefix: "people/"}); !sameSet(hitIDs(got), ids[:2]) {
		t.Fatalf("path_prefix people/ = %v, want %v", hitIDs(got), ids[:2])
	}
	if got := searchF(t, s, "toplantı", 8, Filter{PathPrefix: "People/"}); len(got) != 0 {
		t.Fatalf("path_prefix is byte-exact, got %v", hitIDs(got))
	}
	got := searchF(t, s, "toplantı", 8, Filter{Source: "external_doc"})
	if !reflect.DeepEqual(hitIDs(got), []int64{ids[3]}) || got[0].Source != "external_doc" {
		t.Fatalf("source external_doc = %+v, want only %d", got, ids[3])
	}
}

// TestSearchFilteredEntityIDOnlyCitedEpisodes: entity_id keeps only the
// chunks of episodes an ACTIVE fact touching that entity (through a
// cloud-lane link) cites.
func TestSearchFilteredEntityIDOnlyCitedEpisodes(t *testing.T) {
	st := newTestStore(t)
	ids, eps := seedFiltered(t, st,
		filterSeed{"people/emre.md", "Emre proje notu", "memory_file", "user_asserted", at(3)},
		filterSeed{"people/ayse.md", "Ayşe proje notu", "memory_file", "user_asserted", at(3)},
		filterSeed{"people/eski.md", "Emre eski proje notu", "memory_file", "user_asserted", at(3)},
	)
	db := st.DB()
	exec := func(q string, args ...any) int64 {
		t.Helper()
		res, err := db.Exec(q, args...)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	now := at(0)
	emre := exec(`INSERT INTO entities (canonical_name, kind, status, created_at) VALUES ('Emre', 'person', 'active', ?)`, now)
	ayse := exec(`INSERT INTO entities (canonical_name, kind, status, created_at) VALUES ('Ayşe', 'person', 'active', ?)`, now)
	fact := func(subjID int64, status string, ep int64) {
		id := exec(`INSERT INTO facts (subject, predicate, object, source_tier, evidentiality, confidence, status, updated_at, created_at, subject_entity_id, subject_link_cloud)
			VALUES ('x', 'works_on', 'proje', 'user_asserted', 'witnessed', 2, ?, ?, ?, ?, 1)`, status, now, now, subjID)
		exec(`INSERT INTO evidence (fact_id, episode_id, polarity, weight, created_at) VALUES (?, ?, 1, 2, ?)`, id, ep, now)
	}
	fact(emre, "active", eps[0])
	fact(ayse, "active", eps[1])
	fact(emre, "retracted", eps[2]) // an inactive fact cites nothing

	s := newTestSearcher(t, st)
	if got := searchF(t, s, "proje", 8, Filter{EntityID: emre}); !reflect.DeepEqual(hitIDs(got), []int64{ids[0]}) {
		t.Fatalf("entity %d = %v, want [%d]", emre, hitIDs(got), ids[0])
	}
	if got := searchF(t, s, "proje", 8, Filter{EntityID: 999}); len(got) != 0 {
		t.Fatalf("unknown entity = %v, want none", hitIDs(got))
	}
}

// TestSearchFilteredEntityIDFollowsOnlyCloudLaneLinks (FIX-MEM-1/MEM-4
// review, migration 0025): the memory_search entity_id filter answers a
// worker - a cloud reader - so it follows only a link a cloud-lane
// extraction call wrote. A link a local-lane call wrote (it may have
// resolved "Doktor" through an alias only a secret note gives Ayşe Kaya)
// never returns that note's chunks for Ayşe Kaya's entity, on either side.
func TestSearchFilteredEntityIDFollowsOnlyCloudLaneLinks(t *testing.T) {
	st := newTestStore(t)
	ids, eps := seedFiltered(t, st,
		filterSeed{"people/ayse.md", "Ayşe Kaya Kadıköy notu", "memory_file", "user_asserted", at(3)},
		filterSeed{"inbox/2026-07-10.md", "Doktor ile kahve notu", "memory_file", "user_asserted", at(3)},
		filterSeed{"inbox/2026-07-11.md", "Kullanıcı Doktor ziyareti notu", "memory_file", "user_asserted", at(3)},
	)
	db := st.DB()
	exec := func(q string, args ...any) int64 {
		t.Helper()
		res, err := db.Exec(q, args...)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	now := at(0)
	ayse := exec(`INSERT INTO entities (canonical_name, kind, status, created_at) VALUES ('Ayşe Kaya', 'person', 'active', ?)`, now)
	cite := func(factID, ep int64) {
		exec(`INSERT INTO evidence (fact_id, episode_id, polarity, weight, created_at) VALUES (?, ?, 1, 2, ?)`, factID, ep, now)
	}
	// Cloud-lane subject link: Ayşe Kaya lives_in Kadıköy (people/ayse.md).
	cite(exec(`INSERT INTO facts (subject, predicate, object, source_tier, evidentiality, confidence, status, updated_at, created_at, subject_entity_id, subject_link_cloud)
		VALUES ('Ayşe Kaya', 'lives_in', 'Kadıköy', 'user_asserted', 'witnessed', 2, 'active', ?, ?, ?, 1)`, now, now, ayse), eps[0])
	// Local-lane links: "Doktor" -> Ayşe Kaya, as subject and as object.
	cite(exec(`INSERT INTO facts (subject, predicate, object, source_tier, evidentiality, confidence, status, updated_at, created_at, subject_entity_id, subject_link_cloud)
		VALUES ('Doktor', 'drinks', 'kahve', 'user_asserted', 'witnessed', 2, 'active', ?, ?, ?, 0)`, now, now, ayse), eps[1])
	cite(exec(`INSERT INTO facts (subject, predicate, object, source_tier, evidentiality, confidence, status, updated_at, created_at, object_entity_id, object_link_cloud)
		VALUES ('Kullanıcı', 'visited', 'Doktor', 'user_asserted', 'witnessed', 2, 'active', ?, ?, ?, 0)`, now, now, ayse), eps[2])

	s := newTestSearcher(t, st)
	if got := searchF(t, s, "notu", 8, Filter{EntityID: ayse}); !reflect.DeepEqual(hitIDs(got), []int64{ids[0]}) {
		t.Fatalf("entity %d = %v, want only the cloud-linked note [%d] (local-lane links followed)", ayse, hitIDs(got), ids[0])
	}
}

func TestSearchFilteredMinTierDropsAgentDerived(t *testing.T) {
	st := newTestStore(t)
	ids, _ := seedFiltered(t, st,
		filterSeed{"a.md", "kira sözleşmesi kullanıcı", "memory_file", "user_asserted", at(3)},
		filterSeed{"b.md", "kira sözleşmesi ajan", "memory_file", "agent_derived", at(3)},
		filterSeed{"c.md", "kira sözleşmesi belge", "external_doc", "external_doc", at(3)},
		filterSeed{"d.md", "kira sözleşmesi ekran", "memory_file", "screen", at(3)},
	)
	s := newTestSearcher(t, st)
	if got := searchF(t, s, "kira sözleşmesi", 8, Filter{MinTier: "screen"}); !sameSet(hitIDs(got), []int64{ids[0], ids[2], ids[3]}) {
		t.Fatalf("min_tier screen = %v, want every tier but agent_derived", hitIDs(got))
	}
	if got := searchF(t, s, "kira sözleşmesi", 8, Filter{MinTier: "user_asserted"}); !reflect.DeepEqual(hitIDs(got), []int64{ids[0]}) {
		t.Fatalf("min_tier user_asserted = %v, want [%d]", hitIDs(got), ids[0])
	}
	if got := searchF(t, s, "kira sözleşmesi", 8, Filter{MinTier: "agent_derived"}); len(got) != 4 {
		t.Fatalf("min_tier agent_derived = %v, want all 4", hitIDs(got))
	}
	if _, err := s.SearchFiltered(context.Background(), "", "kira", 8, Filter{MinTier: "trusted"}); !errors.Is(err, ErrUnknownTier) {
		t.Fatalf("unknown min_tier err = %v, want ErrUnknownTier", err)
	}
}

// TestSearchFilteredZeroFilterEqualsSearch: Search is SearchFiltered with a
// zero Filter - same hits, same order, same scores.
func TestSearchFilteredZeroFilterEqualsSearch(t *testing.T) {
	st := newTestStore(t)
	seedFixtureChunks(t, st)
	seedFiltered(t, st,
		filterSeed{"x.md", "evlerimizden biri Kadıköy'de", "memory_file", "user_asserted", at(10)},
		filterSeed{"y.md", "ev bakmaya devam", "memory_file", "agent_derived", at(100)},
	)
	s := newTestSearcher(t, st)
	for _, q := range []string{"ev", "evlerimizden", "Kadıköy"} {
		plain, err := s.Search(context.Background(), "", q, 8)
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		zero := searchF(t, s, q, 8, Filter{})
		if !reflect.DeepEqual(plain, zero) {
			t.Fatalf("q=%q: Search %+v != SearchFiltered(zero) %+v", q, plain, zero)
		}
	}
}

// TestSearchFilteredKeepsKAfterFilter: the filter runs before the k cut, so
// with 3 filtered-out chunks outranking the rest, k results still come back.
func TestSearchFilteredKeepsKAfterFilter(t *testing.T) {
	st := newTestStore(t)
	var seeds []filterSeed
	for i := 0; i < 3; i++ {
		seeds = append(seeds, filterSeed{fmt.Sprintf("ajan/%d.md", i), "bütçe bütçe bütçe planı ajan", "memory_file", "agent_derived", at(1)})
	}
	for i := 0; i < 5; i++ {
		seeds = append(seeds, filterSeed{fmt.Sprintf("not/%d.md", i), fmt.Sprintf("bütçe planı not %d", i), "memory_file", "user_asserted", at(1)})
	}
	ids, _ := seedFiltered(t, st, seeds...)
	s := newTestSearcher(t, st)
	unfiltered := searchF(t, s, "bütçe", 3, Filter{})
	for _, h := range unfiltered {
		if h.SourceTier != "agent_derived" {
			t.Fatalf("fixture: the agent_derived chunks must outrank the rest, got %+v", unfiltered)
		}
	}
	got := searchF(t, s, "bütçe", 3, Filter{MinTier: "user_asserted"})
	if len(got) != 3 {
		t.Fatalf("k=3 with the filter = %v, want 3 hits", hitIDs(got))
	}
	for _, h := range got {
		if h.SourceTier != "user_asserted" {
			t.Fatalf("a filtered-out chunk came back: %+v", h)
		}
	}
	_ = ids
}

// TestSearchTierRankMatchesFactengine: memory_search's min_tier lattice is
// factengine's (HANDOFF §5 memory #1) - one order, never two.
func TestSearchTierRankMatchesFactengine(t *testing.T) {
	for tier, r := range tierRank {
		if want, ok := factengine.TierRank(tier); !ok || want != r {
			t.Errorf("tier %q: search rank %d, factengine rank %d (ok %v)", tier, r, want, ok)
		}
	}
	for _, tier := range []string{factengine.TierUserEdit, factengine.TierUserAsserted, factengine.TierExternalDoc, factengine.TierScreen, factengine.TierAgentDerived} {
		if _, ok := tierRank[tier]; !ok {
			t.Errorf("tier %q missing from search's lattice", tier)
		}
	}
}
