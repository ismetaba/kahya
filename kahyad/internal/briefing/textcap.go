// textcap.go implements the length+charclass cap every collector in this
// package applies to a free-text field BEFORE it ever becomes a
// CollectedItem (task spec deliverable: "collectors returning TYPED
// structs with LENGTH-CAPPED + charclass-constrained free-text fields,
// never unbounded raw text"). Unlike summary.go's sanitizeSummaryLine
// (which REJECTS an over-length/control-bearing worker OUTPUT outright -
// evidence the model's output cannot be trusted at all), capText
// SILENTLY NORMALIZES a collector's raw INPUT (a PR title, a calendar
// event summary, a filename) by replacing control/newline runes with a
// space and truncating - a collector is reporting on already-existing,
// arbitrary third-party text (a GitHub PR title, a Calendar event
// summary), never validating a security-relevant claim about it, so
// clamping is the appropriate behavior here, not failing the whole
// collector closed over one long PR title.
package briefing

import (
	"strings"
	"unicode"
)

// capText returns s with every newline/tab/other Unicode control rune
// replaced by a single space, runs of whitespace collapsed, and the
// result truncated to at most maxLen runes (never bytes - Turkish text is
// frequently multi-byte per rune, and a byte cap would wrongly penalize
// it, mirroring kahyad/internal/reader/schemas.go's own rune-count
// rationale).
func capText(s string, maxLen int) string {
	var b strings.Builder
	count := 0
	lastWasSpace := false
	for _, r := range s {
		if count >= maxLen {
			break
		}
		if r == '\n' || r == '\r' || r == '\t' || unicode.IsControl(r) {
			r = ' '
		}
		if r == ' ' {
			if lastWasSpace {
				continue
			}
			lastWasSpace = true
		} else {
			lastWasSpace = false
		}
		b.WriteRune(r)
		count++
	}
	return strings.TrimSpace(b.String())
}
