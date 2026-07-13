// restoredrill.go implements POST /v1/restore/drill-result (W78-05): the ONE
// production touchpoint of the backup restore drill. The drill itself runs
// entirely against an isolated KAHYA_ENV=restore scratch profile
// (scripts/restore-drill.sh; brain.db under ~/Library/Application Support/
// Kahya-restore, its own socket) - it NEVER opens the production brain.db.
// This route only RECORDS the counts/hashes/flags-only summary row
// (restore.drill.result) in the PRODUCTION ledger, via kahyad's own
// eventLogger, so kahyad stays the sole brain.db writer (tasks/README.md gate
// rule) and no restored/production memory content ever crosses into the
// payload. This summary row is the evidence W78-06 readiness reads.
package server

import (
	"encoding/json"
	"net/http"

	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/restore"
)

// restoreDrillResultRequest is POST /v1/restore/drill-result's JSON body: the
// counts/hashes/flags-only summary produced by a completed drill. NO memory
// content (no <hafiza> block, no chunk text) is ever accepted here - only a
// hash of the reference block, the backup filename, and the pass/fail flag.
type restoreDrillResultRequest struct {
	// OK is the drill's overall pass/fail: true only when integrity_check was
	// ok, user_version matched, the reindex was an incremental no-op, the
	// normalized restored <hafiza> block matched the reference byte-for-byte,
	// and the restored events/episodes counts were >= the reference counts.
	OK bool `json:"ok"`
	// RefQuerySHA is the SHA-256 (lowercase hex) of the NORMALIZED reference
	// <hafiza> block (restore.Normalize applied) the drill proved equivalence
	// against - a hash, never the block itself, so no memory content is
	// recorded.
	RefQuerySHA string `json:"ref_query_sha"`
	// BackupFile is the basename of the brain-YYYYMMDD.db backup the drill
	// restored from (a filename, no content).
	BackupFile string `json:"backup_file"`
}

// restoreDrillResultResponse is POST /v1/restore/drill-result's JSON reply.
type restoreDrillResultResponse struct {
	Recorded bool   `json:"recorded"`
	Error    string `json:"error,omitempty"`
}

// handleRestoreDrillResult implements POST /v1/restore/drill-result. It
// appends one restore.drill.result event (kind =
// restore.EventRestoreDrillResult) to the production ledger carrying only
// {ok, ref_query_sha, backup_file, trace_id}. Deny-all (a halted daemon)
// refuses it, matching every other write-adjacent route; an unwired
// eventLogger answers 503.
func (s *Server) handleRestoreDrillResult(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.DenyAll() {
		writeJSONError(w, http.StatusForbidden, policy.ReasonDenyAll)
		return
	}
	if s.eventLogger == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "restore drill recorder not available")
		return
	}

	var req restoreDrillResultRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// A meaningful drill result must carry the reference hash and the backup it
	// restored from; without them the row would be evidence of nothing.
	if req.RefQuerySHA == "" || req.BackupFile == "" {
		writeJSONError(w, http.StatusBadRequest, "summary must carry ref_query_sha and backup_file")
		return
	}

	traceID := traceIDFromContext(r)
	payload := map[string]any{
		"ok":            req.OK,
		"ref_query_sha": req.RefQuerySHA,
		"backup_file":   req.BackupFile,
		"trace_id":      traceID,
	}
	if err := s.eventLogger.LogEvent(r.Context(), traceID, restore.EventRestoreDrillResult, payload); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(restoreDrillResultResponse{Error: err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(restoreDrillResultResponse{Recorded: true})
}
