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
//
// B-9 fix: the five W3-09 literals alone let the obvious equivalents
// through, and HANDOFF §5 safety #6's "shell-shaped" class covers them
// too, so two rule classes now sit on top of them (each only ever ADDS a
// rejection):
//   - shell_shaped (the same verbatim reason): Terminal/iTerm/Script
//     Editor/Automator automation (`do script`, `write text`, `.doScript(`,
//     `application "Terminal"`) and JXA ObjC-bridge process spawning
//     (`$.system`, `$.popen`, `$.exec*`, `$.posix_spawn`, `$.fork`);
//   - code_loader (reasonCodeLoader): bodies that load or generate further
//     code the scanner cannot see — `run script`, `load script`, `use
//     script`, `use framework` (AppleScriptObjC), `ObjC.import`/
//     `ObjC.bindFunction`, JXA `Library(`, `eval(`, the Function
//     constructor, `fromCharCode(`, `atob(`. Owner decision G04-3: the
//     ObjC bridge imports are refused outright, false positives accepted.
//     Since the B-9 review, ANY reference to the bridge counts (a bare
//     `ObjC` identifier, or `$` used as a value - `var o = ObjC;
//     o.import(...)`, `var d = $; d.system(...)` would otherwise alias past
//     the call-site rules).
//   - raw_event_code (reasonRawEventCode): raw Apple event code syntax
//     («event sysoexec», «class ...», or the ASCII << >> digraph). It
//     compiles to any command - `do shell script`, Terminal `do script`,
//     `run script` - under an opaque four-character code the other rules
//     cannot read, so it is refused outright.
//
// The rules run on flatten(s) and, for tokens a string concatenation can
// split ("NS"+"Task", "do shell" & " script", Application("Term"+"inal")),
// on joinSplitTokens(s) / collapse(s). System Events `keystroke`/`key code`
// stay allowed (owner decision G04-4); the W2 approval card is their
// boundary.
package osascript

import (
	"regexp"
	"strings"
	"unicode"
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
	reasonCodeLoader   = "Başka kod yükleyen ya da üreten script reddedildi (run script / load script / use script / ObjC köprüsü / eval) — Docker shell aracını kullanın."
	reasonRawEventCode = "Ham Apple Event kodu («event …» / «class …») içeren script reddedildi — betiği açık AppleScript sözdizimiyle yazın."
)

