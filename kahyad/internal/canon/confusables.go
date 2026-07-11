// confusables.go embeds and parses the vendored Unicode UTS #39
// confusables.txt data file (kahyad/internal/canon/data/confusables.txt,
// pinned at Unicode Security Mechanisms version 17.0.0 — see that file's
// own header comment for provenance) EXACTLY ONCE, at package init. This
// package NEVER fetches the data over the network at runtime and never
// hand-rolls a substitute table — HANDOFF §5 safety #5's WYSIWYE
// confusable-detection requirement is explicit that this must be the real
// Unicode data, vendored and committed.
package canon

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"

	_ "embed"
)

//go:embed data/confusables.txt
var confusablesRaw []byte

// confusableSkeleton maps a single confusable SOURCE rune to its
// confusables.txt TARGET string (the "skeleton" one or more code points it
// is recommended to be treated as equivalent to for spoof-detection
// purposes — HANDOFF §5 safety #5: "confusable-with-ASCII code points are
// highlighted"). Only single-rune SOURCE entries are indexed (the
// overwhelming majority of the file — see confusables_test.go's own count
// assertion); the rare multi-rune-source contextual entries are not needed
// for this package's per-token confusable-with-ASCII check and are
// skipped.
var confusableSkeleton = parseConfusables(confusablesRaw)

// parseConfusables parses raw confusables.txt content (the Unicode
// Consortium's own "SOURCE ; TARGET ; TYPE # comment" line format, one
// entry per non-comment, non-blank line — TYPE is one of SL/SA/ML/MA,
// unused by this package) into a source-rune -> target-skeleton-string
// map. Lines are semicolon-delimited; splitting with a fixed limit of 3
// tolerates the one line in the real data file whose COMMENT itself
// contains a literal semicolon (037E ; 003B ; MA #* ( ; -> ; ) ...) without
// misparsing SOURCE/TARGET.
func parseConfusables(raw []byte) map[rune]string {
	out := make(map[rune]string, 6600)
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	// Individual data lines are short, but be generous regardless — a
	// vendored file this package does not control should never make init
	// panic on an unexpectedly long line.
	scanner.Buffer(make([]byte, 0, 4096), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.SplitN(line, ";", 3)
		if len(fields) < 2 {
			continue
		}
		sourceField := strings.TrimSpace(fields[0])
		targetField := strings.TrimSpace(fields[1])

		sourceRunes := parseHexRunes(sourceField)
		if len(sourceRunes) != 1 {
			// Multi-rune (contextual) sources are not needed for a per-rune
			// confusable-with-ASCII lookup — skip rather than mis-key.
			continue
		}
		targetRunes := parseHexRunes(targetField)
		if len(targetRunes) == 0 {
			continue
		}
		out[sourceRunes[0]] = string(targetRunes)
	}
	return out
}

// parseHexRunes parses a whitespace-separated list of 4-6 hex-digit
// Unicode code point values (confusables.txt's own SOURCE/TARGET field
// syntax) into runes. Any field that fails to parse is skipped rather than
// aborting the whole line — this is vendored third-party data, not this
// package's own format to enforce strictly.
func parseHexRunes(field string) []rune {
	parts := strings.Fields(field)
	runes := make([]rune, 0, len(parts))
	for _, p := range parts {
		v, err := strconv.ParseUint(p, 16, 32)
		if err != nil {
			continue
		}
		runes = append(runes, rune(v))
	}
	return runes
}

// confusableSkeletonFor looks up r's confusables.txt target skeleton,
// reporting ok=false if r has no entry.
func confusableSkeletonFor(r rune) (string, bool) {
	s, ok := confusableSkeleton[r]
	return s, ok
}

// isASCIIString reports whether every rune in s is in the ASCII range —
// used to decide whether a confusable's skeleton "maps to ASCII" (HANDOFF
// §5 safety #5's own wording), since a skeleton that itself decomposes
// into further non-ASCII combining marks (e.g. "ç" -> "c" + COMBINING
// COMMA BELOW) is not a same-script Latin/ASCII look-alike in the sense
// this package flags.
func isASCIIString(s string) bool {
	for _, r := range s {
		if r > 127 {
			return false
		}
	}
	return true
}
