// state.go persists nightly consolidation's pending-suggestion state in
// the append-only events ledger (task spec Deliverables: "Pending state
// stored in tasks/events") - never a second, bespoke table. This is the
// SAME ledger every other §5 safety#4 event lives in (job.triggered/
// completed, reader.rejected, secretlane_cloud_blocked, ...); brain.db's
// events table is explicitly carved out of the "consolidation never
// touches brain.db" write-boundary invariant (HANDOFF §5: "defter (events)
// ve episodes istisnadir - yalniz brain.db'de bulunurlar").
//
// "Is there currently a pending suggestion" is derived, not stored as a
// separate flag: FindPending reads the LAST consolidation.pending event
// and checks whether any LATER consolidation.approved/rejected/superseded
// event resolves that exact trace_id - if none does, that pending
// suggestion is still outstanding. This makes the ledger the single
// source of truth (across a kahyad restart, or a `kahya consolidation
// show` invoked from a separate process over UDS) with no risk of an
// in-memory flag drifting out of sync with what actually landed on disk.
package consolidation

import (
	"context"
	"encoding/json"
	"fmt"

	"kahya/kahyad/internal/store/sqlcgen"
)

// Ledger event kinds this package appends.
const (
	EventPending    = "consolidation.pending"
	EventApproved   = "consolidation.approved"
	EventRejected   = "consolidation.rejected"
	EventSuperseded = "consolidation.superseded"
)

// EventLogger is the append-only ledger sink this package writes to -
// mirrors kahyad/internal/scheduler.EventLogger's identical shape;
// *kahyad/internal/store.Store already satisfies it with no adapter.
type EventLogger interface {
	LogEvent(ctx context.Context, traceID, kind string, payload map[string]any) error
}

// EventRow is one ledger row, as read back by EventReader - a narrow,
// package-local shape (never kahyad/internal/store/sqlcgen.Event
// directly) so this package's own tests can inject a trivial in-memory
// fake without any brain.db dependency at all.
type EventRow struct {
	ID        int64
	TraceID   string
	Payload   string
	CreatedAt string
}

// EventReader is the append-only ledger's read seam this package needs -
// NewStoreEventReader adapts *kahyad/internal/store/sqlcgen.Queries's
// ListEventsByKind (already ordered oldest-first, ORDER BY id ASC).
type EventReader interface {
	ListEventsByKind(ctx context.Context, kind string) ([]EventRow, error)
}

// StoreEventReader adapts *sqlcgen.Queries to EventReader - the
// production implementation (main.go wires this to st.Queries, mirroring
// every other "*sqlcgen.Queries satisfies a narrow package-local
// interface via a small adapter" pattern already used throughout this
// codebase, e.g. kahyad/internal/server.NewSecretLaneStoreAdapter).
type StoreEventReader struct {
	Q *sqlcgen.Queries
}

func (r StoreEventReader) ListEventsByKind(ctx context.Context, kind string) ([]EventRow, error) {
	rows, err := r.Q.ListEventsByKind(ctx, kind)
	if err != nil {
		return nil, err
	}
	out := make([]EventRow, len(rows))
	for i, row := range rows {
		out[i] = EventRow{ID: row.ID, TraceID: row.TraceID, Payload: row.Payload, CreatedAt: row.CreatedAt}
	}
	return out, nil
}

// Pending is the currently-outstanding consolidation suggestion, as
// reconstructed from the ledger by FindPending.
type Pending struct {
	TraceID string
	Branch  string
	BaseSHA string
}

type pendingPayload struct {
	TraceID           string `json:"trace_id"`
	Branch            string `json:"branch"`
	BaseSHA           string `json:"base_sha"`
	SkippedSecretLane bool   `json:"skipped_secret_lane,omitempty"`
}

