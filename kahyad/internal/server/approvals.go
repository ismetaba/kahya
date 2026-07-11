// approvals.go implements the W3-06 WYSIWYE approval surface's server
// side: GET /policy/approvals (list, `kahya approvals`) and GET
// /policy/approvals?id=<id> (full rendered diff, `kahya approve <id>`'s
// own lookup — read-only, never consumes). Rendering itself is generic,
// dispatched by tool name from the pending_approvals row's own persisted
// tool_input bytes (kahyad/internal/policy.Engine.ListPendingApprovals/
// GetPendingApprovalDetail) — this file is the ONE place that knows how
// to turn a specific tool's wire-shaped tool_input back into a
// kahyad/internal/approval.ApprovalPayload, so kahyad/internal/approval
// itself stays tool-agnostic.
package server

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"time"

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

// osascriptScriptToolInput mirrors mcp/osascript's own (unexported)
// scriptToolInputEnvelope — {script, target_app} — the exact JSON shape
// applescript_run/jxa_run hash via PolicyClient.Check (mcp/osascript/
// runner.go's buildScriptToolInput).
type osascriptScriptToolInput struct {
	Script    string `json:"script"`
	TargetApp string `json:"target_app,omitempty"`
}

// osascriptShortcutToolInput mirrors mcp/osascript's own (unexported)
// shortcutToolInputEnvelope — {name, input_path} and NOTHING else — the
// exact JSON shape shortcuts_run hashes (mcp/osascript/shortcuts.go's
// buildShortcutToolInput).
type osascriptShortcutToolInput struct {
	Name      string `json:"name"`
	InputPath string `json:"input_path,omitempty"`
}

// readCurrentFile returns the CURRENT on-disk bytes at rawPath (resolved
// against home via mcp/fs's own canonicalization — the exact same
// resolution fs_write/fs_delete themselves use), or nil if it does not
// exist / cannot be read. This lets `kahya approve <id>` render a REAL
// before/after diff without kahyad having persisted a separate pre-image
// copy: the approval surface runs on the same machine as kahyad, so
// reading the live file at render time is the same trust boundary the fs
// tool itself already operates in.
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

// renderPendingApproval turns one pending_approvals row's (tool,
// tool_input) into a kahyad/internal/approval.ApprovalPayload for
// rendering — fs_write/fs_delete get a real file_edit diff (read the
// CURRENT file as the "old" side); every other tool (shell_docker,
// osascript, egress/message tools not yet wired, ...) falls back to a
// generic raw-bytes listing, labeled with the real tool name, since their
// own tool_input today is not a structured envelope this file needs to
// parse further (WYSIWYE only needs to show the ACTUAL bytes about to
// execute, which raw bytes already are).
func renderPendingApproval(home, tool string, toolInput []byte) approval.ApprovalPayload {
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
	case "applescript_run", "jxa_run":
		var env osascriptScriptToolInput
		if json.Unmarshal(toolInput, &env) == nil {
			p := approval.BuildOsascript([]byte(env.Script))
			appName := env.TargetApp
			if appName == "" {
				appName = "(bilinmeyen uygulama)"
			}
			p.Summary = tool + ": " + appName
			return p
		}
	case "shortcuts_run":
		var env osascriptShortcutToolInput
		if json.Unmarshal(toolInput, &env) == nil {
			return approval.BuildShortcut(env.Name, env.InputPath)
		}
	}
	p := approval.BuildOsascript(toolInput)
	p.Summary = tool
	return p
}

// policyApprovalsListRow is one GET /policy/approvals row (this task's
// spec, verbatim field list: "id, tool, class, summary, age").
type policyApprovalsListRow struct {
	ID       string `json:"id"`
	Tool     string `json:"tool"`
	Class    string `json:"class"`
	Scope    string `json:"scope"`
	Summary  string `json:"summary"`
	AgeS     int64  `json:"age_s"`
	MintedAt string `json:"minted_at"`
}

type policyApprovalsListResponse struct {
	Approvals []policyApprovalsListRow `json:"approvals"`
}

// policyApprovalDetailResponse is GET /policy/approvals?id=<id>'s body:
// the full rendered WYSIWYE text `kahya approve <id>` prints verbatim
// before prompting.
type policyApprovalDetailResponse struct {
	ID       string `json:"id"`
	Tool     string `json:"tool"`
	Class    string `json:"class"`
	Scope    string `json:"scope"`
	Rendered string `json:"rendered"`
	Error    string `json:"error,omitempty"`
}

// handlePolicyApprovals implements GET /policy/approvals (list) and GET
// /policy/approvals?id=<id> (single-row detail, read-only).
func (s *Server) handlePolicyApprovals(w http.ResponseWriter, r *http.Request) {
	if s.policyEngine == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "policy engine not available")
		return
	}
	if id := r.URL.Query().Get("id"); id != "" {
		s.handlePolicyApprovalDetail(w, r, id)
		return
	}

	rows, err := s.policyEngine.ListPendingApprovals(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	now := time.Now().UTC()
	resp := policyApprovalsListResponse{Approvals: make([]policyApprovalsListRow, len(rows))}
	for i, row := range rows {
		home := s.approvalsHomeDir()
		payload := renderPendingApproval(home, row.Tool, row.ToolInput)
		resp.Approvals[i] = policyApprovalsListRow{
			ID: row.ID, Tool: row.Tool, Class: string(row.Class), Scope: row.Scope,
			Summary: payload.Summary, AgeS: int64(now.Sub(row.MintedAt).Round(time.Second).Seconds()),
			MintedAt: row.MintedAt.Format(time.RFC3339),
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handlePolicyApprovalDetail implements GET /policy/approvals?id=<id>.
func (s *Server) handlePolicyApprovalDetail(w http.ResponseWriter, r *http.Request, id string) {
	detail, err := s.policyEngine.GetPendingApprovalDetail(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, policyApprovalDetailResponse{Error: err.Error()})
		return
	}
	payload := renderPendingApproval(s.approvalsHomeDir(), detail.Tool, detail.ToolInput)
	writeJSON(w, http.StatusOK, policyApprovalDetailResponse{
		ID: detail.ID, Tool: detail.Tool, Class: string(detail.Class), Scope: detail.Scope,
		Rendered: payload.Render(),
	})
}

// approvalsHomeDir resolves the home directory renderPendingApproval
// canonicalizes fs paths against — s.fsServer.Home when the fs tool is
// wired (production always wires it), falling back to os.UserHomeDir()
// otherwise so rendering degrades to "no pre-image available" rather than
// panicking on a nil fsServer (e.g. a deny-all-mode boot).
func (s *Server) approvalsHomeDir() string {
	if s.fsServer != nil {
		return s.fsServer.Home
	}
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	return ""
}
