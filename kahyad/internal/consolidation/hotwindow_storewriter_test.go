package consolidation

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/factengine"
	"kahya/kahyad/internal/ritual"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/store/sqlcgen"
	"kahya/kahyad/internal/taint"
)

// TestStoreFactWriterAgainstRealBrainDB is a real-SQLite integration test
// (a temp brain.db, migrated by store.Open - the SAME hermetic convention
// kahyad/internal/anchor/helpers_test.go and kahyad/internal/indexer's own
// tests already use) proving StoreFactWriter's SQL wiring actually works:
// an episode seeded 91 days old gets its chunk's detail atoms promoted to
// real facts rows (source_tier=agent_derived) and its cooled_at column
// stamped - never a fake standing in for the schema itself.
func TestStoreFactWriterAgainstRealBrainDB(t *testing.T) {
	st, err := store.Open(config.Config{DBPath: filepath.Join(t.TempDir(), "brain.db")})
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	createdAt := now.AddDate(0, 0, -91).Format(time.RFC3339)

	ep, err := st.Queries.InsertEpisode(ctx, sqlcgen.InsertEpisodeParams{
		Source:     "memory_file",
		SourcePath: sql.NullString{String: "eski-not.md", Valid: true},
		SourceHash: sql.NullString{String: "deadbeef", Valid: true},
		SourceTier: "user_asserted",
		Status:     "active",
		CreatedAt:  createdAt,
	})
	if err != nil {
		t.Fatalf("InsertEpisode() error = %v", err)
	}
	if _, err := st.Queries.InsertChunk(ctx, sqlcgen.InsertChunkParams{
		EpisodeID:   ep.ID,
		Seq:         0,
		Text:        "Karar verdim ki 750 TL 20.01.2026 tarihinde odenecek.",
		ContentHash: "chunkhash",
		CreatedAt:   createdAt,
	}); err != nil {
		t.Fatalf("InsertChunk() error = %v", err)
	}

	engine := factengine.New(st.Queries, taint.New(st.Queries, st), st)
	writer := StoreFactWriter{Q: st.Queries, Engine: engine}
	promoted, err := PromoteHotWindow(ctx, writer, "trace-test", now)
	if err != nil {
		t.Fatalf("PromoteHotWindow() error = %v", err)
	}
	if promoted == 0 {
		t.Fatal("PromoteHotWindow() promoted 0 facts against the real store")
	}

	rows, err := st.Queries.ListUncooledEpisodesOlderThan(ctx, now.Format(time.RFC3339))
	if err != nil {
		t.Fatalf("ListUncooledEpisodesOlderThan() error = %v", err)
	}
	for _, r := range rows {
		if r.ID == ep.ID {
			t.Fatalf("episode %d still reports uncooled after PromoteHotWindow", ep.ID)
		}
	}
}

// TestHotWindowFactsReachTheTruthRitual is the end-to-end regression test
// for the hot-window -> evidence -> W5-03 ritual seam: every promoted fact
// must cite its source episode in an evidence row, otherwise the ritual
// sampler's fail-closed classify (kahyad/internal/ritual/select.go) can
// never select it and the weekly truth ritual has nothing to ask (hot-window
// promotion is the only production source of new facts). It also pins the
// other half of that rule: a fact promoted from a secret-lane episode, now
// classifiable, is still excluded.
func TestHotWindowFactsReachTheTruthRitual(t *testing.T) {
	st, err := store.Open(config.Config{DBPath: filepath.Join(t.TempDir(), "brain.db")})
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	createdAt := now.AddDate(0, 0, -91).Format(time.RFC3339)

	seed := func(relPath string) int64 {
		t.Helper()
		ep, err := st.Queries.InsertEpisode(ctx, sqlcgen.InsertEpisodeParams{
			Source:     "memory_file",
			SourcePath: sql.NullString{String: relPath, Valid: true},
			SourceHash: sql.NullString{String: "hash-" + relPath, Valid: true},
			SourceTier: "user_asserted",
			Status:     "active",
			CreatedAt:  createdAt,
		})
		if err != nil {
			t.Fatalf("InsertEpisode(%s) error = %v", relPath, err)
		}
		if _, err := st.Queries.InsertChunk(ctx, sqlcgen.InsertChunkParams{
			EpisodeID:   ep.ID,
			Seq:         0,
			Text:        "Karar verdim ki 750 TL 20.01.2026 tarihinde odenecek.",
			ContentHash: "chunk-" + relPath,
			CreatedAt:   createdAt,
		}); err != nil {
			t.Fatalf("InsertChunk(%s) error = %v", relPath, err)
		}
		return ep.ID
	}
	plainEp := seed("notlar/eski-not.md")
	secretEp := seed("gizli/maas.md")

	engine := factengine.New(st.Queries, taint.New(st.Queries, st), st)
	if _, err := PromoteHotWindow(ctx, StoreFactWriter{Q: st.Queries, Engine: engine}, "trace-test", now); err != nil {
		t.Fatalf("PromoteHotWindow() error = %v", err)
	}

	var uncited int
	if err := st.DB().QueryRow(`SELECT count(*) FROM evidence WHERE episode_id IS NULL`).Scan(&uncited); err != nil {
		t.Fatalf("count uncited evidence rows: %v", err)
	}
	if uncited != 0 {
		t.Fatalf("%d evidence row(s) have episode_id NULL; every hot-window fact must cite its source episode", uncited)
	}
	for _, epID := range []int64{plainEp, secretEp} {
		var n int
		if err := st.DB().QueryRow(`SELECT count(*) FROM evidence WHERE episode_id = ?`, epID).Scan(&n); err != nil {
			t.Fatalf("count evidence rows for episode %d: %v", epID, err)
		}
		if n == 0 {
			t.Fatalf("no evidence row cites episode %d", epID)
		}
	}

	const memoryDir = "/home/test/Kahya/memory"
	sampler := ritual.NewSampler(st.Queries, memoryDir, []string{memoryDir + "/gizli/**"}, nil)
	got, err := sampler.Select(ctx)
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	plainSubject := fmt.Sprintf("episode:%d", plainEp)
	secretSubject := fmt.Sprintf("episode:%d", secretEp)
	selectedPlain := false
	for _, f := range got {
		switch f.Subject {
		case plainSubject:
			selectedPlain = true
		case secretSubject:
			t.Fatalf("Select() picked fact %d, promoted from the secret-lane episode %d", f.ID, secretEp)
		}
	}
	if !selectedPlain {
		t.Fatalf("Select() picked none of the facts promoted from episode %d (selected %d facts in total)", plainEp, len(got))
	}
}
