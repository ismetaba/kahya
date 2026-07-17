// redact.go implements W3-07's secret-lane redaction: HANDOFF §5 safety #5
// ⚑ — "Gizli-şerit (finans/sağlık/kimlik) etiketli tek bir bayt içeren
// diff Telegram'a gönderilmez — bu onaylar yalnız yerel yüzeyde gösterilir,
// Telegram'a en fazla redakte başlık gider." If ANY byte of a W1/W2
// payload is secret-lane-labeled — its path matched policy.yaml's
// secret_lane_globs, OR (W3-10 gate-test fix: this function's own
// original doc comment flagged this as a TODO — "content-based
// classification is W3-08, out of scope here ... glob-label alone
// applies for now, fail-closed on the stronger side once W3-08 lands" —
// W3-08 has now landed, so the stronger side applies here too) its file
// CONTENT matches kahyad/internal/secretlane's deterministic pre-pass
// (IBAN/TCKN/card-number/keyword) — Telegram gets ONLY a redacted title:
// no diff, no path, no content.
package telegram

import (
	"encoding/base64"
	"encoding/json"

	"kahya/kahyad/internal/secretlane"
	mcpfs "kahya/mcp/fs"
)

// fsPathToolInput reads just the {"path": "..."} field common to
// fs_write/fs_delete's tool_input envelope (render.go's fsWriteToolInput
// carries content too, which this check never needs to touch).
type fsPathToolInput struct {
	Path string `json:"path"`
}

// isSecretLane reports whether tool/toolInput touches a path matched by
// secretLaneGlobs (policy.yaml's secret_lane_globs — HANDOFF §4 ordering
// invariant: file-path globs only for THIS check) OR — the W3-10 gate-test
// fix — whose file CONTENT (fs_write only; fs_delete carries none) matches
// kahyad/internal/secretlane's deterministic pre-pass. Only fs_write/
// fs_delete carry a structured path field today (the same two tools
// render.go's renderPendingApprovalPayload understands), so their content
// check runs against the decoded {"content_base64"} body specifically.
//
// review-fix #5: for EVERY other tool (policy.yaml classifies shell_docker,
// shell_host, applescript_run, jxa_run, shortcuts_run, telegram_send as W2),
// renderPendingApprovalPayload falls through to BuildOsascript, which dumps
// the ENTIRE raw tool_input JSON verbatim into the inline-keyboard card. So
// a secret-lane LOCAL worker that emits e.g. an applescript_run whose script
// embeds a TCKN/IBAN/card/health value would otherwise ship those bytes
// straight to api.telegram.org. The default branch therefore runs the SAME
// deterministic pre-pass over the raw tool_input JSON string: because the
// card dumps the whole envelope, scanning string(toolInput) covers
// script/command/body fields regardless of field name, and any newly-added
// content-bearing tool is covered automatically. HANDOFF §5 safety #9
// (secret-lane content is NEVER sent to Telegram) draws no per-tool
// exception.
func isSecretLane(home string, secretLaneGlobs []string, tool string, toolInput []byte) bool {
	switch tool {
	case "fs_write", "fs_delete":
		var env fsPathToolInput
		if err := json.Unmarshal(toolInput, &env); err != nil || env.Path == "" {
			return false
		}
		cp, err := mcpfs.Canonicalize(home, env.Path)
		if err != nil {
			return false
		}
		if ok, _ := mcpfs.MatchesAnyGlobCI(cp.Match, secretLaneGlobs); ok {
			return true
		}
		if tool == "fs_write" {
			var writeEnv fsWriteToolInput
			if json.Unmarshal(toolInput, &writeEnv) == nil && writeEnv.ContentBase64 != "" {
				if content, derr := base64.StdEncoding.DecodeString(writeEnv.ContentBase64); derr == nil {
					if secretlane.ClassifyDeterministic(string(content)).SecretLane {
						return true
					}
				}
			}
		}
		return false
	default:
		if secretlane.ClassifyDeterministic(string(toolInput)).SecretLane {
			return true
		}
		return false
	}
}

// redactedNoticeText builds the ONLY text a secret-lane-labeled W1/W2
// approval may ever send to Telegram — no diff, no path, no content: only
// the raw tool name (never a rendered ApprovalPayload.Summary, which for
// fs_write/fs_delete WOULD contain the real path — see render.go).
func redactedNoticeText(tool string) string {
	return "🔒 Yerel onay gerekiyor: " + tool + " (gizli şerit)"
}
