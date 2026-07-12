package router

import (
	"context"
	"testing"
	"time"

	"kahya/kahyad/internal/secretlane"
)

// TestClassifyIntentDeterministicShortCircuitNeverCallsClassifier proves
// the deterministic-first requirement: a non-empty DeterministicIntent
// short-circuits with source=deterministic and NEVER touches the
// classifier at all - passing a classifier that would panic/fail if ever
// invoked is what actually proves this (a nil classifier would only prove
// "didn't crash on nil", not "never called").
func TestClassifyIntentDeterministicShortCircuitNeverCallsClassifier(t *testing.T) {
	poison := secretlane.NewClassifier(secretlane.QwenClassifierFunc(func(ctx context.Context, text string) (secretlane.Verdict, error) {
		t.Fatal("classifier.Classify was called despite a non-empty DeterministicIntent - deterministic short-circuit must never touch the model")
		return secretlane.Verdict{}, nil
	}))

	result, err := ClassifyIntent(context.Background(), poison, ClassifyIntentInput{
		DeterministicIntent: IntentChat, Text: "kredi kartı ekstresi ekte", // even secret-lane-shaped text must never reach the classifier here
	})
	if err != nil {
		t.Fatalf("ClassifyIntent() error = %v, want nil", err)
	}
	if result.Intent != IntentChat {
		t.Errorf("result.Intent = %q, want %q", result.Intent, IntentChat)
	}
	if result.Source != SourceDeterministic {
		t.Errorf("result.Source = %q, want %q", result.Source, SourceDeterministic)
	}
}

// TestClassifyIntentModelSuccessReturnsIntentAndSourceModel proves the
// combined-call happy path: a well-behaved classifier's Verdict.Intent
// flows straight through, tagged source=model.
func TestClassifyIntentModelSuccessReturnsIntentAndSourceModel(t *testing.T) {
	classifier := secretlane.NewClassifier(secretlane.QwenClassifierFunc(func(ctx context.Context, text string) (secretlane.Verdict, error) {
		return secretlane.Verdict{SecretLane: false, Category: secretlane.CategoryNone, Intent: IntentExtract, Reason: "qwen"}, nil
	}))

	result, err := ClassifyIntent(context.Background(), classifier, ClassifyIntentInput{Text: "şu maildeki tarihleri çıkar"})
	if err != nil {
		t.Fatalf("ClassifyIntent() error = %v, want nil", err)
	}
	if result.Intent != IntentExtract {
		t.Errorf("result.Intent = %q, want %q", result.Intent, IntentExtract)
	}
	if result.Source != SourceModel {
		t.Errorf("result.Source = %q, want %q", result.Source, SourceModel)
	}
	if result.Verdict.SecretLane {
		t.Error("result.Verdict.SecretLane = true, want false")
	}
}

// TestClassifyIntentFailClosedOnHangingClassifier is the acceptance
// criterion's own "classifier stubbed to hang/fail" scenario: ClassifyIntent
// returns an error, the Verdict it returns is ALREADY fail-closed
// (SecretLane:true, exactly W3-08's existing posture) with no usable
// Intent, and composing that Verdict into SelectModel via LaneFromVerdict
// proves the downstream routing decision is Local (never any cloud model) -
// i.e. zero bytes would ever reach the Anthropic proxy for this task.
func TestClassifyIntentFailClosedOnHangingClassifier(t *testing.T) {
	hang := secretlane.QwenClassifierFunc(func(ctx context.Context, text string) (secretlane.Verdict, error) {
		<-ctx.Done()
		return secretlane.Verdict{}, ctx.Err()
	})
	classifier := secretlane.NewClassifier(hang)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	result, err := ClassifyIntent(ctx, classifier, ClassifyIntentInput{Text: "hiçbir kalıba uymayan sıradan bir metin"})
	if err == nil {
		t.Fatal("ClassifyIntent() error = nil, want an error from the hung/ctx-deadline classifier")
	}
	if !result.Verdict.SecretLane {
		t.Fatalf("result.Verdict.SecretLane = false, want true (fail-closed per W3-08 - classification failure must never look like \"safe\")")
	}
	if result.Intent != "" {
		t.Errorf("result.Intent = %q, want empty (no usable intent on a classifier failure)", result.Intent)
	}

	decision := SelectModel(RouteInput{
		Intent: result.Intent, Lane: LaneFromVerdict(result.Verdict), DefaultModel: ModelSonnet,
	})
	if !decision.Local || decision.Model != "" {
		t.Errorf("SelectModel(post-failure verdict) = %+v, want Local=true Model=\"\" - the ordering invariant must hold even when classification itself fails", decision)
	}
}

