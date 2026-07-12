package consolidation

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/factengine"
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
