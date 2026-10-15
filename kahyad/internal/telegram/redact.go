// redact.go implements W3-07's secret-lane redaction: HANDOFF §5 safety #5
// ⚑ — "Gizli-şerit (finans/sağlık/kimlik) etiketli tek bir bayt içeren
// diff Telegram'a gönderilmez — bu onaylar yalnız yerel yüzeyde gösterilir,
// Telegram'a en fazla redakte başlık gider." If ANY byte of a pending
// approval's Telegram text is secret-lane, Telegram gets ONLY a redacted
// title: no diff, no path, no content. Two gates decide it (FIX-P-6):
//
//  1. isSecretLane (first gate): the fs_write/fs_delete path matched
//     policy.yaml's secret_lane_globs, or fs_write's NEW content matched
//     kahyad/internal/secretlane's deterministic pre-pass, or (FIX-FINAL
//     telegram-memory-write-gizli-card-unredacted) the memory_write /
//     memory_forget `file` - relative to the memory dir - matched a
//     secret-lane glob (memlane.MatchesSecretGlob, the rule memlane and
//     consolidation apply to the same path). It fails CLOSED (FIX-P-6a): a
//     path it cannot canonicalize, a malformed glob, or a memory-tool file
//     with no memory dir wired, also redacts.
//  2. outboundSecretLane (second gate): the EXACT text about to be sent —
//     the rendered card, which carries the OLD file content read from disk
//     (render.go's readCurrentFile), every non-fs tool body (script,
//     command, args) and the warning lines, or the W3 notice with its
//     summary — runs through the same deterministic pre-pass, both as sent
//     and with its backslash escapes decoded (a non-fs body is listed as
//     its raw JSON tool_input, where a newline before an IBAN is the two
//     bytes `\n`).
//
// The local surface (`kahya approve <id>`) always shows the full card.
//
// FIX-U-5: every outbound body, not only approval payloads, is classified
// by outboundSecretLane (bot.go send), and a session that has done a
// secret-lane read sends nothing but constant notices.
package telegram

import (
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"kahya/kahyad/internal/canon"
	"kahya/kahyad/internal/memlane"
	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/secretlane"
	mcpfs "kahya/mcp/fs"
)

// memoryFileToolInput reads the {"file": "..."} field memory_write and
// memory_forget carry - a path RELATIVE to the memory dir.
type memoryFileToolInput struct {
	File string `json:"file"`
}

// policyGlobMatcher adapts policy.MatchGlob to memlane.GlobMatcher.
type policyGlobMatcher struct{}

func (policyGlobMatcher) Match(pattern, path string) (bool, error) {
	return policy.MatchGlob(pattern, path)
}

// fsPathToolInput reads just the {"path": "..."} field common to
// fs_write/fs_delete's tool_input envelope (render.go's fsWriteToolInput
// carries content too, which this check never needs to touch).
type fsPathToolInput struct {
	Path string `json:"path"`
}

// Reasons inputSecretLane reports (ledgered locally in
// telegram_card_redacted, never sent to Telegram).
const (
	redactReasonGlob              = "glob"
	redactReasonCanonicalizeError = "canonicalize_error"
	redactReasonGlobError         = "glob_error"
	// redactReasonMemoryDirUnwired: a memory-tool card names a file but no
	// memory dir is wired to resolve it against (fail-closed).
	redactReasonMemoryDirUnwired = "memory_dir_unwired"
)

// isSecretLane is the first redaction gate: it reports whether tool/
// toolInput touches a path matched by secretLaneGlobs (policy.yaml's
// secret_lane_globs — HANDOFF §4 ordering invariant: file-path globs only
// for THIS check) OR — the W3-10 gate-test fix — whose NEW file content
// (fs_write only; fs_delete carries none) matches kahyad/internal/
// secretlane's deterministic pre-pass. Only fs_write/fs_delete carry a
// structured path field (the same two tools render.go's
// renderPendingApprovalPayload understands); every other tool's body, and
// the OLD content of an fs_write/fs_delete target, are classified by the
// second gate, outboundSecretLane, over the exact text about to be sent.
//
// Canonicalize/glob errors redact (FIX-P-6a, CLAUDE.md fail-closed): a
// path that cannot be canonicalized is not a path that can be cleared, and
// a malformed glob means the redaction decision is unknown. The local
// surface still shows the full card.
func isSecretLane(home string, secretLaneGlobs []string, tool string, toolInput []byte) bool {
	secret, _ := inputSecretLane(home, "", secretLaneGlobs, tool, toolInput)
	return secret
}

