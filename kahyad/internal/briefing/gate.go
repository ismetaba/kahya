// gate.go implements the W5-01 ordering-invariant gate: HANDOFF §4 ⚑
// (quoted verbatim in the task spec) - "Hicbir bayt, gizli-serit
// siniflandirmasi yerel/deterministik olarak tamamlanmadan bulut modele
// gitmez." The briefing summarizer runs on router.ModelHaiku (CLOUD), so
// EVERY collector item (PR titles, file names/paths, calendar event
// titles) MUST pass through this gate BEFORE the worker envelope is ever
// built (briefing.go's Run calls gateItem for every CollectedItem strictly
// before BuildEnvelope) - a secret-lane-classified item is DROPPED here
// and represented in the delivered briefing only by PlaceholderSecretLane;
// it never reaches the cloud model. policy.yaml's secret_lane_globs are
// file-PATH-only (the ordering invariant's own text: "policy.yaml globlari
// yalniz dosya yollari icin") - checked first, for file-sourced items
// only, via GlobMatcher; every item (file-sourced or not) then ALSO passes
// through the content classifier. Classifier failure (model/memory
// unavailable) is FAIL-CLOSED: treated exactly like a positive secret-lane
// verdict, never silently let through.
package briefing

import (
	"context"
	"time"

	"kahya/kahyad/internal/secretlane"
)

// PlaceholderSecretLane is the byte-exact Turkish placeholder substituted
// for any secret-lane-classified (or fail-closed-dropped) collector item -
// HANDOFF §4 ⚑ ordering invariant / W5-01 task spec, verbatim.
const PlaceholderSecretLane = "[gizli-şerit: yerel işlendi]"

// Drop reasons gateItem records - used both for the briefing.item_dropped
// ledger payload and by this package's own tests.
const (
	DropReasonPathGlob              = "path_glob"
	DropReasonSecretLane            = "secret_lane"
	DropReasonClassifyFailed        = "classify_failed"
	DropReasonClassifierUnavailable = "classifier_unavailable"
)

// CollectedItem is one piece of content-sourced text a collector produced
// - the ordering-invariant gate's unit. Text is the free-text field to
// classify (already length/charclass-capped by the collector that
// produced it - see each collect_*.go file). Path is set only for
// file-sourced items (the absolute path additionally checked against
// policy.yaml's secret_lane_globs); empty for gh/calendar items, which
// carry no filesystem path at all.
type CollectedItem struct {
	// Section names the bucket this item belongs to when the prompt/
	// delivered text is assembled ("gh_pr" | "gh_run" | "calendar" |
	// "file").
	Section string
	Text    string
	Path    string
}

// Classifier is the narrow W3-08 pre-classifier surface the gate needs.
// *secretlane.Classifier (deterministic pre-pass, then Qwen fallback -
// fails closed on ANY Qwen error, including "no Qwen wired at all")
// satisfies this directly, with no adapter.
type Classifier interface {
	Classify(ctx context.Context, text string) (secretlane.Verdict, error)
}

var _ Classifier = (*secretlane.Classifier)(nil)

// Warmer is the narrow "bring the local classifier model up and block
// until it answers /v1/models" surface Run uses ONCE right before the gate
// loop, and once more before the delivery-time redaction pass (FIX-P-4).
// *kahyad/internal/mlx.Supervisor satisfies this directly (EnsureRunning:
// fail-closed spawn-time memcheck + spawn + health poll bounded by its own
// StartupGrace; an already-warm server returns at once and never re-runs
// the memcheck - FIX-I-9). nil means "no warm-up step": every hermetic
// test that injects a fake Classifier, and nothing else.
type Warmer interface {
	EnsureRunning(ctx context.Context) error
}

