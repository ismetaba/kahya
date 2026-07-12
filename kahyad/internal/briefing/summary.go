// summary.go defines the W5-01 briefing worker's output schema
// (briefing_summary_v1) and its Go-side validation - HANDOFF §5 safety #2:
// "yalniz Okuyucu'nun Go-tarafinda struct/sema-dogrulanmis ciktisiyla
// (serbest-metin alanlari uzunluk+karakter-sinifi kisitli)". Mirrors
// kahyad/internal/reader/schemas.go's own sanitizeField pattern
// (canon.Normalize + a rejected-outright-never-silently-truncated control-
// rune/length check) - duplicated here rather than imported, since that
// package's validators are keyed to its own two job types and this
// package must not take a dependency on kahyad/internal/reader just to
// reuse one small helper.
package briefing

import (
	"fmt"
	"unicode"
	"unicode/utf8"

	"kahya/kahyad/internal/canon"
)

// Field ceilings (this task's own choice, mirroring the "Turkish text is
// often multi-byte per rune" rationale kahyad/internal/reader/schemas.go
// already documents - RUNE counts, not byte counts): at most
// summaryMaxLines lines (task spec step 5/HANDOFF §5 #2: "en fazla 15
// satir"), each at most summaryLineMaxLen runes (a briefing line is a
// short bullet, not a paragraph).
const (
	summaryMaxLines   = 15
	summaryLineMaxLen = 200
)

// BriefingSummaryV1 is the registered Go struct for SummaryJobType - the
// ONLY shape the briefing worker's terminal JSON output may take.
type BriefingSummaryV1 struct {
	Lines []string `json:"lines"`
}

// ValidateBriefingSummaryV1 sanitizes and validates every field of v,
// returning a NEW, fully validated BriefingSummaryV1 - never a partially
// valid one (same no-partial-output contract as
// kahyad/internal/reader.ValidateMailSummaryV1/ValidateWebpageExtractV1:
// the first invalid line fails the WHOLE summary, never silently drops
// just that one line - a length/charclass violation is evidence the
// output cannot be trusted at all, not a cosmetic defect to paper over).
func ValidateBriefingSummaryV1(v BriefingSummaryV1) (BriefingSummaryV1, error) {
	if len(v.Lines) > summaryMaxLines {
		return BriefingSummaryV1{}, fmt.Errorf("briefing: summary has %d lines, exceeds max %d", len(v.Lines), summaryMaxLines)
	}
	out := make([]string, 0, len(v.Lines))
	for i, l := range v.Lines {
		s, err := sanitizeSummaryLine(fmt.Sprintf("lines[%d]", i), l, summaryLineMaxLen)
		if err != nil {
			return BriefingSummaryV1{}, err
		}
		out = append(out, s)
	}
	return BriefingSummaryV1{Lines: out}, nil
}

// sanitizeSummaryLine runs the W3-06 sanitizer (canon.Normalize) then
// rejects - rather than silently stripping - any control/bidi/zero-width
// code point or an over-length result. maxLen is a rune count.
func sanitizeSummaryLine(field, raw string, maxLen int) (string, error) {
	res := canon.Normalize(raw)
	if res.HasControlFlags() {
		return "", fmt.Errorf("%s: contains a bidi/zero-width/format control code point", field)
	}
	if hasDisallowedControlRune(res.Canonical) {
		return "", fmt.Errorf("%s: contains a disallowed control character", field)
	}
	if n := utf8.RuneCountInString(res.Canonical); n > maxLen {
		return "", fmt.Errorf("%s: exceeds max length %d (got %d)", field, maxLen, n)
	}
	return res.Canonical, nil
}

// hasDisallowedControlRune reports whether s contains a newline/tab or any
// Unicode control character - mirrors kahyad/internal/reader/schemas.go's
// identically-named, identically-implemented helper (duplicated per this
// codebase's own "small per-package helper copy" convention - see that
// file's doc comment on sanitizeField). A briefing line is a single-line
// bullet; a legitimate one never needs an embedded newline or tab.
func hasDisallowedControlRune(s string) bool {
	for _, r := range s {
		if r == '\n' || r == '\t' || unicode.IsControl(r) {
			return true
		}
	}
	return false
}
