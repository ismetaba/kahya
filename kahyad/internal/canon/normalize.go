// Package canon implements the W3-06 WYSIWYE text-canonicalization
// primitive (HANDOFF §5 safety #5): NFC-normalize, strip/flag bidi and
// zero-width control code points, and flag (never silently rewrite)
// mixed-script and confusable-with-ASCII tokens. kahyad/internal/approval
// builds every ApprovalPayload's canonical bytes and rendered diff on top
// of this package; kahyad/internal/policy's approvedBytesHash routes every
// approval-token hash through it too, so the exact same canonicalization
// applies whether a payload is being MINTED (approval time) or REBUILT
// (execution time) — the whole WYSIWYE invariant.
//
// Homoglyphs are never "fixed": Normalize never rewrites a confusable rune
// into its ASCII look-alike. It only ever (a) strips bidi/zero-width/other
// Cf-category control code points from the Canonical/hash-basis form
// (while still rendering each one, visibly, in Display) and (b) flags
// mixed-script or confusable-with-ASCII TOKENS so a human reviewer sees
// reality, per this task's own instruction: "you cannot 'fix' homoglyphs -
// you must make them visible".
//
// The Turkish alphabet (ı İ ş ğ ç ö ü, and their uppercase forms) is
// expected content, NOT a homoglyph signal: every one of those code points
// has Unicode Script=Latin, so a pure-Turkish token never trips the
// mixed-script check (single script throughout), and the confusable-with-
// ASCII check below is deliberately restricted to NON-Latin-script runes
// only — seeing this package's own confusables_test.go, the vendored data
// WOULD otherwise map "ı" (dotless i, Script=Latin) straight to ASCII "i",
// which is exactly the false positive HANDOFF explicitly forbids.
package canon

