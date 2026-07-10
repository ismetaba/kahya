// Package textnorm provides the deterministic text folding kahyad's
// trigram search leg needs for Turkish suffix-/I-i-insensitive partial
// matching (HANDOFF S4 stack row flag: "FTS5 cift indeks"). Fold runs at
// FTS5 index time (chunks_fts_tri.text_folded, see
// kahyad/internal/search/ftswrite.go) and at query time
// (kahyad/internal/search/search.go) using the exact same function, so the
// two sides can never drift out of sync. Stored chunks.text stays
// byte-exact - folding only ever produces a derived copy for the trigram
// index and the query string.
//
// This is deliberately NOT a Turkish stemmer/morphological analyzer
// (tasks/README.md forbids manual suffix tables): Fold only removes the
// dotted/dotless-I ambiguity plus general Unicode case, nothing else.
// Turkish suffix matching ("evlerimizden" finding "ev") comes entirely
// from the trigram tokenizer's partial matching plus
// kahyad/internal/search's character-truncation relaxation ladder, not
// from anything in this package.
package textnorm

import (
	"strings"

	"golang.org/x/text/unicode/norm"
)

// Fold produces the canonical trigram-matching form of s:
//
//  1. Unicode NFC normalization (golang.org/x/text/unicode/norm), so a
//     precomposed character and its decomposed base+combining-mark
//     equivalent fold identically.
//  2. Turkish dotted/dotless-I mapping, BEFORE the general lowercase pass:
//     neither Go's strings.ToLower nor SQLite's own casing fold Turkish
//     I/i correctly (both would map 'İ' -> 'i̇' with a stray combining dot,
//     and leave 'I' as-is instead of turning it into 'ı').
//     - 'İ' (U+0130 LATIN CAPITAL LETTER I WITH DOT ABOVE)  -> 'i'
//     - 'I' (U+0049 LATIN CAPITAL LETTER I)                 -> 'ı' (U+0131)
//  3. Unicode lowercase (strings.ToLower) for every other rune - this is
//     a no-op on the 'i'/'ı' produced by step 2, and handles every other
//     letter (including every other Turkish/diacritic letter, which is
//     otherwise left alone: ö, ü, ş, ç, ğ, â, ... are KEPT, never
//     stripped or ASCII-folded).
//  4. A final 'ı' (U+0131) -> 'i' pass, so the net effect collapses all
//     four members of the Turkish I-family (İ, I, i, ı) onto plain ASCII
//     'i': Fold("Iıİi") == "iiii" (see fold_test.go).
func Fold(s string) string {
	s = norm.NFC.String(s)

	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case 'İ': // U+0130
			r = 'i'
		case 'I': // U+0049
			r = 'ı' // U+0131
		}
		b.WriteRune(r)
	}

	folded := strings.ToLower(b.String())
	return strings.ReplaceAll(folded, "ı", "i")
}