// inputSecretLane is isSecretLane with the ledger reason: redactReasonGlob,
// redactReasonCanonicalizeError, redactReasonGlobError,
// redactReasonMemoryDirUnwired, or "content:" + the classifier's reason
// for fs_write's new content. memoryDir is cfg.MemoryDir, the root
// memory_write's / memory_forget's `file` is relative to.
func inputSecretLane(home, memoryDir string, secretLaneGlobs []string, tool string, toolInput []byte) (bool, string) {
	switch tool {
	case "memory_write", "memory_forget":
		var env memoryFileToolInput
		if json.Unmarshal(toolInput, &env) != nil {
			// No path to label; the rendered card is still classified by
			// outboundSecretLane (the fs rule for an unparseable input).
			return false, ""
		}
		rel := strings.TrimSpace(env.File)
		if rel == "" {
			return false, "" // memory_write's default target: inbox/<date>.md
		}
		if memoryDir == "" {
			return true, redactReasonMemoryDirUnwired
		}
		abs := filepath.Join(memoryDir, filepath.FromSlash(rel))
		for _, g := range secretLaneGlobs {
			if memlane.MatchesSecretGlob(policyGlobMatcher{}, g, abs) {
				return true, redactReasonGlob
			}
		}
		return false, ""
	case "fs_write", "fs_delete":
		var env fsPathToolInput
		if err := json.Unmarshal(toolInput, &env); err != nil || env.Path == "" {
			// No path to label: the rendered card (a raw listing of these
			// bytes) is still classified by outboundSecretLane.
			return false, ""
		}
		cp, err := mcpfs.Canonicalize(home, env.Path)
		if err != nil {
			return true, redactReasonCanonicalizeError
		}
		ok, gerr := mcpfs.MatchesAnyGlobCI(cp.Match, secretLaneGlobs)
		if gerr != nil {
			return true, redactReasonGlobError
		}
		if ok {
			return true, redactReasonGlob
		}
		if tool == "fs_write" {
			var writeEnv fsWriteToolInput
			if json.Unmarshal(toolInput, &writeEnv) == nil && writeEnv.ContentBase64 != "" {
				if content, derr := base64.StdEncoding.DecodeString(writeEnv.ContentBase64); derr == nil {
					if v := secretlane.ClassifyDeterministic(string(content)); v.SecretLane {
						return true, "content:" + v.Reason
					}
				}
			}
		}
		return false, ""
	default:
		return false, ""
	}
}

// visibleEscapeRe matches the "<U+XXXX>" escape canon.Normalize's Display
// form (and approval's warning lines) substitute for a stripped bidi/
// zero-width/format code point.
var visibleEscapeRe = regexp.MustCompile(`<U\+[0-9A-F]{4,6}>`)

// outboundSecretLane is the second redaction gate (FIX-P-6): it reports
// whether ANY byte of text — exactly the bytes about to be sent to
// Telegram — is secret-lane. The deterministic pre-pass runs over the text
// itself, then over its canonical form with every visible "<U+XXXX>"
// escape removed: the rendered card shows a zero-width or bidi code point
// hidden inside an IBAN as a visible escape ("TR33<U+200B>0006..."), which
// the IBAN/TCKN/card regexes would not match, while a human reading the
// card still sees the number. canon.Normalize's Canonical form also strips
// any raw control code point the text still carries.
//
// Then the same two passes run over the text with its backslash escapes
// decoded, one nesting level per round (foldBackslashEscapes, at most
// maxEscapeFoldRounds rounds): every non-fs card is a raw listing of its
// JSON tool_input (render.go -> approval.BuildOsascript), so a newline or
// tab inside a script, note or argument is the two bytes `\n` / `\t`, and
// `\nTR33 ...` has no word boundary before "TR33" for the classifier's
// regexes - yet the reader sees the number. The same holds for literal
// escapes inside file content (a JSON file in a diff) and for a JSON
// document nested in a JSON tool_input (`\\n`, the second round). A hit
// in any form redacts; the decoded forms only ever add hits.
//
// The classifier is local and deterministic: it cannot error, and any hit
// redacts.
func outboundSecretLane(text string) (bool, string) {
	if secret, reason := classifyOutboundForm(text, "content:", "canonical:"); secret {
		return true, reason
	}
	decoded := text
	for round := 0; round < maxEscapeFoldRounds; round++ {
		next := foldBackslashEscapes(decoded)
		if next == decoded {
			break
		}
		decoded = next
		if secret, reason := classifyOutboundForm(decoded, "escape:", "escape_canonical:"); secret {
			return true, reason
		}
	}
	return false, ""
}

