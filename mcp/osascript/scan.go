// scan.go implements W3-09's static body scan: the REJECT filter every
// applescript_run/jxa_run script body runs through BEFORE any policy
// decision (mechanical, never approval-overridable — mirrors mcp/fs's
// deny-glob-before-approval and mcp/shell's workdir-scope-before-approval
// ordering, HANDOFF §5 safety #6).
//
// This scanner is explicitly NOT a security boundary (this task's own
// spec, verbatim: "NOT a security boundary — the byte-exact human
// approval IS the boundary; the scanner just refuses the obviously
// shell-shaped class the handoff names"). It exists solely to catch the
// class of body HANDOFF §5 safety #6 names by name: `do shell script`/
// `doShellScript` (AppleScript/JXA's own escape hatch into an arbitrary
// shell command), NSTask/`current application's` + "Task" patterns
// (JXA's ObjC-bridge escape hatch into the exact same thing), NSAppleScript
// (nested-interpreter reflection), oversized bodies, and bodies carrying
// bidi/zero-width control code points (the WYSIWYE diff must show a human
// EXACTLY the bytes about to run — an invisible code point hidden inside
// an otherwise-innocent-looking script defeats that regardless of how the
// script is used).
package osascript

import (
	"regexp"
	"strings"
)

// maxScriptBytes is the hard size ceiling (this task's spec step 1,
// verbatim: "reject scripts > 32KB").
const maxScriptBytes = 32 * 1024

// Turkish rejection reasons (CLAUDE.md language policy: user-facing
// strings are Turkish). reasonShellShaped is this task's spec's own
// VERBATIM string — do not reword.
const (
	reasonShellShaped  = "Kabuk komutu içeren script reddedildi — Docker shell aracını kullanın."
	reasonTooLarge     = "Script çok büyük (32KB sınırını aşıyor) — reddedildi."
	reasonControlChars = "Script görünmez veya çift-yönlü kontrol karakterleri içeriyor — reddedildi."
)

// Reason codes (English, log/ledger-facing — CLAUDE.md language policy:
// code/logs/identifiers are English) distinguishing WHICH rule tripped,
// for the osascript_scan_rejected ledger event's own "reason_code" field.
const (
	ReasonCodeTooLarge     = "too_large"
	ReasonCodeControlChars = "control_chars"
	ReasonCodeShellShaped  = "shell_shaped"
)

// RerouteSuggestion is the structured "you could re-request this via
// shell_docker instead" hint this task's spec names: surfaced ONLY when
// the ENTIRE script body is confidently nothing but a single shell
// invocation wrapper (extractPureShellWrapper below) — never a partial or
// guessed extraction from a MIXED script the user never fully saw (this
// task's spec, verbatim: "no silent auto-rerouting of code the user never
// saw"). The caller (mcp/osascript's Runner) surfaces this as ordinary
// (non-error) structured tool output so the worker can actually read and
// act on it — see runner.go's own doc comment for why.
type RerouteSuggestion struct {
	Tool    string `json:"tool"`
	Command string `json:"command"`
}

// ScanResult is Scan's verdict.
type ScanResult struct {
	Rejected bool
	// Reason is the Turkish, human-facing rejection message.
	Reason string
	// ReasonCode is the English, ledger/log-facing rule name.
	ReasonCode string
	// Reroute is non-nil only for a REJECTED, shell-shaped body that is
	// PURELY a `do shell script "<cmd>"` wrapper and nothing else.
	Reroute *RerouteSuggestion
}

// Scan runs every W3-09 static REJECT rule against script, in a fixed
// order (size, then control code points, then shell-shaped patterns —
// this task's spec step 1 lists all three; the order itself carries no
// safety meaning since ANY one of them alone rejects the whole body).
func Scan(script []byte) ScanResult {
	if len(script) > maxScriptBytes {
		return ScanResult{Rejected: true, Reason: reasonTooLarge, ReasonCode: ReasonCodeTooLarge}
	}
	s := string(script)
	if _, bad := firstForbiddenRune(s); bad {
		return ScanResult{Rejected: true, Reason: reasonControlChars, ReasonCode: ReasonCodeControlChars}
	}
	if isShellShaped(s) {
		res := ScanResult{Rejected: true, Reason: reasonShellShaped, ReasonCode: ReasonCodeShellShaped}
		if cmd, ok := extractPureShellWrapper(s); ok {
			res.Reroute = &RerouteSuggestion{Tool: "shell_docker", Command: cmd}
		}
		return res
	}
	return ScanResult{}
}

// whitespaceRun collapses any run of whitespace to a single space so a
// multi-word reject phrase (e.g. "do shell script") is matched WHITESPACE-
// TOLERANTLY (this task's spec, verbatim: "do  shell   script" with extra
// internal whitespace must still be rejected).
var whitespaceRun = regexp.MustCompile(`\s+`)

