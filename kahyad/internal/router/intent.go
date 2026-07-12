// intent.go implements W4-08's intent-classification half: a
// deterministic-first, fail-closed wrapper around kahyad/internal/
// secretlane's EXTENDED combined classify call, which now returns
// {secret_lane, category, intent} in one round-trip (see that package's
// classifier.go for the schema/prompt change). table.go's SelectModel
// consumes whatever intent this produces (or its own IntentChat default)
// purely as data - this file owns only HOW that intent is obtained.
package router

import (
	"context"
	"fmt"
	"time"

	"kahya/kahyad/internal/secretlane"
)

// DefaultIntentBudget mirrors secretlane.DefaultBudget (HANDOFF §4 ⚑: the
// <300ms-warm classification budget). Advisory, exactly like that
// constant's own doc comment: a caller that knows the local Qwen model is
// currently warm derives a context.WithTimeout(ctx, DefaultIntentBudget)
// itself before calling ClassifyIntent - this file imposes no timeout of
// its own beyond the caller's ctx (same rationale as secretlane.Classifier.
// Classify's own doc comment: a cold model must be allowed to wait for
// load or fail closed, never silently skip ahead).
const DefaultIntentBudget = secretlane.DefaultBudget

// Intent-classification source values - intent_classified's own "source"
// field (task spec: "source: model|deterministic").
const (
	SourceDeterministic = "deterministic"
	SourceModel         = "model"
)

// EventIntentClassified is the ledger/JSONL event LogIntentClassified
// emits - the FIRST step of the W4-08 ledger-ordering acceptance criterion
// (intent_classified -> routing_decision -> model_call, one trace_id).
const EventIntentClassified = "intent_classified"

// ClassifyIntentInput is ClassifyIntent's pure input.
type ClassifyIntentInput struct {
	// DeterministicIntent, when non-empty, SHORT-CIRCUITS classification
	// with NO model call at all (task spec: "explicit opt-in forms,
	// CLI-declared task kinds short-circuit WITHOUT a model call"). An
	// ordinary /v1/task chat prompt with no CLI-declared kind resolves this
	// to IntentChat BEFORE ClassifyIntent is ever called - so the "do NOT
	// make every chat message do a Qwen round-trip" constraint holds
	// structurally: on that path, this function never even reaches
	// `classifier` at all.
	DeterministicIntent string
	// Text is the raw content to classify - consulted ONLY when
	// DeterministicIntent is empty (an EXPLICIT ask-to-classify caller: the
	// W4-03 Reader ingest path, or memory_write/fs-read escalation).
	Text string
}

// ClassifyIntentResult is ClassifyIntent's result.
type ClassifyIntentResult struct {
	// Intent is empty when classification failed (see ClassifyIntent's own
	// doc comment) - callers must never substitute IntentChat themselves in
	// that case; SelectModel's own IntentChat default only applies when a
	// caller explicitly PASSES an empty/unknown Intent as ordinary
	// (non-failure) input, never as a stand-in for "classification failed".
	Intent string
	// Source is SourceDeterministic or SourceModel.
	Source string
	// Verdict is the FULL secret-lane verdict the SAME combined call
	// produced - or the classifier's own fail-closed SecretLane:true
	// verdict on error (secretlane.Classifier.Classify's existing,
	// unchanged posture). Callers needing the lane (not just the intent)
	// read this rather than making a second call; LaneFromVerdict converts
	// it to RouteInput.Lane's own string convention.
	Verdict secretlane.Verdict
	// Duration is how long this call took (0 for a deterministic
	// short-circuit) - intent_classified's own duration_ms field.
	Duration time.Duration
}

