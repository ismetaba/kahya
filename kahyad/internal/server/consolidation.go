// consolidation.go implements the W5-02 nightly-consolidation control
// routes: GET /v1/consolidation (render the pending diff, `kahya
// consolidation show`), POST /v1/consolidation/approve (`kahya
// consolidation approve`), POST /v1/consolidation/reject (`kahya
// consolidation reject`). All three are plain, narrow wrappers over
// kahyad/internal/consolidation.Consolidator - the actual worktree/
// commit/merge/reindex/push mechanics all live there; this file only
// ever translates HTTP <-> that package's own Go API, exactly mirroring
// ledger.go's identical "thin HTTP wrapper" shape for POST /v1/ledger/
// verify.
package server

import (
	"context"
	"encoding/json"
	"net/http"

	"kahya/kahyad/internal/consolidation"
)

// ConsolidationRunner is the narrow surface these routes need -
// *kahyad/internal/consolidation.Consolidator satisfies it directly, with
// no adapter.
type ConsolidationRunner interface {
	Show(ctx context.Context) (diff string, found bool, err error)
	Approve(ctx context.Context, traceID string) error
	Reject(ctx context.Context, traceID string) error
}

// SetConsolidation wires the three /v1/consolidation* routes to r. Call
// before Prepare(); every route answers 503 until this is set, matching
// this package's existing "unwired dependency" convention (SetSearcher/
// SetReindexer/SetLedgerVerifier).
func (s *Server) SetConsolidation(r ConsolidationRunner) {
	s.consolidation = r
}

// consolidationShowResponse is GET /v1/consolidation's JSON body.
type consolidationShowResponse struct {
	Found bool   `json:"found"`
	Diff  string `json:"diff,omitempty"`
}

// handleConsolidationShow implements GET /v1/consolidation: renders the
// pending suggestion's diff (`git diff main...<branch>`, computed live by
// Consolidator.Show) - found=false (200, empty diff) when nothing is
// pending, never an error in that case.
func (s *Server) handleConsolidationShow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.consolidation == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "consolidation not available")
		return
	}
	diff, found, err := s.consolidation.Show(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(consolidationShowResponse{Found: found, Diff: diff})
}

// consolidationActionResponse is POST /v1/consolidation/approve|reject's
// JSON body.
type consolidationActionResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// handleConsolidationApprove implements POST /v1/consolidation/approve:
// merges the pending suggestion to main, deletes the branch/worktree,
// triggers a corpus reindex, then runs the W4-06 nightly git push - all
// inside Consolidator.Approve. consolidation.ErrNoPending surfaces as 404
// (nothing to approve), any other failure as 500.
func (s *Server) handleConsolidationApprove(w http.ResponseWriter, r *http.Request) {
	s.handleConsolidationAction(w, r, func(ctx context.Context, traceID string) error {
		return s.consolidation.Approve(ctx, traceID)
	})
}

// handleConsolidationReject implements POST /v1/consolidation/reject:
// deletes the pending suggestion's branch/worktree and ledgers the
// rejection.
func (s *Server) handleConsolidationReject(w http.ResponseWriter, r *http.Request) {
	s.handleConsolidationAction(w, r, func(ctx context.Context, traceID string) error {
		return s.consolidation.Reject(ctx, traceID)
	})
}

func (s *Server) handleConsolidationAction(w http.ResponseWriter, r *http.Request, action func(ctx context.Context, traceID string) error) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.consolidation == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "consolidation not available")
		return
	}
	traceID := traceIDFromContext(r)
	if err := action(r.Context(), traceID); err != nil {
		status := http.StatusInternalServerError
		if err == consolidation.ErrNoPending {
			status = http.StatusNotFound
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(consolidationActionResponse{OK: false, Error: err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(consolidationActionResponse{OK: true})
}
