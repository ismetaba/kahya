// router.go is the Go routing branch (task spec deliverable): the code
// that ACTS on a Verdict (classifier.go). HANDOFF §5 product principle:
// "Gizlilik kodda: finans/sağlık/kimlik -> yerel-yalnız, hiçbir model
// çıktısı/enjeksiyonun geçemeyeceği Go dalı" - everything in this file is
// plain Go control flow a model cannot argue its way around, never a
// prompt-level instruction.
package secretlane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// Lane values persisted on the tasks row (task spec step 4: "extend the
// task envelope with lane: secret|normal"). kahyad/internal/spawn.Envelope
// carries these same two literal values in its own Lane field.
const (
	LaneSecret = "secret"
	LaneNormal = "normal"
)

// TaskLaneStore is the narrow persistence seam this package needs: the
// STICKY per-task lane (task spec gotcha: "Lane is per-TASK and sticky:
// once a task is lane: secret, it stays secret for its lifetime, including
// resume - persist the lane on the tasks row"). *store.Store's sqlc
// Queries satisfies this via a small adapter (kahyad/internal/server, the
// same "ambiguity-decision" pattern already used for mcp/fs's
// PolicyClient) - this package deliberately does not import
// kahyad/internal/store/sqlcgen directly, so it stays independently
// testable with a trivial in-memory fake (see router_test.go).
type TaskLaneStore interface {
	// SetTaskLane persists (lane, category) for taskID. Callers are
	// responsible for the STICKY "never downgrade" rule (Escalate below
	// applies it) - this method itself is a plain, unconditional write.
	SetTaskLane(ctx context.Context, taskID, lane, category string) error
	// GetTaskLane returns the CURRENTLY persisted lane/category for
	// taskID. found=false means no row exists yet (a brand-new task) -
	// never treated as an error, but also never treated as "known normal".
	GetTaskLane(ctx context.Context, taskID string) (lane, category string, found bool, err error)
}

// EventLedger mirrors kahyad/internal/egress.Ledger's exact shape (the
// same append-only ledger every other §5 safety#4 event writes to).
type EventLedger interface {
	LogEvent(ctx context.Context, traceID, kind string, payload map[string]any) error
}

// EventSecretLaneCloudBlocked is the ledger/JSONL event kind the proxy
// backstop hook (below) writes on every block - task spec: "the proxy
// consults the task registry ... secret-lane task => 403 + ledger event
// secretlane_cloud_blocked", the §6 W3 gate "gizli-şerit içerik bulut
// çağrısına çıkamıyor (test)".
const EventSecretLaneCloudBlocked = "secretlane_cloud_blocked"

// MsgSecretLaneCloudBlocked is the Turkish, user-facing body of the 403
// response the proxy backstop hook produces (CLAUDE.md language policy;
// kahyad/internal/anthproxy.Proxy.ServeHTTP renders whatever error this
// hook returns as the "permission_error" message body).
const MsgSecretLaneCloudBlocked = "Bu görev gizli şerit (finans/sağlık/kimlik) olarak etiketlendi; bulut modele erişemez."

// ClassifyForNewTask runs classifier against text and reports the Verdict
// - the caller (kahyad/internal/server's POST /v1/task handler) uses this
// BEFORE a tasks row exists at all, to decide the brand-new task's OWN
// envelope.Lane/Category and its OWN InsertTask row (task spec step 4:
// "kahyad sets it before worker spawn; the worker never chooses the
// lane"). This function itself never touches the store - there is nothing
// to be "sticky" against yet for a task that doesn't exist. When the
// verdict is secret-lane, markSensitiveRead (may be nil) is called with
// sessionKey/traceID - the SAME "keyed on trace_id" convention
// kahyad/internal/egress.NewAnthproxyEgressGateHook already uses (task
// spec step 6: "classifier hit => POST /session/sensitive-read").
func ClassifyForNewTask(ctx context.Context, classifier *Classifier, sessionKey, traceID, text string, markSensitiveRead func(ctx context.Context, sessionKey, traceID string) error) (Verdict, error) {
	verdict, classifyErr := classifier.Classify(ctx, text)
	// classifyErr is diagnostic only: Classify's OWN return value already
	// encodes fail-closed (SecretLane:true) on any error - this function
	// does not additionally special-case it (a caller wanting to log the
	// underlying reason can inspect the returned error itself).

	if verdict.SecretLane && markSensitiveRead != nil {
		_ = markSensitiveRead(ctx, sessionKey, traceID)
	}
	return verdict, classifyErr
}