// classifyOutboundForm runs the deterministic pre-pass over text and over
// its escape-stripped canonical form (see outboundSecretLane), prefixing
// the classifier's reason with plainPrefix / canonicalPrefix.
func classifyOutboundForm(text, plainPrefix, canonicalPrefix string) (bool, string) {
	if v := secretlane.ClassifyDeterministic(text); v.SecretLane {
		return true, plainPrefix + v.Reason
	}
	if folded := visibleEscapeRe.ReplaceAllString(canon.Normalize(text).Canonical, ""); folded != text {
		if v := secretlane.ClassifyDeterministic(folded); v.SecretLane {
			return true, canonicalPrefix + v.Reason
		}
	}
	return false, ""
}

// maxEscapeFoldRounds bounds how many nested escape levels
// outboundSecretLane decodes: a JSON tool_input is one level, a JSON
// document carried inside it two; more rounds stop early once a round
// changes nothing.
const maxEscapeFoldRounds = 4

// foldBackslashEscapes decodes ONE level of backslash escapes in s: the
// JSON escapes (\n \r \t \b \f \" \\ \/ and \uXXXX, a surrogate pair
// joined into one rune) plus the source-language forms a script body can
// spell a character with (\' , \xHH for ASCII, \u{H...} as in
// JavaScript/JXA). Any other backslash, and a malformed escape, is kept
// verbatim, so a round that finds nothing to decode returns s unchanged.
func foldBackslashEscapes(s string) string {
	i := strings.IndexByte(s, '\\')
	if i < 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	b.WriteString(s[:i])
	for i < len(s) {
		c := s[i]
		if c != '\\' || i+1 == len(s) {
			b.WriteByte(c)
			i++
			continue
		}
		switch e := s[i+1]; e {
		case 'n':
			b.WriteByte('\n')
			i += 2
		case 'r':
			b.WriteByte('\r')
			i += 2
		case 't':
			b.WriteByte('\t')
			i += 2
		case 'b':
			b.WriteByte('\b')
			i += 2
		case 'f':
			b.WriteByte('\f')
			i += 2
		case '"', '\\', '/', '\'':
			b.WriteByte(e)
			i += 2
		case 'u':
			r, n := decodeUnicodeEscape(s[i:])
			if n == 0 {
				b.WriteByte(c)
				i++
				continue
			}
			b.WriteRune(r)
			i += n
		case 'x':
			if i+4 <= len(s) {
				if v, ok := hexValue(s[i+2 : i+4]); ok && v < utf8.RuneSelf {
					b.WriteByte(byte(v))
					i += 4
					continue
				}
			}
			b.WriteByte(c)
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// decodeUnicodeEscape decodes the \u escape at the start of s ("\uXXXX",
// a "\uD8XX\uDCXX" surrogate pair, or "\u{H...}") and reports how many
// bytes it consumed; n == 0 means s does not start with a well-formed one.
// A lone surrogate decodes to U+FFFD, as encoding/json does.
func decodeUnicodeEscape(s string) (r rune, n int) {
	if len(s) >= 4 && s[2] == '{' {
		end := strings.IndexByte(s[3:], '}')
		if end < 1 || end > 6 {
			return 0, 0
		}
		v, ok := hexValue(s[3 : 3+end])
		if !ok || v > utf8.MaxRune {
			return 0, 0
		}
		return rune(v), 3 + end + 1
	}
	if len(s) < 6 {
		return 0, 0
	}
	v, ok := hexValue(s[2:6])
	if !ok {
		return 0, 0
	}
	r = rune(v)
	if !utf16.IsSurrogate(r) {
		return r, 6
	}
	if len(s) >= 12 && s[6] == '\\' && s[7] == 'u' {
		if lo, ok := hexValue(s[8:12]); ok {
			if pair := utf16.DecodeRune(r, rune(lo)); pair != utf8.RuneError {
				return pair, 12
			}
		}
	}
	return utf8.RuneError, 6
}

// hexValue parses a non-empty run of ASCII hex digits.
func hexValue(h string) (uint32, bool) {
	if h == "" {
		return 0, false
	}
	var v uint32
	for i := 0; i < len(h); i++ {
		c := h[i]
		var d byte
		switch {
		case c >= '0' && c <= '9':
			d = c - '0'
		case c >= 'a' && c <= 'f':
			d = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			d = c - 'A' + 10
		default:
			return 0, false
		}
		v = v<<4 | uint32(d)
	}
	return v, true
}

// redactedNoticeText builds the ONLY text a secret-lane approval may ever
// send to Telegram — no diff, no path, no content: only the raw tool name
// (never a rendered ApprovalPayload.Summary, which for fs_write/fs_delete
// WOULD contain the real path — see render.go).
func redactedNoticeText(tool string) string {
	return "🔒 Yerel onay gerekiyor: " + tool + " (gizli şerit)"
}
