package server

import (
	"context"
	"database/sql"
	"testing"

	"kahya/kahyad/internal/store/sqlcgen"
)

// fakeTaskLaneQueries is an in-memory TaskLaneQueries fake.
type fakeTaskLaneQueries struct {
	rows map[string]sqlcgen.GetTaskLaneRow
}

func newFakeTaskLaneQueries() *fakeTaskLaneQueries {
	return &fakeTaskLaneQueries{rows: make(map[string]sqlcgen.GetTaskLaneRow)}
}

func (f *fakeTaskLaneQueries) SetTaskLane(ctx context.Context, arg sqlcgen.SetTaskLaneParams) error {
	f.rows[arg.ID] = sqlcgen.GetTaskLaneRow{Lane: arg.Lane, SecretCategory: arg.SecretCategory}
	return nil
}

func (f *fakeTaskLaneQueries) GetTaskLane(ctx context.Context, id string) (sqlcgen.GetTaskLaneRow, error) {
	row, ok := f.rows[id]
	if !ok {
		return sqlcgen.GetTaskLaneRow{}, sql.ErrNoRows
	}
	return row, nil
}

func TestSecretLaneStoreAdapterSetAndGet(t *testing.T) {
	q := newFakeTaskLaneQueries()
	a := NewSecretLaneStoreAdapter(q)

	if err := a.SetTaskLane(context.Background(), "t_1", "secret", "finans"); err != nil {
		t.Fatalf("SetTaskLane: %v", err)
	}
	lane, category, found, err := a.GetTaskLane(context.Background(), "t_1")
	if err != nil {
		t.Fatalf("GetTaskLane: %v", err)
	}
	if !found || lane != "secret" || category != "finans" {
		t.Errorf("GetTaskLane() = (%q,%q,found=%v), want (secret,finans,true)", lane, category, found)
	}
}

func TestSecretLaneStoreAdapterGetUnknownTaskNotFoundNotError(t *testing.T) {
	q := newFakeTaskLaneQueries()
	a := NewSecretLaneStoreAdapter(q)

	lane, category, found, err := a.GetTaskLane(context.Background(), "t_unknown")
	if err != nil {
		t.Fatalf("GetTaskLane() error = %v, want nil for an unknown task", err)
	}
	if found {
		t.Error("GetTaskLane() found = true, want false")
	}
	if lane != "" || category != "" {
		t.Errorf("GetTaskLane() = (%q,%q), want empty strings", lane, category)
	}
}
