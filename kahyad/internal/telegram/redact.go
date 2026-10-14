// redact.go implements W3-07's secret-lane redaction: HANDOFF §5 safety #5
// ⚑ — "Gizli-şerit (finans/sağlık/kimlik) etiketli tek bir bayt içeren
// diff Telegram'a gönderilmez — bu onaylar yalnız yerel yüzeyde gösterilir,
// Telegram'a en fazla redakte başlık gider." If ANY byte of a pending
// approval's Telegram text is secret-lane, Telegram gets ONLY a redacted
// title: no diff, no path, no content. Two gates decide it (FIX-P-6):
//
//  1. isSecretLane (first gate): the fs_write/fs_delete path matched
//     policy.yaml's secret_lane_globs, or fs_write's NEW content matched
//     kahyad/internal/secretlane's deterministic pre-pass. It fails CLOSED
//     (FIX-P-6a): a path it cannot canonicalize, or a malformed glob, also
//     redacts.
//  2. outboundSecretLane (second gate): the EXACT text about to be sent —
//     the rendered card, which carries the OLD file content read from disk
//     (render.go's readCurrentFile), every non-fs tool body (script,
//     command, args) and the warning lines, or the W3 notice with its
//     summary — runs through the same deterministic pre-pass.
//
// The local surface (`kahya approve <id>`) always shows the full card.
package telegram

import (
	"encoding/base64"
	"encoding/json"
	"regexp"

	"kahya/kahyad/internal/canon"
	"kahya/kahyad/internal/secretlane"
	mcpfs "kahya/mcp/fs"
)

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
	secret, _ := inputSecretLane(home, secretLaneGlobs, tool, toolInput)
	return secret
}

// inputSecretLane is isSecretLane with the ledger reason: redactReasonGlob,
// redactReasonCanonicalizeError, redactReasonGlobError, or "content:" +
// the classifier's reason for fs_write's new content.
func inputSecretLane(home string, secretLaneGlobs []string, tool string, toolInput []byte) (bool, string) {
	switch tool {
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
// any raw control code point the text still carries. The classifier is
// local and deterministic: it cannot error, and any hit redacts.
func outboundSecretLane(text string) (bool, string) {
	if v := secretlane.ClassifyDeterministic(text); v.SecretLane {
		return true, "content:" + v.Reason
	}
	if folded := visibleEscapeRe.ReplaceAllString(canon.Normalize(text).Canonical, ""); folded != text {
		if v := secretlane.ClassifyDeterministic(folded); v.SecretLane {
			return true, "canonical:" + v.Reason
		}
	}
	return false, ""
}

// redactedNoticeText builds the ONLY text a secret-lane approval may ever
// send to Telegram — no diff, no path, no content: only the raw tool name
// (never a rendered ApprovalPayload.Summary, which for fs_write/fs_delete
// WOULD contain the real path — see render.go).
func redactedNoticeText(tool string) string {
	return "🔒 Yerel onay gerekiyor: " + tool + " (gizli şerit)"
}
