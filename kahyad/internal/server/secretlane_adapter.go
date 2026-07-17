// secretlane_adapter.go adapts *store.Store's sqlc Queries to
// kahyad/internal/secretlane's TaskLaneStore/LaneLookup interfaces - both
// have the IDENTICAL GetTaskLane shape by construction, so one small
// adapter satisfies both (the same "ambiguity-decision" pattern this
// package already uses for mcp/fs's PolicyClient - kahyad/internal/
// secretlane cannot import kahyad/internal/store/sqlcgen directly without
// creating a dependency the classifier/router package has no business
// having).
package server

import (
	"context"
	"database/sql"
	"errors"

	"kahya/kahyad/internal/secretlane"
	"kahya/kahyad/internal/store/sqlcgen"
)

// TaskLaneQueries is the narrow sqlcgen dependency this adapter needs -
// *store.Store.Queries already has exactly this shape.
type TaskLaneQueries interface {
	SetTaskLane(ctx context.Context, arg sqlcgen.SetTaskLaneParams) error
	GetTaskLane(ctx context.Context, id string) (sqlcgen.GetTaskLaneRow, error)
}

// SecretLaneStoreAdapter adapts TaskLaneQueries to kahyad/internal/
// secretlane.TaskLaneStore AND kahyad/internal/secretlane.LaneLookup
// (identical GetTaskLane signature - see this file's own doc comment).
type SecretLaneStoreAdapter struct {
	q TaskLaneQueries
}

// NewSecretLaneStoreAdapter constructs a SecretLaneStoreAdapter.
func NewSecretLaneStoreAdapter(q TaskLaneQueries) *SecretLaneStoreAdapter {
	return &SecretLaneStoreAdapter{q: q}
}

// SetTaskLane implements kahyad/internal/secretlane.TaskLaneStore.
func (a *SecretLaneStoreAdapter) SetTaskLane(ctx context.Context, taskID, lane, category string) error {
	return a.q.SetTaskLane(ctx, sqlcgen.SetTaskLaneParams{
		Lane:           lane,
		SecretCategory: sql.NullString{String: category, Valid: category != ""},
		UpdatedAt:      rfc3339Now(),
		ID:             taskID,
	})
}

// EscalateTaskLane implements mcp/fs.SecretLaneEscalator (project-review
// #2): a secret-lane fs_read stickily widens the owning task's lane to
// secret so the W12-08 proxy backstop 403s the worker's subsequent cloud
// call. Widen-only: an already-secret task keeps its (possibly more
// specific) category rather than being clobbered with "unknown". category
// is "" when the caller matched only a PATH glob and cannot name the
// finans/saglik/kimlik sub-category — CategoryUnknown is the honest label.
func (a *SecretLaneStoreAdapter) EscalateTaskLane(ctx context.Context, taskID, traceID, category string) error {
	if category == "" {
		category = secretlane.CategoryUnknown
	}
	curLane, curCat, found, err := a.GetTaskLane(ctx, taskID)
	if err != nil {
		return err
	}
	if found && curLane == secretlane.LaneSecret && curCat != "" && curCat != secretlane.CategoryNone {
		return nil // already secret with a specific category; nothing to widen
	}
	return a.SetTaskLane(ctx, taskID, secretlane.LaneSecret, category)
}

// GetTaskLane implements BOTH kahyad/internal/secretlane.TaskLaneStore and
// kahyad/internal/secretlane.LaneLookup. found=false (no error) means
// taskID has no tasks row at all yet - never treated as an error, but also
// never treated as "known normal" by a caller that cares about the
// distinction (kahyad/internal/secretlane.Escalate does; the proxy
// backstop hook treats "not found" as "not secret", since there is no
// task there to protect - see NewProxyBackstopHook's own doc comment).
func (a *SecretLaneStoreAdapter) GetTaskLane(ctx context.Context, taskID string) (lane, category string, found bool, err error) {
	row, err := a.q.GetTaskLane(ctx, taskID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", false, nil
		}
		return "", "", false, err
	}
	return row.Lane, row.SecretCategory.String, true, nil
}
