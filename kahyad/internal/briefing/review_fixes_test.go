package briefing

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"kahya/kahyad/internal/secretlane"
)

// errOnLineClassifier returns (Verdict{SecretLane:false}, err) for the ONE
// line it is told to fail on, and a clean not-secret verdict for everything
// else. This is the shape a transient local-Qwen hiccup produces: an error
// alongside a zero-value verdict. A fail-OPEN caller (one that discards the
// error and reads only v.SecretLane) would treat this as "safe".
type errOnLineClassifier struct {
	failLine string
	err      error
}

func (c errOnLineClassifier) Classify(_ context.Context, text string) (secretlane.Verdict, error) {
	if strings.Contains(text, c.failLine) {
		return secretlane.Verdict{SecretLane: false, Category: secretlane.CategoryNone}, c.err
	}
	return secretlane.Verdict{SecretLane: false, Category: secretlane.CategoryNone}, nil
}

// hangingClassifier blocks until its ctx is cancelled, then returns that
// ctx's error - a local Qwen that accepted the connection but never answers.
type hangingClassifier struct{}

func (hangingClassifier) Classify(ctx context.Context, _ string) (secretlane.Verdict, error) {
	<-ctx.Done()
	return secretlane.Verdict{}, ctx.Err()
}

// TestDeliveryRedactionFailsClosedOnClassifierError is the regression test
// for the W5-01 review BLOCKER: the step-6 defense-in-depth redaction pass
// discarded the classifier error, so a secret line was delivered VERBATIM
// when the classifier hiccuped at exactly that call. A classifier ERROR must
// REDACT the line (fail-closed), never ship it in the clear.
func TestDeliveryRedactionFailsClosedOnClassifierError(t *testing.T) {
	const ibanLine = "Yeni hesap: TR330006100519786457841326"
	rawJSON, err := json.Marshal(BriefingSummaryV1{Lines: []string{"her sey yolunda.", ibanLine}})
	if err != nil {
		t.Fatal(err)
	}
	delivery := &fakeDelivery{Sent: true}
	o := &Orchestrator{
		// The classifier errors ONLY on the IBAN line, with a false verdict -
		// exactly the fail-open trap. (The deterministic pre-pass is NOT in
		// play here: this is a bare Classifier double, so the redaction pass's
		// own error handling is what must catch it.)
		Classifier: errOnLineClassifier{failLine: "TR330006100519786457841326", err: context.DeadlineExceeded},
		Spawner:    &fakeWorkerSpawner{RawJSON: string(rawJSON)},
		Delivery:   delivery,
		Now:        fixedNow(time.Date(2026, 7, 12, 8, 30, 0, 0, time.UTC)),
	}

	if _, err := o.Run(context.Background(), "trace-redact-failclosed"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(delivery.Calls) != 1 {
		t.Fatalf("delivery calls = %d, want 1", len(delivery.Calls))
	}
	delivered := delivery.Calls[0]
	if strings.Contains(delivered, "TR330006100519786457841326") {
		t.Fatalf("FAIL-OPEN: the IBAN line was delivered verbatim despite the classifier erroring on it:\n%s", delivered)
	}
	if !strings.Contains(delivered, PlaceholderSecretLane) {
		t.Errorf("delivered text lacks the placeholder (the errored line should be redacted): %q", delivered)
	}
}

// TestGateItemClassifierTimeoutFailsClosedAndBounded is the regression test
// for the W5-01 review MAJOR: gateItem imposes no timeout of its own, so a
// hung classifier under the deadline-less production ctx wedged the whole
// briefing forever. With the budget timeout, a hung classifier is dropped
// fail-closed within the budget, and gateItem never hangs.
func TestGateItemClassifierTimeoutFailsClosedAndBounded(t *testing.T) {
	done := make(chan gateOutcome, 1)
	go func() {
		// context.Background(): the exact deadline-less context the scheduler
		// passes in production - the timeout MUST come from gateItem itself.
		done <- gateItem(context.Background(), hangingClassifier{}, nil, CollectedItem{Text: "some pr title"})
	}()

	select {
	case out := <-done:
		if !out.Dropped {
			t.Fatalf("hung classifier item not dropped: %+v", out)
		}
		if out.DropReason != DropReasonClassifyFailed {
			t.Errorf("drop reason = %q, want %q", out.DropReason, DropReasonClassifyFailed)
		}
		if out.Line != PlaceholderSecretLane {
			t.Errorf("line = %q, want the placeholder", out.Line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("gateItem hung on a non-responsive classifier - the budget timeout did not fire (would wedge the whole briefing forever in production)")
	}
}

// TestCapTextStripsBidiAndZeroWidth is the regression test for the W5-01
// review MAJOR: capText let bidi-override / zero-width / other-Cf runes
// through into the cloud-model-bound prompt (Trojan-Source via an
// attacker-controlled PR title). They must be stripped.
func TestCapTextStripsBidiAndZeroWidth(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"rtl-override", "evil‮title.exe‬ safe"}, // U+202E RLO, U+202C PDF
		{"zero-width", "aban​kod"},               // U+200B ZERO WIDTH SPACE
		{"lrm", "x‎y"},                           // U+200E LEFT-TO-RIGHT MARK
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := capText(tc.in, 200)
			for _, bad := range []rune{'‮', '‬', '​', '‎', '‏', '⁦', '⁩'} {
				if strings.ContainsRune(got, bad) {
					t.Errorf("capText(%q) = %q still contains a control/format rune U+%04X", tc.in, got, bad)
				}
			}
		})
	}
}
