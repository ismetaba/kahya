// policy.go mounts the four W3-02 autonomy-ladder endpoints that sit
// alongside the (upgraded, in task.go) POST /policy/check:
// POST /policy/consume-token, POST /policy/feedback, GET /policy/state,
// and POST /policy/promote (the CLI's `kahya autonomy promote` path).
// POST /policy/undo (`kahya undo --trace <id>`) is here too, kept
// separate from /policy/feedback's approve/deny handling since it
// addresses an open undo_windows row by trace_id rather than a
// pending_approval_id.
//
// See kahyad/internal/policy/README.md for the wire schema this file and
// task.go's handlePolicyCheck together implement.
package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"kahya/kahyad/internal/policy"
)

// ---- POST /policy/consume-token ----

// policyConsumeTokenRequest is POST /policy/consume-token's request body:
// everything a side-effectful MCP tool (W3-03 fs, W3-04 docker-shell, ...)
// already knows from the /policy/check decision it just received (tool,
// class, scope, task_id, trace_id, token) plus tool_input - the EXACT
// bytes it is about to execute with, hashed here and compared against
// what was approved at mint time (HANDOFF §5 safety #5 WYSIWYE).
type policyConsumeTokenRequest struct {
	TraceID   string          `json:"trace_id"`
	TaskID    string          `json:"task_id"`
	Tool      string          `json:"tool"`
	Class     string          `json:"class"`
	Scope     string          `json:"scope"`
	Token     string          `json:"token"`
	ToolInput json.RawMessage `json:"tool_input"`
}

type policyConsumeTokenResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// handlePolicyConsumeToken implements POST /policy/consume-token. Any
// failure (malformed body, engine unavailable, or ConsumeToken itself
// failing) answers ok:false, HTTP 200 - the caller only needs to know
// whether it may proceed, not an HTTP-status-coded taxonomy of why not;
// the WHY is in the ledger (token_verify_failed) for operators.
func (s *Server) handlePolicyConsumeToken(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, policyCheckMaxBody)
	var req policyConsumeTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusOK, policyConsumeTokenResponse{OK: false, Error: "invalid request body"})
		return
	}
	traceID := req.TraceID
	if traceID == "" {
		traceID = traceIDFromContext(r)
	}
	if s.denyAll || s.policyEngine == nil {
		writeJSON(w, http.StatusOK, policyConsumeTokenResponse{OK: false, Error: "policy engine not available"})
		return
	}

	err := s.policyEngine.ConsumeToken(r.Context(), policy.ConsumeInput{
		Token: req.Token, Tool: req.Tool, Class: policy.ActionClass(req.Class), Scope: req.Scope,
		TaskID: req.TaskID, TraceID: traceID, ToolInput: req.ToolInput,
	})
	if err != nil {
		writeJSON(w, http.StatusOK, policyConsumeTokenResponse{OK: false, Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, policyConsumeTokenResponse{OK: true})
}

// ---- POST /policy/feedback ----

// policyFeedbackRequest is POST /policy/feedback's request body. Kind
// selects the outcome: "approve"/"deny" address a pending_approval_id (a
// prior NEEDS_APPROVAL decision's opaque reference); "undo" addresses an
// open undo_windows row by trace_id instead (kahya undo --trace <id> -
// see handlePolicyUndo, which is the dedicated route for that; this kind
// is accepted here too so /policy/feedback's documented "approve/deny/
// undo outcomes drive promotion/demotion" contract holds at this one
// endpoint as well).
type policyFeedbackRequest struct {
	Kind              string `json:"kind"` // "approve" | "deny" | "undo"
	PendingApprovalID string `json:"pending_approval_id,omitempty"`
	// Surface must be "local" for a W3-class pending approval (HANDOFF §5
	// safety #5: Telegram may notify, never approve, a W3 action) -
	// enforced in kahyad/internal/policy.Engine.Approve, not here.
	Surface string `json:"surface,omitempty"`
	TraceID string `json:"trace_id,omitempty"` // required for kind="undo"
}

type policyFeedbackResponse struct {
	OK    bool   `json:"ok"`
	Token string `json:"token,omitempty"`
	Error string `json:"error,omitempty"`
}

// handlePolicyFeedback implements POST /policy/feedback's approve/deny/
// undo outcomes (HANDOFF S4 promotion/demotion rules).
func (s *Server) handlePolicyFeedback(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, policyCheckMaxBody)
	var req policyFeedbackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, policyFeedbackResponse{OK: false, Error: "invalid request body"})
		return
	}
	if s.denyAll || s.policyEngine == nil {
		writeJSON(w, http.StatusServiceUnavailable, policyFeedbackResponse{OK: false, Error: "policy engine not available"})
		return
	}

	switch req.Kind {
	case "approve":
		result, err := s.policyEngine.Approve(r.Context(), req.PendingApprovalID, req.Surface)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, policyFeedbackResponse{OK: false, Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, policyFeedbackResponse{OK: true, Token: result.Token})
	case "deny":
		if err := s.policyEngine.Deny(r.Context(), req.PendingApprovalID); err != nil {
			writeJSON(w, http.StatusBadRequest, policyFeedbackResponse{OK: false, Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, policyFeedbackResponse{OK: true})
	case "undo":
		traceID := req.TraceID
		if traceID == "" {
			writeJSON(w, http.StatusBadRequest, policyFeedbackResponse{OK: false, Error: "trace_id required for kind=undo"})
			return
		}
		row, err := s.policyEngine.TriggerUndo(r.Context(), traceID)
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, policy.ErrNoOpenUndoWindow) {
				status = http.StatusNotFound
			}
			writeJSON(w, status, policyFeedbackResponse{OK: false, Error: err.Error()})
			return
		}
		// W3-03: execute the owning tool's undo recipe (Trash restore /
		// git-checkpoint restore) now that the window is durably
		// "triggered" and the ladder state already demoted - see
		// fs.go's dispatchFSUndo doc comment for why this is a SEPARATE,
		// best-effort step from TriggerUndo's own bookkeeping.
		s.dispatchFSUndo(r.Context(), traceID, row.Tool)
		writeJSON(w, http.StatusOK, policyFeedbackResponse{OK: true})
	default:
		writeJSON(w, http.StatusBadRequest, policyFeedbackResponse{OK: false, Error: "kind must be approve, deny, or undo"})
	}
}

