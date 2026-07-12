// evidentiality.go implements the deterministic half of HANDOFF S5's
// schema-block evidentiality rule: "-mis morfolojisi: witnessed|reported
// |inferred" - reported speech via the Turkish -mis/-mis/-mus/-mus
// (dotless-i/dotted-i/u/u-umlaut vowel-harmony) suffix family means
// `reported`; a direct assertion/observation with no such suffix means
// `witnessed`; the engine's own fallback when nothing at all is known
// (empty candidate, or no text to run morphology against) is `inferred`
// (NormalizeEvidentiality in engine.go owns THAT default).
//
// This is deliberately a lexical heuristic, not a morphological parser
// (tasks/README.md forbids hand-rolled stemmer tables): it checks whether
// the FOLDED text (kahyad/internal/textnorm.Fold - the same Turkish
// dotted/dotless-I collapse the trigram search leg uses) contains one of
// the three folded suffix forms as a substring, matching this codebase's
// existing "representative lexicon, not exhaustive" convention
// (kahyad/internal/secretlane's keyword tables, kahyad/internal/
// consolidation's decisionOrPromiseKeywords).
//
// Fold's own dotless-ι(U+0131)->i collapse (textnorm.Fold's documented
// step 4) means "-mis" (dotted i) and "-mis" (dotless ι) already fold to
// the IDENTICAL string "mis" - so only three distinct folded substrings
// ever need checking: "mis", "mus", "mus" (front-rounded u-umlaut).
package factengine

import (
	"strings"

	"kahya/kahyad/internal/textnorm"
)

// reportedSuffixes are the three DISTINCT folded forms of the four -mis
// suffix vowel-harmony variants (dotted-i and dotless-ι collapse to one
// folded form via textnorm.Fold - see this file's own doc comment).
var reportedSuffixes = []string{"miş", "muş", "müş"}

// DetectEvidentialityFromText runs the deterministic -mis-morphology
// detector over quote (the raw utterance an extractor is proposing a
// candidate from) and returns Reported when a reported-speech suffix is
// found, Witnessed otherwise (HANDOFF S5 schema block: "direct
// assertion/observation => witnessed"). This function never returns
// Inferred - that fallback is for when there is no text to analyze at
// all (engine.NormalizeEvidentiality's own default), not a morphological
// outcome.
func DetectEvidentialityFromText(quote string) string {
	folded := textnorm.Fold(quote)
	for _, suf := range reportedSuffixes {
		if strings.Contains(folded, suf) {
			return Reported
		}
	}
	return Witnessed
}