// Reason codes (English, log/ledger-facing — CLAUDE.md language policy:
// code/logs/identifiers are English) distinguishing WHICH rule tripped,
// for the osascript_scan_rejected ledger event's own "reason_code" field.
const (
	ReasonCodeTooLarge     = "too_large"
	ReasonCodeControlChars = "control_chars"
	ReasonCodeShellShaped  = "shell_shaped"
	ReasonCodeCodeLoader   = "code_loader"
	ReasonCodeRawEventCode = "raw_event_code"
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
// order (size, then control code points, then shell-shaped patterns, then
// raw event codes, then code loaders — ANY one of them alone rejects the
// whole body; the shell-shaped rule runs first only so that a body like
// eval("do shell script …") keeps the shell_shaped code).
func Scan(script []byte) ScanResult {
	if len(script) > maxScriptBytes {
		return ScanResult{Rejected: true, Reason: reasonTooLarge, ReasonCode: ReasonCodeTooLarge}
	}
	s := string(script)
	if _, bad := firstForbiddenRune(s); bad {
		return ScanResult{Rejected: true, Reason: reasonControlChars, ReasonCode: ReasonCodeControlChars}
	}
	forms := normalizedForms(s)
	if isShellShaped(forms) {
		res := ScanResult{Rejected: true, Reason: reasonShellShaped, ReasonCode: ReasonCodeShellShaped}
		if cmd, ok := extractPureShellWrapper(s); ok {
			res.Reroute = &RerouteSuggestion{Tool: "shell_docker", Command: cmd}
		}
		return res
	}
	if isRawEventCode(forms) {
		return ScanResult{Rejected: true, Reason: reasonRawEventCode, ReasonCode: ReasonCodeRawEventCode}
	}
	if isCodeLoader(forms) {
		return ScanResult{Rejected: true, Reason: reasonCodeLoader, ReasonCode: ReasonCodeCodeLoader}
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

// isConcatDelimiter reports the string-literal and concatenation
// delimiters a reject token can be split across: " ' ` + & [ ].
func isConcatDelimiter(r rune) bool {
	switch r {
	case '"', '\'', '`', '+', '&', '[', ']':
		return true
	}
	return false
}

func isWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

// collapse is the fully collapsed form (B-9 fix): lowercase, then every
// whitespace rune AND every concatenation delimiter removed, so a
// multi-word AppleScript command split across string literals
// ("do shell" & " script") rejoins into "doshellscript". It is used for
// that one token only: dropping the space BETWEEN two words also fuses
// ordinary prose ("Returns task" -> "returnstask" contains "nstask").
func collapse(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		if unicode.IsSpace(r) || isConcatDelimiter(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// joinSplitTokens is the concatenation-joined form every other split-token
// rule runs on (B-9 fix): lowercase; concatenation delimiters removed; a
// whitespace run is removed unless it sits between two word runes in the
// ORIGINAL text with no delimiter next to it, in which case it becomes one
// space. So "NS" + "Task", $["NS " + "Task"], $ . system and
// Application("Term"+"inal") join into nstask / $.system /
// application(terminal, while prose keeps its word spacing ("returns
// task"). An identifier cannot contain a word-to-word space, so keeping
// those spaces never un-splits a token a string concatenation produced.
// Parentheses and dots are kept: they mark call sites ($.system, eval().
func joinSplitTokens(s string) string {
	rs := []rune(strings.ToLower(s))
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(rs); {
		r := rs[i]
		if isConcatDelimiter(r) {
			i++
			continue
		}
		if !unicode.IsSpace(r) {
			b.WriteRune(r)
			i++
			continue
		}
		j := i
		for j < len(rs) && unicode.IsSpace(rs[j]) {
			j++
		}
		if i > 0 && j < len(rs) && isWordRune(rs[i-1]) && isWordRune(rs[j]) {
			b.WriteByte(' ')
		}
		i = j
	}
	return b.String()
}

// scanForms carries the three normalized forms every rule matches on.
type scanForms struct {
	flat   string // flatten: lowercase, whitespace runs -> one space
	joined string // joinSplitTokens
	coll   string // collapse
}

func normalizedForms(s string) scanForms {
	return scanForms{flat: flatten(s), joined: joinSplitTokens(s), coll: collapse(s)}
}

// shellShapedFlatRes: Terminal/iTerm/Script Editor/Automator automation and
// JXA ObjC-bridge process spawning, matched on flatten(s) with word
// boundaries (so "undo script" does NOT trip "do script").
var shellShapedFlatRes = []*regexp.Regexp{
	regexp.MustCompile(`\bdo\s+script\b`),  // Terminal.app
	regexp.MustCompile(`\bwrite\s+text\b`), // iTerm2
	regexp.MustCompile(`\.doscript\s*\(`),  // JXA Application("Terminal").doScript(
	// "app" is AppleScript's own abbreviation of "application" (B-9 review).
	regexp.MustCompile(`\b(?:application|app)\s*(?:id\s+)?\(?\s*["']?(?:terminal|iterm2?|script editor|automator|com\.apple\.terminal|com\.googlecode\.iterm2)\b`),
	// Path form: tell application "/System/Applications/Utilities/Terminal.app".
	regexp.MustCompile(`\b(?:terminal|iterm2?|script editor|automator)\.app\b`),
	regexp.MustCompile(`\$\.(?:system|popen|execl|execle|execlp|execv|execve|execvp|posix_spawn|fork)\b`),
}

// shellShapedJoinedTokens: the same class, matched on joinSplitTokens(s)
// so string-concatenation evasions ("NS"+"Task", Application("Term"+
// "inal"), $["sys"+"tem"]) are caught. "doshellscript" is additionally
// matched on collapse(s) (see collapse).
var shellShapedJoinedTokens = []string{
	"nstask", "nsapplescript", "doshellscript",
	"applicationterminal", "application(terminal", "applicationiterm", "application(iterm",
	"application(scripteditor", "application(script editor", "application(automator",
	"appterminal", "appiterm", "appscript editor", "appautomator",
	"terminal.app", "iterm.app", "iterm2.app", "script editor.app", "automator.app",
	"com.apple.terminal", "com.googlecode.iterm2",
	".doscript(", "writetext(", "$.system", "$.popen", "$system(", "$popen(",
}

// codeLoaderFlatRes / codeLoaderJoinedTokens: bodies that LOAD or
// GENERATE further code at run time (nested interpreter, script library,
// ObjC bridge import, eval/obfuscation) — the scanner cannot see what that
// code will do, so the class is refused outright (HANDOFF §5 safety #6;
// owner decision G04-3).
var codeLoaderFlatRes = []*regexp.Regexp{
	regexp.MustCompile(`\brun\s+script\b`),
	regexp.MustCompile(`\bload\s+script\b`),
	regexp.MustCompile(`\buse\s+script\b`), // NOT "use scripting additions": \b after "script" fails on "scripting"
	regexp.MustCompile(`\buse\s+framework\b`),
	regexp.MustCompile(`\bobjc\b`),                    // any use of the JXA ObjC bridge object (G04-3), aliased or not
	regexp.MustCompile(`(?:^|[^\w$])\$(?:[^\w{$]|$)`), // "$" used as a value (the bridge global); "${" (template) and "$5" are not
	regexp.MustCompile(`\blibrary\s*\(`),
	regexp.MustCompile(`\beval\s*\(`),
	regexp.MustCompile(`\bnew\s+function\s*\(`),
	regexp.MustCompile(`\bfunction\s*\(\s*["']`), // Function("return ...")() constructor; an anonymous function (x) has an identifier, not a quote
	regexp.MustCompile(`\bfromcharcode\s*\(`),
	regexp.MustCompile(`\batob\s*\(`),
}

// codeLoaderJoinedTokens: ONLY the JXA tokens a string concatenation can
// split ("Ob"+"jC.import", this["ev"+"al"]). AppleScript keywords
// (run/load/use script, use framework) are NOT here on purpose: they are
// language keywords that cannot be split by string concatenation, and
// their boundary-less forms are false positives — "usescript" is a
// substring of the standard header `use scripting additions`, "loadscript"
// of an identifier like `reloadScriptFlag`, "useframework" of
// `reuseFramework`. Accepted false positive (fail-closed): prose where a
// word ending in "eval"/"library" is followed by "(" ("retrieval (beta)").
var codeLoaderJoinedTokens = []string{
	"objc", "fromcharcode(", "eval(", "library(",
}

// codeLoaderJoinedRes: "$" used as a value in the concatenation-joined
// form (this["$"] joins to this$, so `var d = this["$"]; d.system()`
// becomes "this$;d.system(").
var codeLoaderJoinedRes = []*regexp.Regexp{
	regexp.MustCompile(`\$(?:[.();,=]|$)`),
}

// rawEventCodeRes: raw Apple event code syntax - the chevron characters
// themselves, or the ASCII digraph AppleScript also compiles
// (<<event sysoexec>>), matched on flatten(s).
var rawEventCodeRes = []*regexp.Regexp{
	regexp.MustCompile(`[«»]`),
	regexp.MustCompile(`<<\s*(?:event|class|constant|property|data|script|keyword|type|enum)\b`),
}

// isRawEventCode reports a body written with raw Apple event codes.
func isRawEventCode(f scanForms) bool {
	for _, re := range rawEventCodeRes {
		if re.MatchString(f.flat) {
			return true
		}
	}
	return false
}

// isShellShaped implements this task's spec step 1's five reject
// patterns, case-insensitive and whitespace-tolerant throughout (via
// flatten): "do shell script", "doShellScript", "NSTask", "NSAppleScript",
// and "current application's" combined with "Task" (JXA's ObjC-bridge
// idiom for spawning a process — this task's spec step 3 calls out
// `ObjC.import('Foundation')` combined with NSTask specifically, which
// the blanket "nstask" substring rule below already catches regardless of
// whether ObjC.import appears alongside it) — plus, since B-9, the
// shellShapedFlatRes rules on the flat form and shellShapedJoinedTokens on
// the concatenation-joined/collapsed forms.
func isShellShaped(f scanForms) bool {
	flat := f.flat
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
	case strings.Contains(f.coll, "doshellscript"):
		return true
	}
	for _, re := range shellShapedFlatRes {
		if re.MatchString(flat) {
			return true
		}
	}
	for _, tok := range shellShapedJoinedTokens {
		if strings.Contains(f.joined, tok) {
			return true
		}
	}
	return false
}

// isCodeLoader reports a body that loads or generates further code
// (codeLoaderFlatRes on the flat form, codeLoaderJoinedTokens on the
// concatenation-joined form).
func isCodeLoader(f scanForms) bool {
	for _, re := range codeLoaderFlatRes {
		if re.MatchString(f.flat) {
			return true
		}
	}
	for _, tok := range codeLoaderJoinedTokens {
		if strings.Contains(f.joined, tok) {
			return true
		}
	}
	for _, re := range codeLoaderJoinedRes {
		if re.MatchString(f.joined) {
			return true
		}
	}
	return false
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
