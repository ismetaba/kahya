package search

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"testing"
	"time"

	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/store/sqlcgen"
)

// recencyNow is the fixed clock the recency tests run at.
var recencyNow = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

// seedChunkAt is seedChunk with an explicit episodes.created_at (the date
// FIX-MEM-8's recency weight ages a chunk by).
func seedChunkAt(t *testing.T, st *store.Store, path, text, createdAt string) int64 {
	t.Helper()
	ctx := context.Background()
	tx, err := st.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer tx.Rollback()
	q := st.Queries.WithTx(tx)
	ep, err := q.InsertEpisode(ctx, sqlcgen.InsertEpisodeParams{
		Source:     "test",
		SourcePath: sql.NullString{String: path, Valid: true},
		SourceTier: "user_asserted",
		Status:     "active",
		CreatedAt:  createdAt,
	})
	if err != nil {
		t.Fatalf("InsertEpisode: %v", err)
	}
	ch, err := q.InsertChunk(ctx, sqlcgen.InsertChunkParams{
		EpisodeID: ep.ID, Seq: 0, Text: text, ContentHash: "hash-" + path, CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("InsertChunk: %v", err)
	}
	if err := IndexChunk(tx, ch.ID, text); err != nil {
		t.Fatalf("IndexChunk: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return ch.ID
}

func daysBefore(now time.Time, days int) string {
	return now.Add(-time.Duration(days) * 24 * time.Hour).Format(time.RFC3339Nano)
}

// recencySearcher returns a searcher over st with cfg and the fixed clock.
func recencySearcher(t *testing.T, st *store.Store, cfg Config) *Searcher {
	t.Helper()
	s := newTestSearcher(t, st)
	s.cfg = cfg
	s.now = func() time.Time { return recencyNow }
	return s
}

func withRecency(w float64, halfLife int) Config {
	c := DefaultConfig()
	c.RecencyWeight = w
	c.RecencyHalfLifeDays = halfLife
	return c
}

// TestSearchRecencyWeightZeroIsIdentity (FIX-MEM-8): weight 0 (the default)
// leaves Search's order and scores exactly as they were - whatever the clock
// and the half-life say.
func TestSearchRecencyWeightZeroIsIdentity(t *testing.T) {
	st := newTestStore(t)
	seedChunkAt(t, st, "eski.md", "Kadıköy'de ev bakıyoruz, eski not.", daysBefore(recencyNow, 400))
	seedChunkAt(t, st, "yeni.md", "Kadıköy'de ev bakıyoruz, yeni not.", daysBefore(recencyNow, 1))
	seedChunkAt(t, st, "orta.md", "Moda'da ev de olabilir.", daysBefore(recencyNow, 90))

	base := newTestSearcher(t, st) // DefaultConfig, real clock
	want, err := base.Search(context.Background(), "", "ev Kadıköy", 8)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(want) < 2 {
		t.Fatalf("fixture returned %d hits, want >= 2", len(want))
	}
	for _, cfg := range []Config{DefaultConfig(), withRecency(0, 1), withRecency(0, 9999)} {
		got, err := recencySearcher(t, st, cfg).Search(context.Background(), "", "ev Kadıköy", 8)
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if len(got) != len(want) {
			t.Fatalf("len = %d, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i].ChunkID != want[i].ChunkID || got[i].Score != want[i].Score {
				t.Fatalf("hit %d = (%d, %v), want (%d, %v) - weight 0 must be the identity",
					i, got[i].ChunkID, got[i].Score, want[i].ChunkID, want[i].Score)
			}
		}
	}
}

// TestSearchRecencyWeightPrefersNewerOnTie (FIX-MEM-8): two chunks with the
// same text tie on the fused score; without recency the tie-break (chunks.id
// desc) puts the older one first, with weight 0.5 the newer one wins.
func TestSearchRecencyWeightPrefersNewerOnTie(t *testing.T) {
	st := newTestStore(t)
	const text = "Kadıköy'de iki daire gezdik."
	newer := seedChunkAt(t, st, "yeni.md", text, daysBefore(recencyNow, 2))
	older := seedChunkAt(t, st, "eski.md", text, daysBefore(recencyNow, 500))

	off, err := recencySearcher(t, st, DefaultConfig()).Search(context.Background(), "", "daire", 8)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(off) != 2 || off[0].ChunkID != older || off[0].Score != off[1].Score {
		t.Fatalf("recency off = %+v, want the tie broken by id desc (older chunk %d first)", off, older)
	}

	on, err := recencySearcher(t, st, withRecency(0.5, 90)).Search(context.Background(), "", "daire", 8)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(on) != 2 || on[0].ChunkID != newer {
		t.Fatalf("recency on = %+v, want the newer chunk %d first", on, newer)
	}
	if !(on[0].Score > on[1].Score) {
		t.Fatalf("scores = %v, %v; the newer chunk must score strictly higher", on[0].Score, on[1].Score)
	}
	if on[0].EpisodeCreatedAt != daysBefore(recencyNow, 2) {
		t.Fatalf("EpisodeCreatedAt = %q, want the episode's created_at", on[0].EpisodeCreatedAt)
	}
}

// TestSearchRecencyHalfLife (FIX-MEM-8): the decay is exp(-ln2*age/halfLife)
// - 1 at age 0, 0.5 at one half-life, 0.25 at two - and the blended score is
// (1-w)*fused + w*decay.
func TestSearchRecencyHalfLife(t *testing.T) {
	for _, tc := range []struct {
		age  int
		want float64
	}{{0, 1}, {90, 0.5}, {180, 0.25}} {
		got := recencyFactor(daysBefore(recencyNow, tc.age), recencyNow, 90)
		if math.Abs(got-tc.want) > 1e-12 {
			t.Errorf("recencyFactor(age %d days, half-life 90) = %v, want %v", tc.age, got, tc.want)
		}
	}
	// A future created_at (clock skew) is age 0, never a boost above 1.
	if got := recencyFactor(recencyNow.Add(48*time.Hour).Format(time.RFC3339Nano), recencyNow, 90); got != 1 {
		t.Errorf("future created_at = %v, want 1", got)
	}

	st := newTestStore(t)
	seedChunkAt(t, st, "not.md", "Kadıköy'de iki daire gezdik.", daysBefore(recencyNow, 90))
	off, err := recencySearcher(t, st, DefaultConfig()).Search(context.Background(), "", "daire", 8)
	if err != nil || len(off) != 1 {
		t.Fatalf("Search off = %+v, %v", off, err)
	}
	on, err := recencySearcher(t, st, withRecency(0.5, 90)).Search(context.Background(), "", "daire", 8)
	if err != nil || len(on) != 1 {
		t.Fatalf("Search on = %+v, %v", on, err)
	}
	if want := 0.5*off[0].Score + 0.5*0.5; math.Abs(on[0].Score-want) > 1e-12 {
		t.Fatalf("blended score = %v, want 0.5*%v + 0.5*0.5 = %v", on[0].Score, off[0].Score, want)
	}
}

// TestSearchRecencyUnparseableCreatedAtIsZeroRecency (FIX-MEM-8): a
// created_at that does not parse as RFC3339 gets no recency (age treated as
// infinite), never a boost.
func TestSearchRecencyUnparseableCreatedAtIsZeroRecency(t *testing.T) {
	if got := recencyFactor("10/07/2026", recencyNow, 90); got != 0 {
		t.Fatalf("recencyFactor(unparseable) = %v, want 0", got)
	}
	st := newTestStore(t)
	bad := seedChunkAt(t, st, "bozuk.md", "Kadıköy'de iki daire gezdik.", "dün")
	good := seedChunkAt(t, st, "iyi.md", "Kadıköy'de iki daire gezdik.", daysBefore(recencyNow, 400))
	off, err := recencySearcher(t, st, DefaultConfig()).Search(context.Background(), "", "daire", 8)
	if err != nil || len(off) != 2 {
		t.Fatalf("Search off = %+v, %v", off, err)
	}
	on, err := recencySearcher(t, st, withRecency(0.5, 90)).Search(context.Background(), "", "daire", 8)
	if err != nil || len(on) != 2 {
		t.Fatalf("Search on = %+v, %v", on, err)
	}
	if on[0].ChunkID != good {
		t.Fatalf("order = %+v, want the parseable (even if old) chunk %d first", on, good)
	}
	for _, h := range on {
		if h.ChunkID == bad && math.Abs(h.Score-0.5*off[0].Score) > 1e-12 {
			t.Fatalf("unparseable chunk score = %v, want (1-w)*fused = %v", h.Score, 0.5*off[0].Score)
		}
	}
}

// TestSearchRecencyAppliesWhenVecLegDegrades (FIX-MEM-8 / EK-MEM-C): a
// vector-leg degrade swaps Search's LOCAL config for FTSOnlyConfig; the
// recency weight is read from the declared s.cfg, so an embed outage (no
// embedder, or a failing one) never silently turns recency off.
func TestSearchRecencyAppliesWhenVecLegDegrades(t *testing.T) {
	st := newTestStore(t)
	const text = "Kadıköy'de iki daire gezdik."
	newer := seedChunkAt(t, st, "yeni.md", text, daysBefore(recencyNow, 2))
	seedChunkAt(t, st, "eski.md", text, daysBefore(recencyNow, 500))

	for name, embed := range map[string]Embedder{
		"no_embedder":      nil,
		"embed_call_fails": &fakeEmbedder{err: errors.New("embed service down")},
	} {
		t.Run(name, func(t *testing.T) {
			s := recencySearcher(t, st, withRecency(0.5, 90))
			if embed != nil {
				s.SetEmbedder(embed, testActiveModelVer)
			}
			hits, err := s.Search(context.Background(), "", "daire", 8)
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if len(hits) != 2 || hits[0].ChunkID != newer {
				t.Fatalf("degraded search = %+v, want recency still applied (newer chunk %d first)", hits, newer)
			}
		})
	}
}