// ---- GET /policy/state ----

// policyStateRow is one GET /policy/state row (kahya autonomy's ladder
// dump).
type policyStateRow struct {
	Tool                 string `json:"tool"`
	Class                string `json:"class"`
	Scope                string `json:"scope"`
	Level                int64  `json:"level"`
	ConsecutiveApprovals int64  `json:"consecutive_approvals"`
	UpdatedAt            string `json:"updated_at"`
}

type policyStateResponse struct {
	States []policyStateRow `json:"states"`
}

// handlePolicyState implements GET /policy/state: the full autonomy_state
// table dump `kahya autonomy` renders.
func (s *Server) handlePolicyState(w http.ResponseWriter, r *http.Request) {
	if s.policyEngine == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "policy engine not available")
		return
	}
	rows, err := s.policyEngine.ListState(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp := policyStateResponse{States: make([]policyStateRow, len(rows))}
	for i, row := range rows {
		resp.States[i] = policyStateRow{
			Tool: row.Tool, Class: row.Class, Scope: row.Scope,
			Level: row.Level, ConsecutiveApprovals: row.ConsecutiveApprovals, UpdatedAt: row.UpdatedAt,
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---- POST /policy/promote ----

// policyPromoteRequest is POST /policy/promote's request body - the
// ONLY promotion path (HANDOFF S4), invoked exclusively by
// `kahya autonomy promote <tool> <class> <scope>`.
type policyPromoteRequest struct {
	TraceID string `json:"trace_id"`
	Tool    string `json:"tool"`
	Class   string `json:"class"`
	Scope   string `json:"scope"`
}

type policyPromoteResponse struct {
	Level int    `json:"level"`
	Error string `json:"error,omitempty"`
}

// handlePolicyPromote implements POST /policy/promote.
func (s *Server) handlePolicyPromote(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, policyCheckMaxBody)
	var req policyPromoteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, policyPromoteResponse{Error: "invalid request body"})
		return
	}
	if s.policyEngine == nil {
		writeJSON(w, http.StatusServiceUnavailable, policyPromoteResponse{Error: "policy engine not available"})
		return
	}
	traceID := req.TraceID
	if traceID == "" {
		traceID = traceIDFromContext(r)
	}
	level, err := s.policyEngine.Promote(r.Context(), traceID, req.Tool, policy.ActionClass(req.Class), req.Scope)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, policyPromoteResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, policyPromoteResponse{Level: level})
}

// ---- POST /policy/undo ----

// policyUndoRequest is POST /policy/undo's request body - `kahya undo
// --trace <id>`'s server-side call.
type policyUndoRequest struct {
	TraceID string `json:"trace_id"`
}

type policyUndoResponse struct {
	OK     bool   `json:"ok"`
	Tool   string `json:"tool,omitempty"`
	TaskID string `json:"task_id,omitempty"`
	Error  string `json:"error,omitempty"`
}

// handlePolicyUndo implements POST /policy/undo (`kahya undo --trace
// <id>`'s real server-side call, kahyad/cmd/kahya/client.go's
// PolicyUndo): triggers the undo window for trace_id while it is still
// open (HANDOFF S4 ladder L2 row), then dispatches to the owning tool's
// recipe (W3-03: mcp/fs.Server.UndoWrite/UndoDelete) - see
// dispatchFSUndo's doc comment (fs.go) for why that is a separate,
// best-effort step from TriggerUndo's own window/demotion bookkeeping.
func (s *Server) handlePolicyUndo(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, policyCheckMaxBody)
	var req policyUndoRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, policyUndoResponse{OK: false, Error: "invalid request body"})
		return
	}
	if s.policyEngine == nil {
		writeJSON(w, http.StatusServiceUnavailable, policyUndoResponse{OK: false, Error: "policy engine not available"})
		return
	}
	if req.TraceID == "" {
		writeJSON(w, http.StatusBadRequest, policyUndoResponse{OK: false, Error: "trace_id required"})
		return
	}
	row, err := s.policyEngine.TriggerUndo(r.Context(), req.TraceID)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, policy.ErrNoOpenUndoWindow) {
			status = http.StatusNotFound
		}
		writeJSON(w, status, policyUndoResponse{OK: false, Error: err.Error()})
		return
	}
	s.dispatchFSUndo(r.Context(), req.TraceID, row.Tool)
	writeJSON(w, http.StatusOK, policyUndoResponse{OK: true, Tool: row.Tool, TaskID: row.TaskID})
}
