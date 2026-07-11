// redact.go implements W3-07's secret-lane redaction: HANDOFF §5 safety #5
// ⚑ — "Gizli-şerit (finans/sağlık/kimlik) etiketli tek bir bayt içeren
// diff Telegram'a gönderilmez — bu onaylar yalnız yerel yüzeyde gösterilir,
// Telegram'a en fazla redakte başlık gider." If ANY byte of a W1/W2
// payload is secret-lane-labeled (its path matched policy.yaml's
// secret_lane_globs — content-based classification is W3-08, out of
// scope here per this task's own context note: "glob-label alone applies
// for now"), Telegram gets ONLY a redacted title: no diff, no path, no
// content.
package telegram

import (
	"encoding/json"

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
// invariant: file-path globs only, never content-based here). Only
// fs_write/fs_delete carry a structured path field today (the same two
// tools render.go's renderPendingApprovalPayload understands); every
// other tool's tool_input is opaque to this function and is therefore
// NEVER flagged secret-lane by path alone — acceptable ONLY because
// content-based classification is explicitly deferred to W3-08 (this
// task's own context note: "glob-label alone applies for now, fail-closed
// on the stronger side once W3-08 lands").
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
		ok, _ := mcpfs.MatchesAnyGlobCI(cp.Match, secretLaneGlobs)
		return ok
	default:
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
