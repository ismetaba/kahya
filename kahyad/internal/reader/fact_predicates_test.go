package reader

import (
	"strings"
	"testing"
)

// TestExtractionPromptsNameExactlyFactPredicates (FIX-FINAL TUX-1): the
// predicate list both extraction prompts give the model is factPredicates,
// in order. A surface that shows facts to the user labels this vocabulary
// (kahyad/internal/ritual), so a predicate added to a prompt alone would
// reach the user as a raw identifier.
func TestExtractionPromptsNameExactlyFactPredicates(t *testing.T) {
	list := "from this list when it fits: " + strings.Join(factPredicates, ", ") + " - otherwise other:<snake_case>"
	for name, prompt := range map[string]string{
		"factExtractSystemPrompt": factExtractSystemPrompt,
		"docIngestSystemPrompt":   docIngestSystemPrompt,
	} {
		if got := strings.Count(prompt, list); got != 1 {
			t.Errorf("%s names the predicate list %d times, want exactly once: %q", name, got, list)
		}
	}
	for _, p := range factPredicates {
		if !predicateRe.MatchString(p) || strings.HasPrefix(p, "other:") {
			t.Errorf("factPredicates entry %q is not a plain predicate the validator accepts", p)
		}
	}
	got := FactPredicates()
	got[0] = "changed"
	if factPredicates[0] == "changed" {
		t.Fatal("FactPredicates must return a copy")
	}
}
