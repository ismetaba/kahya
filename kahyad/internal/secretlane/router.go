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
	"sync"
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

// ErrCloudBlocked is the sentinel NewProxyBackstopHook returns when it
// refuses a model call because the task's persisted lane IS secret
// (FIX-B-6). Error() == MsgSecretLaneCloudBlocked, so the proxy's 403 body
// is unchanged; kahyad/main.go's combined egress hook uses errors.Is on it
// to raise MsgSecretLaneCloudBlockedNotice once per task (CloudBlockedNotices).
var ErrCloudBlocked = errors.New(MsgSecretLaneCloudBlocked)

// ErrCloudBlockedLookupFailed is the backstop's fail-closed refusal when the
// task's lane could not be READ at all (a transient brain.db error). Same
// Turkish 403 body, but deliberately NOT ErrCloudBlocked: nothing says the
// task touched secret-lane content, so no "gizli-şerit içeriğe dokundu"
// notice is raised, and a caller that ends the task on ErrCloudBlocked
// (G00 U-4) does not end it on a transient read error.
var ErrCloudBlockedLookupFailed = errors.New(MsgSecretLaneCloudBlocked)

// MsgSecretLaneCloudBlockedNotice is the user notification raised the first
// time the proxy backstop refuses a task's model call because the task was
// escalated to the secret lane (FIX-B-6) - once per task, never per call
// (CloudBlockedNotices).
const MsgSecretLaneCloudBlockedNotice = "Görev gizli-şerit (finans/sağlık/kimlik) içeriğe dokundu; bulut modeline devam edilemez."

// EventSecretLaneCloudBlockedNotice is the ledger/JSONL kind of that
// one-shot notice. Distinct from EventSecretLaneCloudBlocked on purpose:
// notify.JSONLNotifier.Alarm ledgers under the kind it is given, so reusing
// EventSecretLaneCloudBlocked would write a second secretlane_cloud_blocked
// row for the first block and make "one secretlane_cloud_blocked row per
// refused call" ambiguous.
const EventSecretLaneCloudBlockedNotice = "secretlane_cloud_blocked_notice"

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
// Returns the FINAL (post-widen) lane/category actually persisted. The
// persistence half is EscalateVerdict.
func Escalate(ctx context.Context, classifier *Classifier, store TaskLaneStore, taskID, traceID, text string, markSensitiveRead func(ctx context.Context, sessionKey, traceID string) error) (lane, category string, err error) {
	verdict, _ := classifier.Classify(ctx, text)
	return EscalateVerdict(ctx, store, taskID, traceID, verdict, markSensitiveRead)
}

// EscalateVerdict is Escalate's persistence half (FIX-B-6): given an
// ALREADY-DECIDED verdict (fs_read's deterministic content check or path
// glob, a Reader verdict, ...), stickily widen taskID's persisted lane. A
// GetTaskLane read ERROR is returned as-is and nothing is written: it is
// never "no row yet" - before FIX-B-6 a transient read failure plus a
// non-secret verdict wrote lane=normal over an existing secret row, a
// silent downgrade. found=false (no row, no error) is still written as a
// plain SetTaskLane - a brand-new task's first verdict.
func EscalateVerdict(ctx context.Context, store TaskLaneStore, taskID, traceID string, verdict Verdict, markSensitiveRead func(ctx context.Context, sessionKey, traceID string) error) (lane, category string, err error) {
	existingLane, existingCategory, found, storeErr := store.GetTaskLane(ctx, taskID)
	if storeErr != nil {
		return "", "", fmt.Errorf("secretlane: read lane for task %s: %w", taskID, storeErr)
	}
	if !found {
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
// never chooses it - router.go's ClassifyForNewTask/Escalate/EscalateVerdict
// are the ONLY writers, all entirely kahyad-side), so this check is
// unbypassable from inside the worker process, unlike a client-suppliable
// header would be. No caching: a lane escalated mid-task (fs_read, FIX-B-6)
// takes effect on the very next request. It returns ErrCloudBlocked for a
// secret-lane task and ErrCloudBlockedLookupFailed when the lane cannot be
// read (fail-closed); both render as the same 403 body.
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
				return ErrCloudBlockedLookupFailed
			}
			if !found || lane != LaneSecret {
				return nil
			}
			logSecretLaneBlocked(ledger, r.Context(), traceID, taskID, "secret_lane")
			return ErrCloudBlocked
		}
	}
}

