// approvals_decision.go implements W6-01's Hammerspoon-facing approval
// routes on top of the SAME W3-02/W3-06 approval plumbing GET/POST
// /policy/approvals and /policy/feedback already use:
//
//   - GET /approvals/pending — the exact same pending-approval list
//     GET /policy/approvals (no ?id=) already answers, reused verbatim
//     under a second, Hammerspoon-facing name.
//   - POST /approvals/{id}/decision {approve bool, typed string} — id
//     comes from the URL path (Go's ServeMux {id} pattern), never the
//     body; there is deliberately NO surface field on the wire at all.
//
// CORE SECURITY INVARIANT (server-stamped surface, never client-supplied):
// approvalDecisionRequest below has no Surface field, so a request body
// that tries to smuggle {"surface":"remote", ...} has nowhere for that
// key to land — json.Unmarshal simply ignores it, by construction, not by
// a runtime check someone could forget to add. handleApprovalsDecision
// always calls Engine.Approve with the hardcoded approvalsDecisionSurface
// constant, because the ONLY way to reach this route at all is over
// kahyad's own UDS control socket (channel-derived, exactly like every
// other route this package mounts) — see approvals_decision_test.go's
// TestApprovalsDecisionSurfaceForgeryIgnored for the regression test.
package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/policy"
)

// approvalsDecisionSurface is the hardcoded, non-configurable surface
// label the UDS control socket implies for every POST
// /approvals/{id}/decision call (see this file's own package doc
// comment). Named as a constant, rather than an inline literal, purely so
// grepping for kahyad's "local" surface call sites reliably finds this
// one too.
const approvalsDecisionSurface = "local"

// approvalDecisionRequest is POST /approvals/{id}/decision's request body
// — approve/typed ONLY, deliberately no surface field (see this file's
// own package doc comment).
type approvalDecisionRequest struct {
	Approve bool   `json:"approve"`
	Typed   string `json:"typed,omitempty"`
}

type approvalDecisionResponse struct {
	OK    bool   `json:"ok"`
	Token string `json:"token,omitempty"`
	Error string `json:"error,omitempty"`
}

// handleApprovalsPending implements GET /approvals/pending (W6-01): the
// exact same pending-approval list handlePolicyApprovals' own list branch
// already answers (approvals.go) — a bare GET /approvals/pending carries
// no "id" query parameter, so that handler's own list branch is always
// what runs; this route is a second, Hammerspoon-facing name for it, not
// a second implementation.
func (s *Server) handleApprovalsPending(w http.ResponseWriter, r *http.Request) {
	s.handlePolicyApprovals(w, r)
}

// handleApprovalsDecision implements POST /approvals/{id}/decision
// (W6-01): id comes from the URL path (Go 1.22+ ServeMux {id} pattern),
// never the body. Delegates ALL verification (hash-of-approved-bytes, NFC
// normalize + bidi/zero-width/homoglyph strip, one-time consume, W3
// byte-exact typed-"onayla" — W3-06/W6-01) to the same
// kahyad/internal/policy.Engine.Approve/Deny GET/POST /policy/feedback
// already uses; this route only adds the hardcoded server-side surface
// stamp and accepts `typed` on the wire without a `surface` field at all.
func (s *Server) handleApprovalsDecision(w http.ResponseWriter, r *http.Request) {
	if s.denyAll || s.policyEngine == nil {
		writeJSON(w, http.StatusServiceUnavailable, approvalDecisionResponse{OK: false, Error: "policy engine not available"})
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, approvalDecisionResponse{OK: false, Error: "id must not be empty"})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, policyCheckMaxBody)
	var req approvalDecisionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, approvalDecisionResponse{OK: false, Error: "invalid request body"})
		return
	}

	if !req.Approve {
		if err := s.policyEngine.Deny(r.Context(), id); err != nil {
			writeJSON(w, http.StatusBadRequest, approvalDecisionResponse{OK: false, Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, approvalDecisionResponse{OK: true})
		return
	}

	// approvalsDecisionSurface, NEVER req.<anything> — see this file's own
	// package doc comment and the type's own field list (there is no
	// Surface field to read one from in the first place).
	result, err := s.policyEngine.Approve(r.Context(), id, approvalsDecisionSurface, req.Typed)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, approvalDecisionResponse{OK: false, Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, approvalDecisionResponse{OK: true, Token: result.Token})
}

// ---- POST /debug/emit-approval ----

// debugEmitApprovalRequest is POST /debug/emit-approval's request body:
// `kahya debug emit-approval --class W2|W3`'s own wire shape.
type debugEmitApprovalRequest struct {
	Class string `json:"class"`
}

type debugEmitApprovalResponse struct {
	ID    string `json:"id,omitempty"`
	Error string `json:"error,omitempty"`
}

// MsgDebugEmitApprovalRefused is the Turkish refusal string
// handleDebugEmitApproval answers with outside KAHYA_ENV=dev — byte-exact,
// CLAUDE.md language policy.
const MsgDebugEmitApprovalRefused = "kahya debug emit-approval yalnızca KAHYA_ENV=dev altında kullanılabilir."

// handleDebugEmitApproval implements POST /debug/emit-approval (W6-01):
// mints a synthetic pending_approvals row for the requested class (W2 or
// W3) via kahyad/internal/policy.Engine.DebugEmitPendingApproval, purely
// so a developer/reviewer can exercise the Hammerspoon approval-card flow
// end to end without a real W2/W3 tool call. Refuses (403) unless
// s.cfg.Env == config.EnvDev — checked HERE, server-side, on every call;
// `kahya debug emit-approval`'s own client-side KAHYA_ENV check is UX
// only (an immediate Turkish error without a round trip), never the
// authoritative gate.
func (s *Server) handleDebugEmitApproval(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Env != config.EnvDev {
		writeJSON(w, http.StatusForbidden, debugEmitApprovalResponse{Error: MsgDebugEmitApprovalRefused})
		return
	}
	if s.policyEngine == nil {
		writeJSON(w, http.StatusServiceUnavailable, debugEmitApprovalResponse{Error: "policy engine not available"})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, policyCheckMaxBody)
	var req debugEmitApprovalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, debugEmitApprovalResponse{Error: "invalid request body"})
		return
	}

	traceID := traceIDFromContext(r)
	id, err := s.policyEngine.DebugEmitPendingApproval(r.Context(), traceID, policy.ActionClass(req.Class))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, debugEmitApprovalResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, debugEmitApprovalResponse{ID: id})
}
