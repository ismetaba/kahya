// actor_seed.go implements the W4-03 Actor-seeding path (task spec step
// 7): every time a Reader episode's validated, Go-side schema-checked
// output (schemas.go) is ready, Spawn renders it DETERMINISTICALLY
// (RenderPrompt below - stable key order, no raw string outside the
// validated struct's own fields ever appears) into a brand-new task with
// a FRESH session_id, inserts that session's session_taint(tier=clean)
// row IN THE SAME TRANSACTION as the task row itself, and ledgers
// actor.seeded (derived_from = the originating Reader job's episode id).
//
// This is the SECOND (and only other) place a session_taint row is ever
// born clean - see kahyad/internal/taint's own package doc comment for
// the first (kahyad/internal/server's OnSession callback). W-actions from
// this freshly-seeded session then flow through the ordinary W3-02
// ladder/approval path exactly like any other session's - Spawn performs
// no policy decision of its own.
package reader

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"kahya/kahyad/internal/spawn"
	"kahya/kahyad/internal/store/sqlcgen"
	"kahya/kahyad/internal/taint"
)

// EventActorSeeded is the ledger event kind task spec step 7 names,
// verbatim.
const EventActorSeeded = "actor.seeded"

// ActorDB is the narrow *sql.DB subset Spawn needs (BeginTx only) -
// kahyad/internal/store.Store.DB() satisfies this directly. A narrow
// interface (rather than the concrete *sql.DB type) purely so this
// package's own tests can be explicit about what Spawn actually needs,
// mirroring kahyad/internal/taint.Store's identical "consumer defines the
// interface" convention.
type ActorDB interface {
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

// ActorResult is Spawn's successful outcome: everything a caller needs to
// actually dispatch the seeded Actor task (kahyad/internal/spawn.Run, with
// Prompt as the envelope's own Prompt field) - Spawn itself does not
// dispatch a worker; it only prepares the durable task/session_taint rows
// and the deterministic prompt.
type ActorResult struct {
	TaskID    string
	SessionID string
	Prompt    string
}

// RenderPrompt deterministically renders a validated Reader output
// (MailSummaryV1 or WebpageExtractV1 - the only two types this function
// accepts) into the fresh Actor session's own prompt text: a STABLE field
// order, and no raw string outside the validated struct's own
// already-sanitized/length-capped fields ever appears - the untrusted
// original bytes are never referenced, quoted, or reachable from this
// function at all (task spec step 7, verbatim).
func RenderPrompt(validated any) (string, error) {
	var b strings.Builder
	switch v := validated.(type) {
	case MailSummaryV1:
		b.WriteString("Aşağıda bir e-postadan Okuyucu tarafından çıkarılmış, doğrulanmış bilgiler var (ham e-posta metni DEĞİL):\n")
		fmt.Fprintf(&b, "Gönderen: %s\n", v.FromDisplay)
		fmt.Fprintf(&b, "Konu: %s\n", v.Subject)
		fmt.Fprintf(&b, "Özet: %s\n", v.Summary)
		fmt.Fprintf(&b, "Tarihler: %s\n", strings.Join(v.Dates, ", "))
		fmt.Fprintf(&b, "Tutarlar: %s\n", strings.Join(v.Amounts, ", "))
	case WebpageExtractV1:
		b.WriteString("Aşağıda bir web sayfasından Okuyucu tarafından çıkarılmış, doğrulanmış bilgiler var (ham sayfa metni DEĞİL):\n")
		fmt.Fprintf(&b, "Başlık: %s\n", v.Title)
		b.WriteString("Ana noktalar:\n")
		for _, p := range v.KeyPoints {
			fmt.Fprintf(&b, "- %s\n", p)
		}
	default:
		return "", fmt.Errorf("reader: RenderPrompt: unsupported validated type %T (only MailSummaryV1/WebpageExtractV1)", validated)
	}
	return b.String(), nil
}

// newActorSessionID mints a fresh, random session_id for a seeded Actor
// task ("actor-<hex32>" - the same entropy/shape convention
// newReaderSessionID uses, kept visually distinguishable by its own
// prefix).
func newActorSessionID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("reader: crypto/rand unavailable: %v", err))
	}
	return "actor-" + hex.EncodeToString(b)
}

// Spawn implements the task spec step 7 Actor-seeding path. db is the raw
// *sql.DB Spawn opens its own transaction against (store.Store.DB()) -
// the SAME "tasks row + session_taint(clean) row commit atomically"
// pattern kahyad/internal/server's persistSessionStarted already
// establishes for a user-initiated task's own OnSession capture. ledger
// may be nil. model is the model this Actor task should run
// (kahyad/internal/spawn.AllowedModels - the caller's own routing
// decision, never chosen here); episodeID identifies the originating
// Reader job (Result.EpisodeID) for the actor.seeded ledger event's
// derived_from field. traceID correlates every write this call performs.
//
// tasks.taint_tier (a Day-1, pre-W4-03 column - migrations/0001) is set
// to "clean" here purely as an informational mirror; it is NOT the real
// enforcement mechanism (session_taint is - see that table's own
// migration doc comment) and nothing in this codebase currently reads it
// back.
func Spawn(ctx context.Context, db ActorDB, ledger Ledger, validated any, model, episodeID, traceID string) (ActorResult, error) {
	prompt, err := RenderPrompt(validated)
	if err != nil {
		return ActorResult{}, err
	}

	taskID := spawn.NewTaskID()
	sessionID := newActorSessionID()
	now := time.Now().UTC().Format(time.RFC3339)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return ActorResult{}, fmt.Errorf("actor_seed: begin tx: %w", err)
	}
	txq := sqlcgen.New(tx)

	if _, err := txq.InsertTask(ctx, sqlcgen.InsertTaskParams{
		ID:        taskID,
		TraceID:   traceID,
		SessionID: sql.NullString{String: sessionID, Valid: true},
		State:     "running",
		TaintTier: "clean",
		Model:     sql.NullString{String: model, Valid: model != ""},
		UpdatedAt: now,
		CreatedAt: now,
		Lane:      "normal",
	}); err != nil {
		_ = tx.Rollback()
		return ActorResult{}, fmt.Errorf("actor_seed: insert task: %w", err)
	}

	// ledger is deliberately nil here, not the outer ledger param - see
	// kahyad/internal/server's persistSessionStarted for why: brain.db's
	// connection pool is capped at exactly 1, and this goroutine already
	// holds that single connection via tx: a ledger write through the
	// caller's ORDINARY (non-tx) ledger would try to acquire a SECOND
	// connection from the same exhausted pool and deadlock against itself.
	// The should-never-happen lower-attempt case is ledgered manually,
	// below, AFTER Rollback has released the connection.
	if err := taint.New(txq, nil).InsertClean(ctx, traceID, sessionID); err != nil {
		_ = tx.Rollback()
		if ledger != nil && errors.Is(err, taint.ErrLowerAttempt) {
			_ = ledger.LogEvent(ctx, traceID, taint.EventLowerAttempt, map[string]any{
				"event": taint.EventLowerAttempt, "session_id": sessionID,
			})
		}
		return ActorResult{}, fmt.Errorf("actor_seed: insert session_taint clean: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return ActorResult{}, fmt.Errorf("actor_seed: commit: %w", err)
	}

	if ledger != nil {
		_ = ledger.LogEvent(ctx, traceID, EventActorSeeded, map[string]any{
			"event": EventActorSeeded, "task_id": taskID, "session_id": sessionID, "derived_from": episodeID,
		})
	}

	return ActorResult{TaskID: taskID, SessionID: sessionID, Prompt: prompt}, nil
}