// Alarmer is the narrow notification surface CloudBlockedNotices needs -
// kahyad/internal/notify.Notifier's Alarm method (JSONL warn line + ledger
// row under the given kind).
type Alarmer interface {
	Alarm(ctx context.Context, traceID, kind, message string, payload map[string]any) error
}

// CloudBlockedNotices raises MsgSecretLaneCloudBlockedNotice (ledger kind
// EventSecretLaneCloudBlockedNotice) the FIRST time the proxy backstop
// refuses a task's model call with ErrCloudBlocked - once per task id for
// the life of the kahyad process, however many calls (or worker spawns of
// the same task, e.g. a resume) are refused afterwards (FIX-B-6). One
// instance is shared by every per-task proxy hook (kahyad/main.go).
type CloudBlockedNotices struct {
	alarm Alarmer

	mu   sync.Mutex
	sent map[string]bool
}

// NewCloudBlockedNotices constructs a CloudBlockedNotices over alarm (nil =
// never notify).
func NewCloudBlockedNotices(alarm Alarmer) *CloudBlockedNotices {
	return &CloudBlockedNotices{alarm: alarm, sent: make(map[string]bool)}
}

// Observe is called with every error the backstop hook returned for
// (taskID, traceID). Only ErrCloudBlocked counts (a lookup failure is not
// evidence of secret-lane content); the first one per task raises the
// notice. sent reports whether this call raised it; err is the notifier's
// error (the task id is marked notified either way - "once" means once,
// and the JSONL half has already been written when only the ledger fails).
// The notice is written under a context detached from cancellation: the
// refused request's own context may be canceled the moment the 403 lands.
func (n *CloudBlockedNotices) Observe(ctx context.Context, taskID, traceID string, blockErr error) (sent bool, err error) {
	if n == nil || n.alarm == nil || !errors.Is(blockErr, ErrCloudBlocked) {
		return false, nil
	}
	n.mu.Lock()
	if n.sent[taskID] {
		n.mu.Unlock()
		return false, nil
	}
	n.sent[taskID] = true
	n.mu.Unlock()
	return true, n.alarm.Alarm(context.WithoutCancel(ctx), traceID, EventSecretLaneCloudBlockedNotice,
		MsgSecretLaneCloudBlockedNotice, map[string]any{"task_id": taskID})
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

// ChainEgressGates composes the per-task anthproxy EgressGate factory
// kahyad/main.go wires (kahyad/internal/server.SetAnthproxy): backstop
// (NewProxyBackstopHook - the persisted lane, read on every request) runs
// FIRST and a refusal ends the request there; next (the egress allowlist/
// budget/sensitive-read gate) runs only when the backstop allowed. Every
// backstop refusal is shown to notices (nil = no notice), which raises the
// one-per-task MsgSecretLaneCloudBlockedNotice.
func ChainEgressGates(backstop, next func(taskID, traceID string) func(*http.Request) error, notices *CloudBlockedNotices) func(taskID, traceID string) func(*http.Request) error {
	return func(taskID, traceID string) func(*http.Request) error {
		secretHook := backstop(taskID, traceID)
		nextHook := next(taskID, traceID)
		return func(r *http.Request) error {
			if err := secretHook(r); err != nil {
				_, _ = notices.Observe(r.Context(), taskID, traceID, err)
				return err
			}
			return nextHook(r)
		}
	}
}