// FindPending returns the currently-outstanding pending suggestion, or nil
// if there is none (never ran yet, or the last one was approved/rejected/
// superseded).
func FindPending(ctx context.Context, reader EventReader) (*Pending, error) {
	pendingRows, err := reader.ListEventsByKind(ctx, EventPending)
	if err != nil {
		return nil, fmt.Errorf("consolidation: list %s events: %w", EventPending, err)
	}
	if len(pendingRows) == 0 {
		return nil, nil
	}
	last := pendingRows[len(pendingRows)-1]
	var p pendingPayload
	if err := json.Unmarshal([]byte(last.Payload), &p); err != nil {
		return nil, fmt.Errorf("consolidation: decode pending payload (event id %d): %w", last.ID, err)
	}

	resolved, err := traceIDResolved(ctx, reader, p.TraceID)
	if err != nil {
		return nil, err
	}
	if resolved {
		return nil, nil
	}
	return &Pending{TraceID: p.TraceID, Branch: p.Branch, BaseSHA: p.BaseSHA}, nil
}

// traceIDResolved reports whether traceID's own consolidation.pending
// event has since been closed out by an approved/rejected event carrying
// the SAME trace_id, or superseded by a later run's own event naming it as
// old_trace_id.
func traceIDResolved(ctx context.Context, reader EventReader, traceID string) (bool, error) {
	for _, kind := range []string{EventApproved, EventRejected} {
		rows, err := reader.ListEventsByKind(ctx, kind)
		if err != nil {
			return false, fmt.Errorf("consolidation: list %s events: %w", kind, err)
		}
		for _, row := range rows {
			var payload struct {
				TraceID string `json:"trace_id"`
			}
			if json.Unmarshal([]byte(row.Payload), &payload) == nil && payload.TraceID == traceID {
				return true, nil
			}
		}
	}
	supersededRows, err := reader.ListEventsByKind(ctx, EventSuperseded)
	if err != nil {
		return false, fmt.Errorf("consolidation: list %s events: %w", EventSuperseded, err)
	}
	for _, row := range supersededRows {
		var payload struct {
			OldTraceID string `json:"old_trace_id"`
		}
		if json.Unmarshal([]byte(row.Payload), &payload) == nil && payload.OldTraceID == traceID {
			return true, nil
		}
	}
	return false, nil
}

// LedgerPending appends a consolidation.pending event.
func LedgerPending(ctx context.Context, log EventLogger, traceID, branch, baseSHA string, skippedSecretLane bool) error {
	if log == nil {
		return nil
	}
	return log.LogEvent(ctx, traceID, EventPending, map[string]any{
		"trace_id": traceID, "branch": branch, "base_sha": baseSHA, "skipped_secret_lane": skippedSecretLane,
	})
}

// LedgerApproved appends a consolidation.approved event resolving
// pendingTraceID.
func LedgerApproved(ctx context.Context, log EventLogger, approveTraceID, pendingTraceID, mergeCommit string) error {
	if log == nil {
		return nil
	}
	return log.LogEvent(ctx, approveTraceID, EventApproved, map[string]any{
		"trace_id": pendingTraceID, "merge_commit": mergeCommit,
	})
}

// LedgerRejected appends a consolidation.rejected event resolving
// pendingTraceID.
func LedgerRejected(ctx context.Context, log EventLogger, rejectTraceID, pendingTraceID string) error {
	if log == nil {
		return nil
	}
	return log.LogEvent(ctx, rejectTraceID, EventRejected, map[string]any{
		"trace_id": pendingTraceID,
	})
}

// LedgerSuperseded appends a consolidation.superseded event carrying BOTH
// trace_ids (task spec, verbatim: "ledger consolidation.superseded with
// BOTH trace_ids").
func LedgerSuperseded(ctx context.Context, log EventLogger, newTraceID, oldTraceID string) error {
	if log == nil {
		return nil
	}
	return log.LogEvent(ctx, newTraceID, EventSuperseded, map[string]any{
		"old_trace_id": oldTraceID, "new_trace_id": newTraceID,
	})
}