// gateClassifyBudget bounds ONE gate/redaction classify call AFTER the
// model has been warmed - a hang backstop only (HTTPQwenClassifier has no
// timeout of its own and the production ctx has no deadline). The real
// per-call latency bound for a warm Qwen call is mlx.Config.
// ClassifyWarmBudget (cfg.qwen_classify_budget_ms, default 300ms), applied
// inside mlx.QwenClassifierAdapter; this value stays comfortably ABOVE it
// so the adapter's budget, not this backstop, is what decides.
const gateClassifyBudget = 5 * time.Second

// classifierWarmupBudget bounds the ONE cold warm-up call before the gate
// loop: generous, above mlx.Config.StartupGrace's 120s default, so a cold
// 17GB load is bounded by the supervisor's own grace, never by an
// unrelated clock here (task spec: "cold model means the classification
// WAITS for load or fails closed - it never skips ahead to cloud").
const classifierWarmupBudget = 3 * time.Minute

// GlobMatcher is the narrow policy.yaml secret_lane_globs surface the gate
// needs for file-sourced items. PolicyGlobMatcher (production.go) is the
// production implementation, wrapping policy.MatchGlob.
type GlobMatcher interface {
	MatchesSecretLane(path string) bool
}

// gateOutcome is gateItem's result: exactly one of "kept, Line ==
// item.Text" or "dropped, Line == PlaceholderSecretLane, DropReason
// explains why".
type gateOutcome struct {
	Item       CollectedItem
	Line       string
	Dropped    bool
	DropReason string
}

// gateItem runs the ordering-invariant gate on one item, in this fixed
// order: (1) file-path glob check (file-sourced items only - HANDOFF's
// "globs are file-path-only" rule), (2) content classification bounded by
// budget. Both a positive secret-lane verdict AND a classifier error/
// unavailability/timeout drop the item - only a clean, successfully-
// classified non-secret item is ever kept.
func gateItem(ctx context.Context, classifier Classifier, globs GlobMatcher, item CollectedItem, budget time.Duration) gateOutcome {
	if item.Path != "" && globs != nil && globs.MatchesSecretLane(item.Path) {
		return gateOutcome{Item: item, Line: PlaceholderSecretLane, Dropped: true, DropReason: DropReasonPathGlob}
	}
	if classifier == nil {
		// FAIL-CLOSED: no classifier wired at all means this item's
		// secret-lane status can never be verified - never let unclassified
		// bytes through to a cloud-facing prompt.
		return gateOutcome{Item: item, Line: PlaceholderSecretLane, Dropped: true, DropReason: DropReasonClassifierUnavailable}
	}
	// Bound the classify call by budget INDEPENDENTLY of the ctx the caller
	// passed: the production caller (the scheduler handler) invokes Run
	// with context.Background() (no deadline), so a genuinely HUNG local
	// Qwen (connection accepted, never answers - HTTPQwenClassifier has no
	// timeout of its own) would otherwise wedge the entire briefing forever
	// with no fail-closed degradation. On deadline the Classify call returns
	// a context error, which the err branch below already treats
	// fail-closed. Run passes gateClassifyBudget after its warm-up (FIX-P-4):
	// the 300ms warm figure is the mlx adapter's own per-call budget
	// (cfg.qwen_classify_budget_ms), never applied here to a cold load.
	cctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	verdict, err := classifier.Classify(cctx, item.Text)
	if err != nil {
		// FAIL-CLOSED (task spec, verbatim): "If the local classifier
		// CANNOT run (model/memory failure), classification is
		// FAIL-CLOSED: treat the item as secret-lane and DROP it." A hung
		// classifier surfaces here as context.DeadlineExceeded (see above).
		return gateOutcome{Item: item, Line: PlaceholderSecretLane, Dropped: true, DropReason: DropReasonClassifyFailed}
	}
	if verdict.SecretLane {
		return gateOutcome{Item: item, Line: PlaceholderSecretLane, Dropped: true, DropReason: DropReasonSecretLane}
	}
	return gateOutcome{Item: item, Line: item.Text}
}
