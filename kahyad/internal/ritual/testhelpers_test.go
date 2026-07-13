package ritual

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/store/sqlcgen"
	"kahya/kahyad/internal/taint"
)

// newTestStore builds a *store.Store against a real temp-file brain.db -
// migrations' real CHECK constraints/column types are exercised for real,
// mirroring kahyad/internal/factengine/testhelpers_test.go's own
// newTestStore rationale.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	cfg := config.Config{DBPath: filepath.Join(t.TempDir(), "brain.db")}
	st, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func testLogger(t *testing.T) *logx.Logger {
	t.Helper()
	log, err := logx.New(t.TempDir(), "boot0123456789abcdef0123456789ab")
	if err != nil {
		t.Fatalf("logx.New: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	return log
}

func nowStr() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// seedEpisode inserts a memory_file episode at relPath (relative to some
// memoryDir a test's own Sampler is constructed with), returning its id.
func seedEpisode(t *testing.T, st *store.Store, relPath string) int64 {
	t.Helper()
	ep, err := st.Queries.InsertEpisode(context.Background(), sqlcgen.InsertEpisodeParams{
		Source:     "memory_file",
		SourcePath: sql.NullString{String: relPath, Valid: true},
		SourceHash: sql.NullString{String: "hash", Valid: true},
		SourceTier: "user_asserted",
		Status:     "active",
		CreatedAt:  nowStr(),
	})
	if err != nil {
		t.Fatalf("InsertEpisode(%s): %v", relPath, err)
	}
	return ep.ID
}

// seedFact inserts an active fact directly (bypassing factengine, so
// tests can control source_tier/confidence/confirmed independently of any
// evidence-derived cap), returning the full row.
func seedFact(t *testing.T, st *store.Store, subject, predicate, object, sourceTier string, confidence float64, confirmed bool) sqlcgen.Fact {
	t.Helper()
	now := nowStr()
	f, err := st.Queries.InsertFact(context.Background(), sqlcgen.InsertFactParams{
		Subject: subject, Predicate: predicate, Object: object,
		SourceTier: sourceTier, Evidentiality: "witnessed", Confidence: confidence,
		Importance: 0, Status: "active", UpdatedAt: now, CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("InsertFact(%s/%s/%s): %v", subject, predicate, object, err)
	}
	if confirmed {
		if err := st.Queries.ConfirmFact(context.Background(), sqlcgen.ConfirmFactParams{
			ConfirmedAt: sql.NullString{String: now, Valid: true}, UpdatedAt: now, ID: f.ID,
		}); err != nil {
			t.Fatalf("ConfirmFact(%d): %v", f.ID, err)
		}
		f.ConfirmedAt = sql.NullString{String: now, Valid: true}
	}
	return f
}

// seedClassifyingEvidence inserts one evidence row linking factID to
// episodeID - the sampler's ONLY use for evidence rows is classification
// (select.go), so polarity/weight/session are fixed placeholder values.
func seedClassifyingEvidence(t *testing.T, st *store.Store, factID, episodeID int64) {
	t.Helper()
	if _, err := st.Queries.InsertEvidence(context.Background(), sqlcgen.InsertEvidenceParams{
		FactID: factID, EpisodeID: sql.NullInt64{Int64: episodeID, Valid: true},
		Polarity: 1, Weight: 0, CreatedAt: nowStr(),
	}); err != nil {
		t.Fatalf("InsertEvidence(fact=%d, episode=%d): %v", factID, episodeID, err)
	}
}

// seedUnclassifiableEvidence inserts one evidence row with NO episode_id
// at all (e.g. a direct user assertion with no episode citation) - the
// sampler's fail-closed "no classification record" case.
func seedUnclassifiableEvidence(t *testing.T, st *store.Store, factID int64) {
	t.Helper()
	if _, err := st.Queries.InsertEvidence(context.Background(), sqlcgen.InsertEvidenceParams{
		FactID: factID, Polarity: 1, Weight: 0, CreatedAt: nowStr(),
	}); err != nil {
		t.Fatalf("InsertEvidence(fact=%d, no episode): %v", factID, err)
	}
}

func newTestTaint(st *store.Store) *taint.Tracker { return taint.New(st.Queries, st) }

func countEvidenceRows(t *testing.T, st *store.Store, factID int64) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM evidence WHERE fact_id = ?`, factID).Scan(&n); err != nil {
		t.Fatalf("count evidence rows for fact %d: %v", factID, err)
	}
	return n
}

func countEventsOfKind(t *testing.T, st *store.Store, kind string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM events WHERE kind = ?`, kind).Scan(&n); err != nil {
		t.Fatalf("count %s events: %v", kind, err)
	}
	return n
}

func getFact(t *testing.T, st *store.Store, id int64) sqlcgen.Fact {
	t.Helper()
	f, err := st.Queries.GetFact(context.Background(), id)
	if err != nil {
		t.Fatalf("GetFact(%d): %v", id, err)
	}
	return f
}

// fakeDelivery records every SendRitualQuestion call and answers with
// sendResult (default true - "reached Telegram") unless configured to
// simulate a gate-denied/failed send.
type fakeDelivery struct {
	sendResult bool
	calls      []fakeDeliveryCall
}

type fakeDeliveryCall struct {
	TraceID     string
	EvalLabelID int64
	FactText    string
}

func (f *fakeDelivery) SendRitualQuestion(_ context.Context, traceID string, evalLabelID int64, factText string) bool {
	f.calls = append(f.calls, fakeDeliveryCall{TraceID: traceID, EvalLabelID: evalLabelID, FactText: factText})
	return f.sendResult
}