// TestClassifyIntentNilClassifierFailsClosed mirrors
// secretlane.Classifier's own "no Qwen wired at all" fail-closed posture:
// a nil classifier (no DeterministicIntent given) must fail closed exactly
// like a real one that errored, never silently default to a cloud-routed
// intent. Source must be "deterministic", NOT "model" - no model
// round-trip was ever attempted (post-review fix: this used to report
// SourceModel here, which would make intent_classified's own "source"
// field lie about a call that never happened).
func TestClassifyIntentNilClassifierFailsClosed(t *testing.T) {
	result, err := ClassifyIntent(context.Background(), nil, ClassifyIntentInput{Text: "herhangi bir metin"})
	if err == nil {
		t.Fatal("ClassifyIntent() error = nil, want an error for a nil classifier")
	}
	if !result.Verdict.SecretLane {
		t.Error("result.Verdict.SecretLane = false, want true (fail-closed)")
	}
	if result.Intent != "" {
		t.Errorf("result.Intent = %q, want empty", result.Intent)
	}
	if result.Source != SourceDeterministic {
		t.Errorf("result.Source = %q, want %q (no model call was ever attempted)", result.Source, SourceDeterministic)
	}
}

// TestClassifyIntentDeterministicPrePassHitReportsSourceDeterministic
// proves a classifier whose DETERMINISTIC pre-pass matches (e.g. an IBAN)
// - never reaching Qwen at all - reports source=deterministic, not
// source=model, even though a non-nil *secretlane.Classifier was passed in
// (post-review fix: ClassifyIntent used to report SourceModel for EVERY
// non-nil classifier call, regardless of whether Qwen was actually
// consulted).
func TestClassifyIntentDeterministicPrePassHitReportsSourceDeterministic(t *testing.T) {
	poison := secretlane.NewClassifier(secretlane.QwenClassifierFunc(func(ctx context.Context, text string) (secretlane.Verdict, error) {
		t.Fatal("Qwen was called despite a deterministic pre-pass hit (IBAN) - it should never be reached")
		return secretlane.Verdict{}, nil
	}))
	result, err := ClassifyIntent(context.Background(), poison, ClassifyIntentInput{Text: "IBAN: TR330006100519786457841326"})
	if err != nil {
		t.Fatalf("ClassifyIntent() error = %v, want nil", err)
	}
	if result.Source != SourceDeterministic {
		t.Errorf("result.Source = %q, want %q", result.Source, SourceDeterministic)
	}
	if !result.Verdict.SecretLane {
		t.Error("result.Verdict.SecretLane = false, want true (IBAN hit)")
	}
}

// TestSourceForVerdict covers every Reason value SourceForVerdict branches
// on directly.
func TestSourceForVerdict(t *testing.T) {
	modelReasons := []string{"qwen", "qwen_error_fail_closed", "qwen_invalid_category_fail_closed"}
	for _, reason := range modelReasons {
		if got := SourceForVerdict(reason); got != SourceModel {
			t.Errorf("SourceForVerdict(%q) = %q, want %q", reason, got, SourceModel)
		}
	}
	deterministicReasons := []string{"iban", "tckn", "card_number", "cvv", "keyword:sağlık", "qwen_unavailable_fail_closed", "reader_no_classifier_fail_closed", ""}
	for _, reason := range deterministicReasons {
		if got := SourceForVerdict(reason); got != SourceDeterministic {
			t.Errorf("SourceForVerdict(%q) = %q, want %q", reason, got, SourceDeterministic)
		}
	}
}

// TestLogIntentClassifiedNilLedgerNoop proves a nil Ledger is a safe no-op.
func TestLogIntentClassifiedNilLedgerNoop(t *testing.T) {
	LogIntentClassified(context.Background(), nil, "trace1", ClassifyIntentResult{Intent: IntentChat, Source: SourceDeterministic})
}

// TestLaneFromVerdict covers both branches directly.
func TestLaneFromVerdict(t *testing.T) {
	if got := LaneFromVerdict(secretlane.Verdict{SecretLane: true}); got != LaneSecret {
		t.Errorf("LaneFromVerdict(secret) = %q, want %q", got, LaneSecret)
	}
	if got := LaneFromVerdict(secretlane.Verdict{SecretLane: false}); got != LaneNormal {
		t.Errorf("LaneFromVerdict(normal) = %q, want %q", got, LaneNormal)
	}
}
