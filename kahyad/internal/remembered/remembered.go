// Package remembered implements the W5-03 "hatirladi ani" (remembered-
// moment) marking flow's single write path (HANDOFF S6 metrik tanimlari
// flag: "hatirladi ani = kullanicinin o oturumda acikca vermedigi bir
// hafiza olgusunun yanitta dogru kullanimi (kullanici elle isaretler,
// haftalik rituele baglanir)"; S9 MVP-done: "haftada >=5 hatirladi ani").
//
// Marker.Mark is the ONE place that ever appends an events row
// kind="remembered_moment" - kahyad/internal/server's POST /v1/remembered
// handler (the CLI's `kahya remembered --trace <id>`, channel=local) and
// kahyad/internal/telegram's "Hatirladi" button callback (channel=remote)
// are both thin callers of this SAME function, never independent writers
// (kahyad is brain.db's only writer; this package is that writer's ONE
// remembered-moment-shaped door, mirroring kahyad/internal/factengine's
// identical "one package, one door" posture for facts).
//
// Idempotency is a REAL, SQL-enforced guarantee, not merely an
// application-level check: migrations/0013_eval_labels.sql adds a partial
// UNIQUE index on events(trace_id) WHERE kind='remembered_moment', so a
// second Mark call for a trace_id already marked hits a UNIQUE constraint
// violation at the INSERT itself - Mark catches exactly that violation
// (isUniqueConstraintViolation, the same mattn/go-sqlite3 shape
// kahyad/internal/taint and kahyad/internal/task/receipts.go already
// detect it by) and reports duplicate=true rather than a second row.
package remembered

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mattn/go-sqlite3"

	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/store/sqlcgen"
)

// EventRememberedMoment is the ledger/events kind Mark appends on a fresh
// mark (never on a duplicate - see Mark's own doc comment).
const EventRememberedMoment = "remembered_moment"

// EventRememberedDuplicate is the JSONL-only (never ledgered - there is
// nothing new to append-only-record) line Mark logs when a trace_id was
// already marked (W5-03 task spec deliverable, verbatim: "a re-mark
// inserts nothing and is JSONL-logged as remembered_moment.duplicate").
const EventRememberedDuplicate = "remembered_moment.duplicate"

// ErrEmptyTrace is returned by Mark when traceID is blank - never a valid
// events.trace_id to look up or mark.
var ErrEmptyTrace = errors.New("remembered: trace_id bos olamaz")

// ErrUnknownTrace is returned by Mark when traceID has no row at all in
// the events ledger - there is no task/ritual run for the CLI/Telegram
// caller to mark as remembered. The message is ALREADY the Turkish,
// user-facing string (CLAUDE.md language policy: CLI/bot-facing strings
// are Turkish) `kahya remembered --trace <id>` prints verbatim on an
// unknown trace (W5-03 acceptance criterion).
var ErrUnknownTrace = errors.New("Bilinmeyen iz (trace_id): böyle bir görev/ritüel bulunamadı.")

// Store is the narrow events-ledger READ surface Mark needs to validate a
// trace_id actually exists before marking it - *sqlcgen.Queries (via
// *store.Store) already has exactly this method shape (W12-02's own
// ListEventsByTrace, `kahya log --trace <id>`'s identical read), so it
// satisfies this directly with no adapter.
type Store interface {
	ListEventsByTrace(ctx context.Context, traceID string) ([]sqlcgen.Event, error)
}

// Ledger is the append-only events-ledger WRITE surface (HANDOFF S5
// safety #4) - *kahyad/internal/store.Store already has exactly this
// method shape (Store.LogEvent), so it satisfies this directly.
type Ledger interface {
	LogEvent(ctx context.Context, traceID, kind string, payload map[string]any) error
}

// Marker is kahyad's single remembered-moment write path (package doc).
// Construct one with New per kahyad process and share it across every
// caller (the POST /v1/remembered HTTP handler AND the Telegram
// "Hatirladi" callback) - never a second, independent implementation.
type Marker struct {
	store  Store
	ledger Ledger
	log    *logx.Logger
}

// New constructs a Marker. log may be nil (the duplicate JSONL line is
// simply skipped - this codebase's usual "unwired dependency" posture).
func New(store Store, ledger Ledger, log *logx.Logger) *Marker {
	return &Marker{store: store, ledger: ledger, log: log}
}

// Mark validates traceID exists in the events ledger, then appends ONE
// events row kind="remembered_moment" carrying {trace_id, channel}.
// channel is "local" (CLI) or "remote" (Telegram) - callers pass whichever
// surface this mark actually came from; not otherwise validated here
// (an unrecognized value is stored as-is - this package's job is
// idempotency and existence-validation, not channel enum enforcement).
//
// Returns duplicate=true (and a nil error) when traceID was ALREADY
// marked - the SQL-level partial-unique-index violation this function
// catches, never a second row and never an error surfaced to the caller
// (a re-mark is not a failure, HANDOFF S6's "kullanici elle isaretler"
// flow is deliberately safe to invoke more than once for the same trace).
func (m *Marker) Mark(ctx context.Context, traceID, channel string) (duplicate bool, err error) {
	traceID = strings.TrimSpace(traceID)
	if traceID == "" {
		return false, ErrEmptyTrace
	}

	rows, err := m.store.ListEventsByTrace(ctx, traceID)
	if err != nil {
		return false, fmt.Errorf("remembered: list events for trace %s: %w", traceID, err)
	}
	if len(rows) == 0 {
		return false, ErrUnknownTrace
	}

	err = m.ledger.LogEvent(ctx, traceID, EventRememberedMoment, map[string]any{
		"trace_id": traceID, "channel": channel,
	})
	if err == nil {
		return false, nil
	}
	if isUniqueConstraintViolation(err) {
		if m.log != nil {
			m.log.With(traceID).Info(EventRememberedDuplicate, "channel", channel)
		}
		return true, nil
	}
	return false, fmt.Errorf("remembered: log event for trace %s: %w", traceID, err)
}

// isUniqueConstraintViolation reports whether err is a SQLite unique (or
// partial-unique-index) constraint violation - the mattn/go-sqlite3 shape
// migrations/0013_eval_labels.sql's idx_events_remembered_moment_once
// surfaces as. Mirrors kahyad/internal/taint.isUniqueConstraintViolation
// and kahyad/internal/task/receipts.go's identically-named helper
// (duplicated here rather than imported - this package must not depend on
// either of those, and both of them are unexported in their own package).
func isUniqueConstraintViolation(err error) bool {
	var sqliteErr sqlite3.Error
	if errors.As(err, &sqliteErr) {
		return sqliteErr.Code == sqlite3.ErrConstraint
	}
	return false
}
