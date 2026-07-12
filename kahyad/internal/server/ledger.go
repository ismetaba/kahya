// ledger.go implements POST /v1/ledger/verify (W4-05): `kahya ledger
// verify`'s server-side leg. kahyad is brain.db's only writer (HANDOFF
// §4/§5), so even a verification RUN's own outcome ledger event
// (anchor.mismatch) must be appended by kahyad itself - the kahya CLI
// process never touches brain.db directly, exactly like every other
// mutating route on this server.
package server

import (
	"context"
	"encoding/json"
	"net/http"

	"kahya/kahyad/internal/anchor"
)

// LedgerVerifier is the tamper-check source POST /v1/ledger/verify calls
// into. kahyad/internal/anchor.Verifier satisfies this directly, with no
// adapter.
type LedgerVerifier interface {
	Verify(ctx context.Context, traceID string) (anchor.VerifyResult, error)
}

// SetLedgerVerifier wires POST /v1/ledger/verify to v. Call before
// Prepare(); the route answers 503 until this is set, matching this
// package's existing "unwired dependency" convention (SetSearcher/
// SetReindexer/SetScheduler).
func (s *Server) SetLedgerVerifier(v LedgerVerifier) {
	s.ledgerVerifier = v
}

// ledgerVerifyResponse is POST /v1/ledger/verify's JSON body.
type ledgerVerifyResponse struct {
	OK              bool   `json:"ok"`
	MismatchEventID int64  `json:"mismatch_event_id,omitempty"`
	Message         string `json:"message,omitempty"`
}

// handleLedgerVerify implements POST /v1/ledger/verify: runs the full
// recompute-from-event-1 tamper check (kahyad/internal/anchor.Verifier.
// Verify already ledgers `anchor.mismatch` + alarms internally on a
// mismatch - task spec step 6) and reports the outcome as JSON. `kahya
// ledger verify` (the CLI) translates {"ok":false,...} into a non-zero
// exit code and prints Message.
func (s *Server) handleLedgerVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.ledgerVerifier == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "ledger verify not available")
		return
	}

	traceID := traceIDFromContext(r)
	result, err := s.ledgerVerifier.Verify(r.Context(), traceID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(ledgerVerifyResponse{
		OK: result.OK, MismatchEventID: result.MismatchEventID, Message: result.Message,
	})
}