import (
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// FlagKind labels one Flag's category.
type FlagKind string

const (
	// FlagBidi marks a bidi-control code point (U+202A-U+202E LTR/RTL
	// embedding/override, U+2066-U+2069 isolates, U+200E/U+200F
	// direction marks) — the classic "Trojan Source" vector, and this
	// task's own bidi_filename fixture.
	FlagBidi FlagKind = "bidi"
	// FlagZeroWidth marks a zero-width/invisible code point (U+200B-
	// U+200D, U+2060, U+FEFF) — this task's own zero_width_url fixture.
	FlagZeroWidth FlagKind = "zero_width"
	// FlagFormatOther marks any OTHER Unicode Cf (format) category code
	// point not already covered by FlagBidi/FlagZeroWidth — the task
	// spec's "diğer Cf kategorisi varsayılanları" catch-all.
	FlagFormatOther FlagKind = "format_other"
	// FlagMixedScript marks a TOKEN (not a single rune) whose letters
	// span more than one non-Common Unicode script — e.g. Latin mixed
	// with Cyrillic, this task's own homoglyph_host fixture ("pаypal",
	// Cyrillic а among Latin letters).
	FlagMixedScript FlagKind = "mixed_script"
	// FlagConfusable marks a token containing a NON-Latin-script rune
	// whose UTS #39 confusables.txt skeleton is pure ASCII — a
	// same-script-looking (or single-script) impersonation the
	// mixed-script check alone would miss (e.g. an all-Cyrillic domain
	// label impersonating a Latin one).
	FlagConfusable FlagKind = "confusable"
)

// Flag is one detected-and-surfaced canonicalization event. Rune is set
// (non-zero) for a single-code-point control flag (Bidi/ZeroWidth/
// FormatOther); Token is set for a whole-token flag (MixedScript/
// Confusable). Never both.
type Flag struct {
	Kind  FlagKind
	Rune  rune   // set for Bidi/ZeroWidth/FormatOther
	Token string // set for MixedScript/Confusable
}

// Result is Normalize's output.
type Result struct {
	// Raw is the original input, byte-exact, untouched.
	Raw string
	// NFC is Raw NFC-normalized (golang.org/x/text/unicode/norm), with
	// every code point still present — the basis Canonical/Display are
	// both built from.
	NFC string
	// Canonical is NFC with every bidi/zero-width/other-Cf-category
	// control code point STRIPPED (never a confusable rune — those are
	// never rewritten, only flagged). This is the hash-basis /
	// allowlist-matching form: kahyad/internal/approval.ApprovalPayload
	// hashes this, never Raw or NFC directly, so an NFD-vs-NFC input
	// difference collapses to the identical Canonical string (and
	// therefore the identical hash), while a zero-width-smuggled host
	// canonicalizes to the real host it was hiding inside.
	Canonical string
	// Display is NFC with every stripped control code point replaced by
	// a VISIBLE escape (e.g. "<U+202E>") instead of removed — this task's
	// own instruction: "never dropped invisibly". kahyad/internal/
	// approval/diff.go renders this, never Canonical, to the user.
	Display string
	// Flags lists every bidi/zero-width/format-other code point stripped
	// (Kind+Rune) and every mixed-script/confusable token detected
	// (Kind+Token), in the order encountered.
	Flags []Flag
}

// HasControlFlags reports whether Result has any Bidi/ZeroWidth/
// FormatOther flag (i.e. Canonical differs from NFC) — a convenience for
// callers that just need a yes/no signal without inspecting Flags
// themselves.
func (r Result) HasControlFlags() bool {
	for _, f := range r.Flags {
		switch f.Kind {
		case FlagBidi, FlagZeroWidth, FlagFormatOther:
			return true
		}
	}
	return false
}

// bidiControlRunes are the explicit bidi-control code points this
// package's own spec names (U+202A-U+202E, U+2066-U+2069), plus the two
// directional marks (U+200E/U+200F) mcp/fs/paths.go's own equivalent list
// already includes for the identical reason (a directional-mark-only
// filename is just as capable of visually reordering a rendered name).
var bidiControlRunes = buildRuneSet(
	0x200E, 0x200F, // LEFT/RIGHT-TO-LEFT MARK
	0x202A, 0x202B, 0x202C, 0x202D, 0x202E, // LRE/RLE/PDF/LRO/RLO
	0x2066, 0x2067, 0x2068, 0x2069, // LRI/RLI/FSI/PDI
)

// zeroWidthRunes are the explicit zero-width code points this package's
// own spec names (U+200B-U+200D, U+FEFF, U+2060).
var zeroWidthRunes = buildRuneSet(
	0x200B, 0x200C, 0x200D, // ZERO WIDTH SPACE/NON-JOINER/JOINER
	0x2060, // WORD JOINER
	0xFEFF, // ZERO WIDTH NO-BREAK SPACE / BOM
)

func buildRuneSet(codes ...int32) map[rune]bool {
	m := make(map[rune]bool, len(codes))
	for _, c := range codes {
		m[rune(c)] = true
	}
	return m
}

// controlFlagKind classifies r as a control code point this package
// strips, if any: the explicit bidi/zero-width sets above take priority
// (so their Kind is always Bidi/ZeroWidth, never the FormatOther
// catch-all), falling back to "any other Unicode Cf (format) category code
// point" (this task's own spec: "diğer Cf kategorisi varsayılanları").
func controlFlagKind(r rune) (FlagKind, bool) {
	switch {
	case bidiControlRunes[r]:
		return FlagBidi, true
	case zeroWidthRunes[r]:
		return FlagZeroWidth, true
	case unicode.Is(unicode.Cf, r):
		return FlagFormatOther, true
	default:
		return "", false
	}
}

// escapeRune renders r as a visible "<U+XXXX>" escape (at least 4 hex
// digits, uppercase) — this task's own spec example, verbatim.
func escapeRune(r rune) string {
	return fmt.Sprintf("<U+%04X>", r)
}

// Normalize is this package's one entry point: NFC-normalize s, strip/flag
// bidi/zero-width/other-Cf control code points, and flag (never rewrite)
// mixed-script/confusable-with-ASCII tokens.
func Normalize(s string) Result {
	nfc := norm.NFC.String(s)

	var canonical, display strings.Builder
	canonical.Grow(len(nfc))
	display.Grow(len(nfc))
	var flags []Flag

	for _, r := range nfc {
		if kind, ok := controlFlagKind(r); ok {
			flags = append(flags, Flag{Kind: kind, Rune: r})
			display.WriteString(escapeRune(r))
			continue // stripped from Canonical - never part of the hash basis
		}
		canonical.WriteRune(r)
		display.WriteRune(r)
	}

	flags = append(flags, scanTokenFlags(nfc)...)

	return Result{
		Raw:       s,
		NFC:       nfc,
		Canonical: canonical.String(),
		Display:   display.String(),
		Flags:     flags,
	}
}

// CanonicalizeBytes applies Normalize to b (treated as UTF-8 text — every
// current caller hashes either a JSON envelope or a text script/argument
// listing, both always valid UTF-8) and returns the Canonical form's own
// bytes — kahyad/internal/policy's approvedBytesHash hashes THIS, never
// the raw input, so NFD-vs-NFC input collapses to an identical hash while
// a genuine byte-level mutation (a homoglyph swap, an added trailing
// space, ...) still produces a different Canonical string and therefore a
// different hash.
func CanonicalizeBytes(b []byte) []byte {
	return []byte(Normalize(string(b)).Canonical)
}

// isTokenRune reports whether r is part of a "word" for tokenization
// purposes (mixed-script/confusable scanning runs per-token, not
// per-rune, so one warning is produced per offending word, not one per
// offending code point within it) — any Unicode letter or digit; every
// other rune (whitespace, punctuation, path/URL separators like '.', '/',
// ':', "'") is a token boundary.
func isTokenRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// scanTokenFlags tokenizes s (already NFC-normalized) on non-letter/digit
// boundaries and flags each token that mixes Unicode scripts or contains a
// non-Latin-script rune confusable with ASCII.
func scanTokenFlags(s string) []Flag {
	var flags []Flag
	for _, tok := range splitTokens(s) {
		if isASCIIString(tok) {
			continue // nothing exotic to check
		}
		if kind, flagged := classifyToken(tok); flagged {
			flags = append(flags, Flag{Kind: kind, Token: tok})
		}
	}
	return flags
}

// splitTokens splits s into maximal runs of isTokenRune code points,
// discarding everything else.
func splitTokens(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return !isTokenRune(r) })
}