// flatten lowercases s and collapses whitespace runs — the ONE normalized
// form every case-insensitive, whitespace-tolerant substring check below
// runs against.
func flatten(s string) string {
	return whitespaceRun.ReplaceAllString(strings.ToLower(s), " ")
}

// isShellShaped implements this task's spec step 1's five reject
// patterns, case-insensitive and whitespace-tolerant throughout (via
// flatten): "do shell script", "doShellScript", "NSTask", "NSAppleScript",
// and "current application's" combined with "Task" (JXA's ObjC-bridge
// idiom for spawning a process — this task's spec step 3 calls out
// `ObjC.import('Foundation')` combined with NSTask specifically, which
// the blanket "nstask" substring rule below already catches regardless of
// whether ObjC.import appears alongside it).
func isShellShaped(s string) bool {
	flat := flatten(s)
	switch {
	case strings.Contains(flat, "do shell script"):
		return true
	case strings.Contains(flat, "doshellscript"):
		return true
	case strings.Contains(flat, "nstask"):
		return true
	case strings.Contains(flat, "nsapplescript"):
		return true
	case strings.Contains(flat, "current application") && strings.Contains(flat, "task"):
		return true
	default:
		return false
	}
}

// pureShellWrapperRe matches a script whose ENTIRE (trimmed) body is
// nothing but a single `do shell script "..."` statement — case-
// insensitive, whitespace-tolerant between the three words (mirrors
// isShellShaped's own tolerance), anchored start-to-end so anything else
// before/after the statement (another AppleScript line, a `tell` block
// wrapping it, ...) fails this match and gets NO reroute suggestion at
// all, per this file's own doc comment.
var pureShellWrapperRe = regexp.MustCompile(`(?is)^\s*do\s+shell\s+script\s+"((?:[^"\\]|\\.)*)"\s*$`)

// extractPureShellWrapper reports the shell command a PURE `do shell
// script "<cmd>"` wrapper body carries, unescaping AppleScript's simple
// backslash-escaping (\" and \\) — ok is false for anything else,
// including a shell-shaped body that also contains other code.
func extractPureShellWrapper(s string) (command string, ok bool) {
	m := pureShellWrapperRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return "", false
	}
	return unescapeAppleScriptString(m[1]), true
}

// unescapeAppleScriptString undoes AppleScript string-literal escaping
// (\" and \\ — the only two sequences extractPureShellWrapper's own regex
// can ever have matched into its capture group) so the extracted command
// reads as the actual shell command bytes, not the AppleScript source
// quoting of it.
func unescapeAppleScriptString(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// forbiddenRuneHex is the bidi-control/zero-width rune set this package
// refuses to operate on anywhere in a script body (HANDOFF §5 safety #5
// WYSIWYE). Mirrored BY HAND from kahyad/internal/canon's identical list
// (and mcp/fs/paths.go's own copy) rather than imported: this package
// lives under kahya/mcp, outside the kahya/kahyad/internal/* import
// boundary Go enforces (see mcp/fs/paths.go's own doc comment on the
// identical constraint) — every package on this side of that boundary
// duplicates this small, reviewable-in-a-diff rune set independently
// rather than reaching across it.
var forbiddenRuneHex = []int32{
	0x200B, // ZERO WIDTH SPACE
	0x200C, // ZERO WIDTH NON-JOINER
	0x200D, // ZERO WIDTH JOINER
	0x200E, // LEFT-TO-RIGHT MARK
	0x200F, // RIGHT-TO-LEFT MARK
	0x202A, // LEFT-TO-RIGHT EMBEDDING
	0x202B, // RIGHT-TO-LEFT EMBEDDING
	0x202C, // POP DIRECTIONAL FORMATTING
	0x202D, // LEFT-TO-RIGHT OVERRIDE
	0x202E, // RIGHT-TO-LEFT OVERRIDE
	0x2060, // WORD JOINER
	0x2066, // LEFT-TO-RIGHT ISOLATE
	0x2067, // RIGHT-TO-LEFT ISOLATE
	0x2068, // FIRST STRONG ISOLATE
	0x2069, // POP DIRECTIONAL ISOLATE
	0xFEFF, // ZERO WIDTH NO-BREAK SPACE / BOM
}

var forbiddenRunes = buildForbiddenRunes()

func buildForbiddenRunes() map[rune]bool {
	m := make(map[rune]bool, len(forbiddenRuneHex))
	for _, code := range forbiddenRuneHex {
		m[rune(code)] = true
	}
	return m
}

// firstForbiddenRune returns the first bidi/zero-width control rune found
// in s, if any.
func firstForbiddenRune(s string) (rune, bool) {
	for _, r := range s {
		if forbiddenRunes[r] {
			return r, true
		}
	}
	return 0, false
}
