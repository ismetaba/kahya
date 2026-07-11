package approval

import (
	"bufio"
	"strings"
	"testing"
	"time"
)

func TestPromptYesNo_AcceptsEAndEvet(t *testing.T) {
	for _, in := range []string{"e", "evet", "EVET", "E"} {
		r := bufio.NewReader(strings.NewReader(in + "\n"))
		var w strings.Builder
		d, err := PromptYesNo(r, &w, "prompt: ", "e", "evet")
		if err != nil {
			t.Fatalf("input %q: unexpected error: %v", in, err)
		}
		if d != DecisionApprove {
			t.Fatalf("input %q: expected DecisionApprove, got %v", in, d)
		}
	}
}

func TestPromptYesNo_RejectsHAndAnythingElse(t *testing.T) {
	for _, in := range []string{"h", "hayır", "", "yes", "onayla"} {
		r := bufio.NewReader(strings.NewReader(in + "\n"))
		var w strings.Builder
		d, _ := PromptYesNo(r, &w, "prompt: ", "e", "evet")
		if d != DecisionDeny {
			t.Fatalf("input %q: expected DecisionDeny, got %v", in, d)
		}
	}
}

// TestPromptLiteral_W3RejectsEvetAcceptsOnayla is this task's own
// acceptance criterion, verbatim: the W3 CLI prompt rejects "evet",
// accepts "onayla" (case-sensitive, exact match only).
func TestPromptLiteral_W3RejectsEvetAcceptsOnayla(t *testing.T) {
	reject := []string{"evet", "e", "y", "yes", "Onayla", "ONAYLA", "onayla ", " onayla", ""}
	for _, in := range reject {
		r := bufio.NewReader(strings.NewReader(in + "\n"))
		var w strings.Builder
		d, _ := PromptLiteral(r, &w, "onayla yazın: ", "onayla")
		if d != DecisionDeny {
			t.Fatalf("input %q: expected DecisionDeny for anything but exact 'onayla', got %v", in, d)
		}
	}

	r := bufio.NewReader(strings.NewReader("onayla\n"))
	var w strings.Builder
	d, err := PromptLiteral(r, &w, "onayla yazın: ", "onayla")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != DecisionApprove {
		t.Fatalf("expected DecisionApprove for exact 'onayla', got %v", d)
	}
}

func TestPromptLiteral_WritesPromptText(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("onayla\n"))
	var w strings.Builder
	const prompt = "Bu eylem geri alınamaz (W3). Devam etmek için 'onayla' yazın:"
	if _, err := PromptLiteral(r, &w, prompt, "onayla"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(w.String(), prompt) {
		t.Fatalf("expected prompt text written verbatim, got %q", w.String())
	}
}

func TestFormatApprovalsList_ContainsAllFields(t *testing.T) {
	items := []PendingApprovalSummary{
		{ID: "abc123", Tool: "fs_write", Class: "W1", Summary: "fs_write: ~/x.txt", Age: 90 * time.Second},
	}
	out := FormatApprovalsList(items)
	for _, want := range []string{"abc123", "fs_write", "W1", "~/x.txt", "1m30s"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q in formatted list, got: %s", want, out)
		}
	}
}