// Escalate re-classifies text against an ALREADY-EXISTING task (e.g. a
// memory_write call, or any later ingestion point within the same task's
// lifetime that hands new content to a model) and STICKILY widens taskID's
// persisted lane to secret when warranted - it NEVER downgrades an
// already-secret task back to normal (task spec gotcha: "once a task is
// lane: secret, it stays secret for its lifetime, including resume").
// Returns the FINAL (post-widen) lane/category actually persisted.
func Escalate(ctx context.Context, classifier *Classifier, store TaskLaneStore, taskID, traceID, text string, markSensitiveRead func(ctx context.Context, sessionKey, traceID string) error) (lane, category string, err error) {
	verdict, _ := classifier.Classify(ctx, text)

	existingLane, existingCategory, found, storeErr := store.GetTaskLane(ctx, taskID)
	if storeErr != nil || !found {
		existingLane = ""
		existingCategory = ""
	}

	lane = LaneNormal
	category = CategoryNone
	if verdict.SecretLane {
		lane = LaneSecret
		category = verdict.Category
	}
	// STICKY: an already-secret task can never be written back to normal
	// by a LATER call that happens not to trip the classifier - keep
	// whatever this call found for category (a more specific new category
	// is still useful information), but the lane itself only ever widens.
	if existingLane == LaneSecret {
		lane = LaneSecret
		if category == CategoryNone || category == "" {
			category = existingCategory
		}
	}

	if err := store.SetTaskLane(ctx, taskID, lane, category); err != nil {
		return "", "", fmt.Errorf("secretlane: persist lane for task %s: %w", taskID, err)
	}

	if lane == LaneSecret && markSensitiveRead != nil {
		_ = markSensitiveRead(ctx, traceID, traceID)
	}
	return lane, category, nil
}

// LaneLookup is the narrow read-only seam the proxy backstop hook needs.
// *store.Store's sqlc Queries satisfies this via the SAME small adapter
// TaskLaneStore uses (GetTaskLane's shape is intentionally identical).
type LaneLookup interface {
	GetTaskLane(ctx context.Context, taskID string) (lane, category string, found bool, err error)
}

// NewProxyBackstopHook builds the per-task egress-gate-shaped hook
// (kahyad/internal/anthproxy.ProxyConfig.EgressGate's fixed
// func(*http.Request) error shape; kahyad/internal/server.SetAnthproxy's
// egressGateFactory parameter) that is THE W12-08 proxy chokepoint
// enforcement (task spec step 5): "the W12-08 proxy consults the task
// registry by trace_id/task_id header; secret-lane task => 403 + ledger
// secretlane_cloud_blocked. This is the enforcement even if a
// prompt-injected worker tries the cloud URL directly."
//
// taskID/traceID are captured ONCE per task (the same per-task-closure
// pattern kahyad/internal/egress.NewAnthproxyEgressGateHook already uses)
// - the worker cannot influence its own task's lane by any means (it
// never chooses it - router.go's ClassifyForNewTask/Escalate are the ONLY
// writers, both entirely kahyad-side), so this check is unbypassable from
// inside the worker process, unlike a client-suppliable header would be.
func NewProxyBackstopHook(lookup LaneLookup, ledger EventLedger) func(taskID, traceID string) func(*http.Request) error {
	return func(taskID, traceID string) func(*http.Request) error {
		return func(r *http.Request) error {
			lane, _, found, err := lookup.GetTaskLane(r.Context(), taskID)
			if err != nil {
				// Fail-closed (tasks/README.md global convention): a lookup
				// failure must never be treated as "assume normal lane" -
				// this is the same posture as every other policy/permission
				// check in this codebase.
				logSecretLaneBlocked(ledger, r.Context(), traceID, taskID, "lookup_error")
				return errors.New(MsgSecretLaneCloudBlocked)
			}
			if !found || lane != LaneSecret {
				return nil
			}
			logSecretLaneBlocked(ledger, r.Context(), traceID, taskID, "secret_lane")
			return errors.New(MsgSecretLaneCloudBlocked)
		}
	}
}

func logSecretLaneBlocked(ledger EventLedger, ctx context.Context, traceID, taskID, reason string) {
	if ledger == nil {
		return
	}
	_ = ledger.LogEvent(ctx, traceID, EventSecretLaneCloudBlocked, map[string]any{
		"task_id": taskID,
		"reason":  reason,
	})
}
