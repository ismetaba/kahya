// gate.go implements the W5-01 ordering-invariant gate: HANDOFF §4 ⚑
// (quoted verbatim in the task spec) - "Hicbir bayt, gizli-serit
// siniflandirmasi yerel/deterministik olarak tamamlanmadan bulut modele
// gitmez." The briefing summarizer runs on claude-haiku-4-5 (CLOUD), so
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
// "globs are file-path-only" rule), (2) content classification. Both a
// positive secret-lane verdict AND a classifier error/unavailability drop
// the item - only a clean, successfully-classified non-secret item is
// ever kept.
func gateItem(ctx context.Context, classifier Classifier, globs GlobMatcher, item CollectedItem) gateOutcome {
	if item.Path != "" && globs != nil && globs.MatchesSecretLane(item.Path) {
		return gateOutcome{Item: item, Line: PlaceholderSecretLane, Dropped: true, DropReason: DropReasonPathGlob}
	}
	if classifier == nil {
		// FAIL-CLOSED: no classifier wired at all means this item's
		// secret-lane status can never be verified - never let unclassified
		// bytes through to a cloud-facing prompt.
		return gateOutcome{Item: item, Line: PlaceholderSecretLane, Dropped: true, DropReason: DropReasonClassifierUnavailable}
	}
	// Bound the classify call by the warm-model budget (secretlane.
	// DefaultBudget, 300ms) INDEPENDENTLY of the ctx the caller passed: the
	// production caller (the scheduler handler) invokes Run with
	// context.Background() (no deadline), so a genuinely HUNG local Qwen
	// (connection accepted, never answers - HTTPQwenClassifier has no timeout
	// of its own) would otherwise wedge the entire briefing forever with no
	// fail-closed degradation. On deadline the Classify call returns a
	// context error, which the err branch below already treats fail-closed.
	cctx, cancel := context.WithTimeout(ctx, secretlane.DefaultBudget)
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
