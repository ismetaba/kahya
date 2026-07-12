package briefing

import (
	"context"
	"errors"
	"testing"
)

// TestGateItemKeepsCleanText proves an item that matches no path glob and
// classifies non-secret is kept verbatim.
func TestGateItemKeepsCleanText(t *testing.T) {
	c := &fakeClassifier{}
	item := CollectedItem{Section: "gh_pr", Text: "kahya/gold-token#12: bump deps"}

	out := gateItem(context.Background(), c, nil, item)
	if out.Dropped {
		t.Fatalf("Dropped = true, want false (clean item)")
	}
	if out.Line != item.Text {
		t.Errorf("Line = %q, want %q", out.Line, item.Text)
	}
}

// TestGateItemDropsOnPathGlobBeforeEverClassifying is the ordering-
// invariant's file-path-glob half: a file item whose path matches
// policy.yaml's secret_lane_globs is dropped WITHOUT ever reaching the
// classifier - checked here by asserting the fake classifier's Calls slice
// stays empty.
func TestGateItemDropsOnPathGlobBeforeEverClassifying(t *testing.T) {
	c := &fakeClassifier{}
	globs := fakeGlobMatcher{Paths: map[string]bool{"/Users/x/Documents/saglik/notes.md": true}}
	item := CollectedItem{Section: "file", Text: "notes.md (2026-07-12T08:00:00Z)", Path: "/Users/x/Documents/saglik/notes.md"}

	out := gateItem(context.Background(), c, globs, item)
	if !out.Dropped {
		t.Fatal("Dropped = false, want true (path matched a secret-lane glob)")
	}
	if out.DropReason != DropReasonPathGlob {
		t.Errorf("DropReason = %q, want %q", out.DropReason, DropReasonPathGlob)
	}
	if out.Line != PlaceholderSecretLane {
		t.Errorf("Line = %q, want the byte-exact placeholder", out.Line)
	}
	if len(c.Calls) != 0 {
		t.Errorf("classifier.Calls = %v, want empty (glob check must short-circuit before content classification)", c.Calls)
	}
}

// TestGateItemDropsOnSecretLaneVerdict proves a positive content-
// classification verdict drops the item and substitutes the placeholder.
func TestGateItemDropsOnSecretLaneVerdict(t *testing.T) {
	c := &fakeClassifier{Marks: []string{"IBAN"}}
	item := CollectedItem{Section: "gh_pr", Text: "kahya/x#3: rotate IBAN in docs"}

	out := gateItem(context.Background(), c, nil, item)
	if !out.Dropped {
		t.Fatal("Dropped = false, want true (secret-lane verdict)")
	}
	if out.DropReason != DropReasonSecretLane {
		t.Errorf("DropReason = %q, want %q", out.DropReason, DropReasonSecretLane)
	}
	if out.Line != PlaceholderSecretLane {
		t.Errorf("Line = %q, want the byte-exact placeholder", out.Line)
	}
}

// TestGateItemFailClosedOnClassifierError is the FAIL-CLOSED regression
// test: a classifier error (model/memory unavailable) drops the item
// exactly like a positive secret-lane verdict - never lets unclassified
// bytes through.
func TestGateItemFailClosedOnClassifierError(t *testing.T) {
	c := &fakeClassifier{Err: errors.New("qwen unavailable (simulated)")}
	item := CollectedItem{Section: "gh_pr", Text: "kahya/x#4: totally ordinary PR title"}

	out := gateItem(context.Background(), c, nil, item)
	if !out.Dropped {
		t.Fatal("Dropped = false, want true (classifier error must fail closed)")
	}
	if out.DropReason != DropReasonClassifyFailed {
		t.Errorf("DropReason = %q, want %q", out.DropReason, DropReasonClassifyFailed)
	}
	if out.Line != PlaceholderSecretLane {
		t.Errorf("Line = %q, want the byte-exact placeholder", out.Line)
	}
}

// TestGateItemFailClosedOnNilClassifier proves a completely unwired
// classifier ALSO fails closed - "cannot verify" must never be treated as
// "assume safe".
func TestGateItemFailClosedOnNilClassifier(t *testing.T) {
	item := CollectedItem{Section: "gh_pr", Text: "anything"}
	out := gateItem(context.Background(), nil, nil, item)
	if !out.Dropped || out.DropReason != DropReasonClassifierUnavailable {
		t.Fatalf("gateItem(nil classifier) = %+v, want Dropped=true reason=%q", out, DropReasonClassifierUnavailable)
	}
}
