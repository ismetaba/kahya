// schemas.go defines the W4-03 Reader output schemas: mail_summary_v1 and
// webpage_extract_v1 (task spec step 5). Every validator is a hand-
// written Go function against stdlib types ONLY - no JSON-Schema
// library (HANDOFF §4/§9 name no such dependency, and CLAUDE.md forbids
// inventing one). Every free-text field runs the W3-06 sanitizer
// (kahyad/internal/canon.Normalize), THEN is rejected outright (never
// silently truncated/stripped) on any control/bidi/zero-width code point
// or an over-length result - task spec step 5, verbatim: "Validation
// failure => Reader job fails closed (no partial output)".
package reader

import (
	"fmt"
	"regexp"
	"time"
	"unicode"
	"unicode/utf8"

	"kahya/kahyad/internal/canon"
)

// JobType values - the registered schema names task spec step 5 fixes.
const (
	JobTypeMailSummary    = "mail_summary_v1"
	JobTypeWebpageExtract = "webpage_extract_v1"
)

// Field length ceilings (task spec step 5, verbatim char counts - RUNE
// counts in this implementation, since Turkish text has plenty of
// multi-byte, single-rune characters a byte-length cap would wrongly
// penalize).
const (
	mailFromDisplayMaxLen = 120
	mailSubjectMaxLen     = 200
	mailSummaryMaxLen     = 500
	webpageTitleMaxLen    = 200
	webpageKeyPointMaxLen = 300
	webpageKeyPointsMax   = 10
)

// MailSummaryV1 is the registered Go struct for JobTypeMailSummary.
type MailSummaryV1 struct {
	FromDisplay string   `json:"from_display"`
	Subject     string   `json:"subject"`
	Summary     string   `json:"summary"`
	Dates       []string `json:"dates"`
	Amounts     []string `json:"amounts"`
}

// WebpageExtractV1 is the registered Go struct for JobTypeWebpageExtract.
type WebpageExtractV1 struct {
	Title     string   `json:"title"`
	KeyPoints []string `json:"key_points"`
}

// amountRe is the task spec's own pattern, verbatim: `^[0-9.,]+ ?(TL|USD|EUR)$`.
var amountRe = regexp.MustCompile(`^[0-9.,]+ ?(TL|USD|EUR)$`)

// hasDisallowedControlRune reports whether s contains a newline/tab or any
// Unicode control character (unicode.IsControl - the Latin-1 Cc range:
// NUL, ESC, a bare CR, ...). canon.Normalize's own HasControlFlags already
// catches the Trojan-Source-style Cf category (bidi/zero-width/other
// format controls); this catches the more mundane ASCII-control-character
// class the task spec's char-class validators must ALSO reject (step 5:
// "control chars, bidi, zero-width, or over-length") - every Reader
// schema field is a single-line value (a subject, a summary, a title, one
// key point), so a legitimate extraction never needs an embedded newline
// or tab either.
func hasDisallowedControlRune(s string) bool {
	for _, r := range s {
		if r == '\n' || r == '\t' || unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// sanitizeField runs the W3-06 sanitizer (canon.Normalize) then rejects -
// rather than silently stripping - any control/bidi/zero-width code point
// or an over-length result. maxLen is a rune count.
func sanitizeField(fieldName, raw string, maxLen int) (string, error) {
	res := canon.Normalize(raw)
	if res.HasControlFlags() {
		return "", fmt.Errorf("%s: contains a bidi/zero-width/format control code point", fieldName)
	}
	if hasDisallowedControlRune(res.Canonical) {
		return "", fmt.Errorf("%s: contains a disallowed control character", fieldName)
	}
	if n := utf8.RuneCountInString(res.Canonical); n > maxLen {
		return "", fmt.Errorf("%s: exceeds max length %d (got %d)", fieldName, maxLen, n)
	}
	return res.Canonical, nil
}

// ValidateMailSummaryV1 sanitizes and validates every field of v,
// returning a NEW, fully validated MailSummaryV1 - never a partially
// valid one (task spec step 5: Reader job fails closed with NO partial
// output on the first failure).
func ValidateMailSummaryV1(v MailSummaryV1) (MailSummaryV1, error) {
	from, err := sanitizeField("from_display", v.FromDisplay, mailFromDisplayMaxLen)
	if err != nil {
		return MailSummaryV1{}, err
	}
	subject, err := sanitizeField("subject", v.Subject, mailSubjectMaxLen)
	if err != nil {
		return MailSummaryV1{}, err
	}
	summary, err := sanitizeField("summary", v.Summary, mailSummaryMaxLen)
	if err != nil {
		return MailSummaryV1{}, err
	}

	dates := make([]string, 0, len(v.Dates))
	for i, d := range v.Dates {
		if _, perr := time.Parse(time.RFC3339, d); perr != nil {
			return MailSummaryV1{}, fmt.Errorf("dates[%d]: %q is not RFC3339: %w", i, d, perr)
		}
		dates = append(dates, d)
	}

	amounts := make([]string, 0, len(v.Amounts))
	for i, a := range v.Amounts {
		if !amountRe.MatchString(a) {
			return MailSummaryV1{}, fmt.Errorf("amounts[%d]: %q does not match ^[0-9.,]+ ?(TL|USD|EUR)$", i, a)
		}
		amounts = append(amounts, a)
	}

	return MailSummaryV1{
		FromDisplay: from, Subject: subject, Summary: summary,
		Dates: dates, Amounts: amounts,
	}, nil
}

// ValidateWebpageExtractV1 sanitizes and validates every field of v,
// returning a NEW, fully validated WebpageExtractV1 - same no-partial-
// output contract as ValidateMailSummaryV1.
func ValidateWebpageExtractV1(v WebpageExtractV1) (WebpageExtractV1, error) {
	title, err := sanitizeField("title", v.Title, webpageTitleMaxLen)
	if err != nil {
		return WebpageExtractV1{}, err
	}
	if len(v.KeyPoints) > webpageKeyPointsMax {
		return WebpageExtractV1{}, fmt.Errorf("key_points: %d entries exceeds max %d", len(v.KeyPoints), webpageKeyPointsMax)
	}

	points := make([]string, 0, len(v.KeyPoints))
	for i, p := range v.KeyPoints {
		sanitized, perr := sanitizeField(fmt.Sprintf("key_points[%d]", i), p, webpageKeyPointMaxLen)
		if perr != nil {
			return WebpageExtractV1{}, perr
		}
		points = append(points, sanitized)
	}

	return WebpageExtractV1{Title: title, KeyPoints: points}, nil
}
