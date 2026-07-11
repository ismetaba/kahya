// render.go turns one pending_approvals row's (tool, tool_input) into a
// kahyad/internal/approval.ApprovalPayload for the Telegram surface — the
// SAME dispatch table kahyad/internal/server/approvals.go's
// renderPendingApproval implements for the CLI surface (fs_write/fs_delete
// get a real file_edit diff against the CURRENT on-disk file; every other
// tool falls back to a generic raw-bytes listing labeled with the real
// tool name). This is intentionally duplicated rather than imported from
// kahyad/internal/server: that package's own doc comment explains why the
// tool-name dispatch table is kept private to each approval SURFACE
// (keeping kahyad/internal/approval itself tool-agnostic) rather than
// shared — and kahyad/internal/server must never import this package (or
// vice versa) since main.go wires both independently.
package telegram

import (
	"encoding/base64"
	"encoding/json"
	"os"

	"kahya/kahyad/internal/approval"
	mcpfs "kahya/mcp/fs"
)

// fsWriteToolInput mirrors mcp/fs's own (unexported) toolInputEnvelope —
// {path, content_base64} — the exact JSON shape fs_write/fs_delete hash
// via PolicyClient.Check (mcp/fs/server.go's buildToolInput). ContentBase64
// is absent (zero value) for fs_delete, which carries no content at all.
type fsWriteToolInput struct {
	Path          string `json:"path"`
	ContentBase64 string `json:"content_base64,omitempty"`
}

// readCurrentFile returns the CURRENT on-disk bytes at rawPath (resolved
// against home via mcp/fs's own canonicalization), or nil if it does not
// exist / cannot be read — lets the Telegram card render a REAL
// before/after diff without kahyad having persisted a separate pre-image
// copy, exactly like the CLI surface's own renderPendingApproval.
func readCurrentFile(home, rawPath string) []byte {
	cp, err := mcpfs.Canonicalize(home, rawPath)
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(cp.Op)
	if err != nil {
		return nil
	}
	return data
}

// renderPendingApprovalPayload dispatches tool/toolInput into a rendered
// approval.ApprovalPayload — see this file's own doc comment for the
// dispatch rule (fs_write/fs_delete get a structured file_edit diff;
// everything else is a generic, tool-name-labeled raw listing).
func renderPendingApprovalPayload(home, tool string, toolInput []byte) approval.ApprovalPayload {
	switch tool {
	case "fs_write":
		var env fsWriteToolInput
		if json.Unmarshal(toolInput, &env) == nil && env.Path != "" {
			newContent, err := base64.StdEncoding.DecodeString(env.ContentBase64)
			if err != nil {
				newContent = nil
			}
			oldContent := readCurrentFile(home, env.Path)
			p := approval.BuildFileEdit(env.Path, oldContent, newContent)
			p.Summary = "fs_write: " + p.Path
			return p
		}
	case "fs_delete":
		var env fsWriteToolInput
		if json.Unmarshal(toolInput, &env) == nil && env.Path != "" {
			oldContent := readCurrentFile(home, env.Path)
			p := approval.BuildFileEdit(env.Path, oldContent, nil)
			p.Summary = "fs_delete: " + p.Path
			return p
		}
	}
	p := approval.BuildOsascript(toolInput)
	p.Summary = tool
	return p
}