// ClassifyIntent implements the task spec's deterministic-first,
// fail-closed intent classification.
//
// DeterministicIntent short-circuits with zero model dependency.
// Otherwise this defers to classifier's SAME combined {secret_lane,
// category, intent} call W3-08 already uses for secret-lane detection
// (kahyad/internal/secretlane.Classifier.Classify) - literally the same
// method, so a hanging/erroring classifier automatically ALSO fails closed
// to secret_lane:true (Verdict, entirely unchanged from W3-08's existing
// posture) AND yields Intent=="" here. A caller composing this result's
// Verdict into SelectModel's RouteInput.Lane (via LaneFromVerdict) therefore
// lands on the local lane, never a cloud model, on any classifier
// error/hang - the ordering invariant holds without this function needing
// any fail-closed logic of its own beyond propagating classifier's own
// (W3-08's existing, permanent guarantee).
func ClassifyIntent(ctx context.Context, classifier *secretlane.Classifier, in ClassifyIntentInput) (ClassifyIntentResult, error) {
	start := time.Now()
	if in.DeterministicIntent != "" {
		return ClassifyIntentResult{
			Intent: in.DeterministicIntent, Source: SourceDeterministic, Duration: time.Since(start),
		}, nil
	}

	if classifier == nil {
		// No local classifier wired at all - mirrors
		// secretlane.Classifier.Classify's own nil-Qwen fail-closed verdict
		// exactly, so a caller composing THIS result behaves identically to
		// one that called the real classifier and got the same error.
		// Source is "deterministic", NOT "model": no model round-trip was
		// even attempted here (see SourceForVerdict's own doc comment).
		return ClassifyIntentResult{
			Source: SourceDeterministic,
			Verdict: secretlane.Verdict{
				SecretLane: true, Category: secretlane.CategoryUnknown, Reason: "qwen_unavailable_fail_closed",
			},
			Duration: time.Since(start),
		}, fmt.Errorf("router: no classifier wired for intent classification")
	}

	verdict, err := classifier.Classify(ctx, in.Text)
	dur := time.Since(start)
	// Source is derived from verdict.Reason, NOT merely from "classifier is
	// non-nil" or "err is non-nil" - classifier.Classify's own deterministic
	// pre-pass can hit (Reason e.g. "iban", err==nil - no model consulted at
	// all) exactly as easily as its Qwen leg can genuinely be attempted and
	// fail (Reason "qwen_error_fail_closed", err!=nil) or never even be
	// attempted because no Qwen is wired (Reason
	// "qwen_unavailable_fail_closed", ALSO err!=nil) - only Reason itself
	// distinguishes these, see SourceForVerdict.
	source := SourceForVerdict(verdict.Reason)
	if err != nil {
		// classifier.Classify's own return value already encodes
		// fail-closed (SecretLane:true) - propagated as-is, Intent left
		// empty (no usable intent on failure).
		return ClassifyIntentResult{Source: source, Verdict: verdict, Duration: dur}, err
	}
	return ClassifyIntentResult{Intent: verdict.Intent, Source: source, Verdict: verdict, Duration: dur}, nil
}

// SourceForVerdict classifies which "source" produced a secretlane.Verdict
// by inspecting its Reason field - the only signal available for "was a
// model round-trip actually attempted", since Classify's return SHAPE
// alone (Verdict/error) does not separately expose that. "qwen" (success),
// "qwen_error_fail_closed", and "qwen_invalid_category_fail_closed" all
// mean the local Qwen server was genuinely called (successfully or not);
// every other Reason - a deterministic pre-pass hit ("iban", "tckn",
// "card_number", "cvv", "keyword:...") OR "qwen_unavailable_fail_closed"
// (no Qwen wired at all, so nothing was ever attempted) - means no model
// call happened, so intent_classified's own "source" field must say
// SourceDeterministic, never SourceModel, for those.
func SourceForVerdict(reason string) string {
	switch reason {
	case "qwen", "qwen_error_fail_closed", "qwen_invalid_category_fail_closed":
		return SourceModel
	default:
		return SourceDeterministic
	}
}

// LaneFromVerdict converts a secretlane.Verdict into RouteInput.Lane's own
// string convention.
func LaneFromVerdict(v secretlane.Verdict) string {
	if v.SecretLane {
		return LaneSecret
	}
	return LaneNormal
}

// LogIntentClassified records one ClassifyIntent call's outcome under
// traceID (task spec: "emit intent_classified {intent, duration_ms,
// source} with trace_id"). ledger may be nil (no-op, same posture as
// LogRoutingDecision).
func LogIntentClassified(ctx context.Context, ledger Ledger, traceID string, result ClassifyIntentResult) {
	if ledger == nil {
		return
	}
	_ = ledger.LogEvent(ctx, traceID, EventIntentClassified, map[string]any{
		"intent": result.Intent, "duration_ms": result.Duration.Milliseconds(), "source": result.Source,
	})
}
