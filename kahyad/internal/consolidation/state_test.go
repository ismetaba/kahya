package consolidation

import (
	"context"
	"encoding/json"
	"testing"
)

// fakeEventStore is an in-memory EventLogger+EventReader fake - every
// hermetic test in this package that needs ledger state uses this instead
// of a real brain.db, exactly mirroring kahyad/internal/backup's own
// "temp git repo + injectable runner, no real filesystem/network" posture
// applied to the events ledger instead of git.
type fakeEventStore struct {
	rows []fakeEventRow
}

type fakeEventRow struct {
	traceID string
	kind    string
	payload map[string]any
}

func (f *fakeEventStore) LogEvent(ctx context.Context, traceID, kind string, payload map[string]any) error {
	f.rows = append(f.rows, fakeEventRow{traceID: traceID, kind: kind, payload: payload})
	return nil
}

func (f *fakeEventStore) ListEventsByKind(ctx context.Context, kind string) ([]EventRow, error) {
	var out []EventRow
	for i, r := range f.rows {
		if r.kind != kind {
			continue
		}
		b, err := json.Marshal(r.payload)
		if err != nil {
			return nil, err
		}
		out = append(out, EventRow{ID: int64(i + 1), TraceID: r.traceID, Payload: string(b), CreatedAt: "2026-07-12T00:00:00Z"})
	}
	return out, nil
}

func TestFindPendingNoneWhenNeverRan(t *testing.T) {
	store := &fakeEventStore{}
	p, err := FindPending(context.Background(), store)
	if err != nil {
		t.Fatalf("FindPending() error = %v", err)
	}
	if p != nil {
		t.Fatalf("FindPending() = %+v, want nil", p)
	}
}

func TestFindPendingReturnsOutstandingSuggestion(t *testing.T) {
	store := &fakeEventStore{}
	if err := LedgerPending(context.Background(), store, "trace-1", "kahya/consolidation-20260712", "abc123", false); err != nil {
		t.Fatalf("LedgerPending() error = %v", err)
	}
	p, err := FindPending(context.Background(), store)
	if err != nil {
		t.Fatalf("FindPending() error = %v", err)
	}
	if p == nil || p.TraceID != "trace-1" || p.Branch != "kahya/consolidation-20260712" || p.BaseSHA != "abc123" {
		t.Fatalf("FindPending() = %+v, want trace-1/kahya/consolidation-20260712/abc123", p)
	}
}

func TestFindPendingNoneAfterApproved(t *testing.T) {
	store := &fakeEventStore{}
	ctx := context.Background()
	if err := LedgerPending(ctx, store, "trace-1", "branch-1", "sha1", false); err != nil {
		t.Fatal(err)
	}
	if err := LedgerApproved(ctx, store, "trace-2", "trace-1", "merged-sha"); err != nil {
		t.Fatal(err)
	}
	p, err := FindPending(ctx, store)
	if err != nil {
		t.Fatalf("FindPending() error = %v", err)
	}
	if p != nil {
		t.Fatalf("FindPending() = %+v, want nil after approval", p)
	}
}

func TestFindPendingNoneAfterRejected(t *testing.T) {
	store := &fakeEventStore{}
	ctx := context.Background()
	if err := LedgerPending(ctx, store, "trace-1", "branch-1", "sha1", false); err != nil {
		t.Fatal(err)
	}
	if err := LedgerRejected(ctx, store, "trace-2", "trace-1"); err != nil {
		t.Fatal(err)
	}
	p, err := FindPending(ctx, store)
	if err != nil {
		t.Fatalf("FindPending() error = %v", err)
	}
	if p != nil {
		t.Fatalf("FindPending() = %+v, want nil after rejection", p)
	}
}

func TestFindPendingReturnsFreshOneAfterSupersede(t *testing.T) {
	store := &fakeEventStore{}
	ctx := context.Background()
	if err := LedgerPending(ctx, store, "trace-1", "branch-1", "sha1", false); err != nil {
		t.Fatal(err)
	}
	if err := LedgerSuperseded(ctx, store, "trace-2", "trace-1"); err != nil {
		t.Fatal(err)
	}
	if err := LedgerPending(ctx, store, "trace-2", "branch-2", "sha2", false); err != nil {
		t.Fatal(err)
	}
	p, err := FindPending(ctx, store)
	if err != nil {
		t.Fatalf("FindPending() error = %v", err)
	}
	if p == nil || p.TraceID != "trace-2" || p.Branch != "branch-2" {
		t.Fatalf("FindPending() = %+v, want the fresh trace-2/branch-2 suggestion", p)
	}
}
