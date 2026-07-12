package briefing

import "testing"

func TestValidateBriefingSummaryV1Valid(t *testing.T) {
	v, err := ValidateBriefingSummaryV1(BriefingSummaryV1{Lines: []string{"3 açık PR var.", "Bugün 2 takvim etkinliği."}})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(v.Lines) != 2 {
		t.Fatalf("Lines = %v, want 2 entries", v.Lines)
	}
}

func TestValidateBriefingSummaryV1RejectsTooManyLines(t *testing.T) {
	lines := make([]string, summaryMaxLines+1)
	for i := range lines {
		lines[i] = "line"
	}
	if _, err := ValidateBriefingSummaryV1(BriefingSummaryV1{Lines: lines}); err == nil {
		t.Fatal("Validate: want error for > summaryMaxLines lines")
	}
}

func TestValidateBriefingSummaryV1RejectsOverLongLine(t *testing.T) {
	long := make([]byte, summaryLineMaxLen+1)
	for i := range long {
		long[i] = 'a'
	}
	if _, err := ValidateBriefingSummaryV1(BriefingSummaryV1{Lines: []string{string(long)}}); err == nil {
		t.Fatal("Validate: want error for an over-length line")
	}
}

func TestValidateBriefingSummaryV1RejectsNewlineInLine(t *testing.T) {
	if _, err := ValidateBriefingSummaryV1(BriefingSummaryV1{Lines: []string{"line one\nline two"}}); err == nil {
		t.Fatal("Validate: want error for an embedded newline")
	}
}

func TestValidateBriefingSummaryV1RejectsBidiOverride(t *testing.T) {
	// U+202E RIGHT-TO-LEFT OVERRIDE - canon.Normalize's own Cf-category
	// control-flag detection (mirrors kahyad/internal/reader/schemas_test.go's
	// identical fixture).
	if _, err := ValidateBriefingSummaryV1(BriefingSummaryV1{Lines: []string{"safe‮attack"}}); err == nil {
		t.Fatal("Validate: want error for a bidi override code point")
	}
}

func TestValidateBriefingSummaryV1RejectsTooManyEntriesNoPartialOutput(t *testing.T) {
	lines := []string{"ok one", "ok two"}
	for i := 0; i < summaryMaxLines; i++ {
		lines = append(lines, "filler")
	}
	v, err := ValidateBriefingSummaryV1(BriefingSummaryV1{Lines: lines})
	if err == nil {
		t.Fatal("Validate: want error")
	}
	if len(v.Lines) != 0 {
		t.Fatalf("Validate must return a zero value on failure (no partial output), got %+v", v)
	}
}
