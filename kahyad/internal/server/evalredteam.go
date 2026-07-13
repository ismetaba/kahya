// evalredteam.go implements POST /v1/eval/redteam (W78-02): the ONE
// production touchpoint of the red-team eval. The scenarios themselves run in
// the dev-profile process (`kahya eval redteam`, over the dev brain.db); this
// route only RECORDS the counts/hashes-only summary row (eval.redteam.result)
// in the PRODUCTION ledger, via kahyad's own eventLogger, so kahyad stays the
// sole brain.db writer (tasks/README.md gate rule) and no dev-brain content
// ever crosses into prod. This summary row is the evidence W78-06 readiness
// reads.
package server

import (
	"encoding/json"
	"net/http"

	"kahya/kahyad/internal/eval"
	"kahya/kahyad/internal/policy"
)

// evalRedteamRecordRequest is POST /v1/eval/redteam's JSON body: the
// counts/hashes-only summary produced by a completed red-team run. NO dev
// content (no scenario payloads, no brain rows) is ever accepted here.
type evalRedteamRecordRequest struct {
	Scenarios       int    `json:"scenarios"`
	Blocked         int    `json:"blocked"`
	Bypasses        int    `json:"bypasses"`
	ScenariosSHA256 string `json:"scenarios_sha256"`
}

// evalRedteamRecordResponse is POST /v1/eval/redteam's JSON reply.
type evalRedteamRecordResponse struct {
	Recorded bool   `json:"recorded"`
	Error    string `json:"error,omitempty"`
}

// handleEvalRedteamRecord implements POST /v1/eval/redteam. It appends one
// eval.redteam.result event (kind = eval.EventRedteamResult) to the
// production ledger with the summary's counts + scenarios_sha256 + trace_id.
// Deny-all (a halted daemon) refuses it, matching every other write-adjacent
// route; an unwired eventLogger answers 503.
func (s *Server) handleEvalRedteamRecord(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.DenyAll() {
		writeJSONError(w, http.StatusForbidden, policy.ReasonDenyAll)
		return
	}
	if s.eventLogger == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "eval redteam recorder not available")
		return
	}

	var req evalRedteamRecordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Scenarios <= 0 || req.ScenariosSHA256 == "" {
		writeJSONError(w, http.StatusBadRequest, "summary must carry a positive scenario count and a scenarios_sha256")
		return
	}

	traceID := traceIDFromContext(r)
	payload := map[string]any{
		"event":            eval.EventRedteamResult,
		"scenarios":        req.Scenarios,
		"blocked":          req.Blocked,
		"bypasses":         req.Bypasses,
		"scenarios_sha256": req.ScenariosSHA256,
		"trace_id":         traceID,
	}
	if err := s.eventLogger.LogEvent(r.Context(), traceID, eval.EventRedteamResult, payload); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(evalRedteamRecordResponse{Error: err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(evalRedteamRecordResponse{Recorded: true})
}
