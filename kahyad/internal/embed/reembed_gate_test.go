package embed

import (
	"context"
	"testing"
)

// fakeReEmbedGate is a hermetic stand-in for the W78-01 retrieval eval gate:
// it returns a fixed allow/refuse verdict and records the model_ver it was
// asked to activate.
type fakeReEmbedGate struct {
	allow    bool
	gotModel string
}

func (g *fakeReEmbedGate) AllowReEmbedActivation(ctx context.Context, modelVer string) (bool, string, string) {
	g.gotModel = modelVer
	if g.allow {
		return true, "", ""
	}
	return false, "retrieval eval kapısı yeşil değil — önce 'kahya eval retrieval' çalıştır", "no green candidate (test)"
}

// TestReEmbedRefusedWithoutGreenGate proves gate point b: a full re_embed
// (model_ver activation) is refused when the eval gate is not green - it
// touches NO vectors, ledgers the refusal, and returns an error.
func TestReEmbedRefusedWithoutGreenGate(t *testing.T) {
	st := newTestStore(t)
	id := seedChunk(t, st, "a.md", "metin a")
	// Pre-seed a stale-version vector that a successful purge WOULD delete.
	seedVec(t, st, id, "old:v0")

	bf := NewBackfiller(st.DB(), &fakeEmbedder{}, activeVer, testLogger(t), st)
	gate := &fakeReEmbedGate{allow: false}
	bf.SetReEmbedGate(gate)

	_, err := bf.Backfill(context.Background(), "trace-1", true) // re_embed trigger
	if err == nil {
		t.Fatal("Backfill(full=true) with a red gate should return an error (activation refused)")
	}
	if gate.gotModel != activeVer {
		t.Fatalf("gate consulted with model_ver %q, want %q", gate.gotModel, activeVer)
	}

	// Fail-closed: the stale vector must still be present (no purge happened).
	if got := countModelVerRows(t, st, "old:v0"); got != 1 {
		t.Fatalf("stale vector rows = %d, want 1 (refused re_embed must not purge)", got)
	}

	// The refusal is ledgered.
	rows, err := st.Queries.ListEventsByKind(context.Background(), EventReEmbedRefused)
	if err != nil || len(rows) == 0 {
		t.Fatalf("ListEventsByKind(%s) = (%+v, %v), want at least one row", EventReEmbedRefused, rows, err)
	}
}

// TestReEmbedProceedsWithGreenGate proves the allow path: a green gate lets
// the full re_embed run and purge stale-version rows.
func TestReEmbedProceedsWithGreenGate(t *testing.T) {
	st := newTestStore(t)
	id := seedChunk(t, st, "a.md", "metin a")
	seedVec(t, st, id, "old:v0")

	bf := NewBackfiller(st.DB(), &fakeEmbedder{}, activeVer, testLogger(t), st)
	bf.SetReEmbedGate(&fakeReEmbedGate{allow: true})

	if _, err := bf.Backfill(context.Background(), "trace-1", true); err != nil {
		t.Fatalf("Backfill(full=true) with a green gate: %v", err)
	}
	if got := countModelVerRows(t, st, "old:v0"); got != 0 {
		t.Fatalf("stale vector rows = %d, want 0 (green re_embed purges stale model_ver)", got)
	}
}