// classifyToken reports the single flag tok deserves (mixed-script takes
// priority over confusable-with-ASCII: a token already flagged as
// mixed-script is unambiguously suspicious, and reporting AND both would
// just be noise), or ok=false for a clean token.
func classifyToken(tok string) (FlagKind, bool) {
	scripts := make(map[string]bool, 2)
	for _, r := range tok {
		if sc := scriptOf(r); sc != "" {
			scripts[sc] = true
		}
	}
	if len(scripts) > 1 {
		return FlagMixedScript, true
	}

	// Single (or no) resolvable script across the token: fall back to a
	// per-rune confusable-with-ASCII check, restricted to non-Latin-script
	// runes (see this file's package doc comment for why the Latin-script
	// exclusion is load-bearing for Turkish text specifically).
	for _, r := range tok {
		if r <= unicode.MaxASCII {
			continue
		}
		if scriptOf(r) == "Latin" {
			continue
		}
		skeleton, ok := confusableSkeletonFor(r)
		if ok && isASCIIString(skeleton) {
			return FlagConfusable, true
		}
	}
	return "", false
}

// scriptOf resolves r's Unicode Script property value among the common
// scripts this package cares about for spoof-detection, returning "" for
// Common/Inherited (script-neutral: digits, combining marks with no
// script of their own) or any script not in this list. unicode.Scripts
// (Go stdlib) has ~150 entries; checking a curated subset first (the
// scripts realistically involved in Latin-look-alike spoofing, per UTS
// #39's own "Restricted"/"Highly Restrictive" script list) keeps this
// cheap and avoids ever needing to special-case Common/Inherited
// separately — they are simply absent from the set below.
var scriptsOfInterest = []string{
	"Latin", "Cyrillic", "Greek", "Armenian", "Hebrew", "Arabic",
	"Han", "Hiragana", "Katakana", "Hangul", "Devanagari", "Thai",
	"Georgian", "Cherokee",
}

func scriptOf(r rune) string {
	for _, name := range scriptsOfInterest {
		if table, ok := unicode.Scripts[name]; ok && unicode.Is(table, r) {
			return name
		}
	}
	return ""
}
