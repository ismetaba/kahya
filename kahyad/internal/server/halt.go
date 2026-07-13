// halt.go implements POST /halt (W6-03, HANDOFF §6 W6 flag): the UDS
// control-socket route `kahya halt`/hammerspoon/kahya.lua's ⌥⎋ binding
// both call. The actual halt sequence (process-group SIGKILL, docker
// kill, terminal user_halted transition, outbox cancel, approval
// invalidation + token revocation, ledger) lives in
// kahyad/internal/halt.Executor - this file is only the thin HTTP
// surface over it, matching task_durability.go's own "thin surface over a
// sibling package" pattern for GET /v1/task/status + POST
// /v1/task/resolve.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// HaltExecutor is the narrow halt-sequence surface handleHalt needs -
// *kahyad/internal/halt.Executor satisfies this directly.
type HaltExecutor interface {
	HaltTask(ctx context.Context, taskID string) (bool, error)
	HaltAll(ctx context.Context) (int, error)
}

// SetHaltExecutor wires POST /halt (W6-03). Call before Prepare(); the
// route answers 503 until this is called, matching this package's usual
// unwired-dependency posture (SetSearcher/SetReindexer/...).
func (s *Server) SetHaltExecutor(exec HaltExecutor) {
	s.haltExecutor = exec
}

// haltRequest is POST /halt's request body: `{"task_id": "<id>"}` or
// `{"all": true}` (W6-03 task spec step 4). A body with neither (or an
// empty task_id AND all=false) is rejected - halt must never guess what
// the caller meant.
type haltRequest struct {
	TaskID string `json:"task_id,omitempty"`
	All    bool   `json:"all,omitempty"`
}

type haltResponse struct {
	Halted int    `json:"halted"`
	Error  string `json:"error,omitempty"`
}

// handleHalt implements POST /halt. {"all":true} iterates every task in a
// non-terminal running state (kahyad/internal/halt.Executor.HaltAll); a
// {"task_id":"<id>"} body halts exactly that one task
// (Executor.HaltTask) - a missing/unknown/already-terminal task_id is
// documented as a no-op success (0 halted), never an error, so a
// panicked repeat ⌥⎋ press can never surface a scary failure. Response
// is always `{"halted": n}` on success.
func (s *Server) handleHalt(w http.ResponseWriter, r *http.Request) {
	if s.haltExecutor == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "halt executor not available")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, policyCheckMaxBody)
	var req haltRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, haltResponse{Error: "invalid request body"})
		return
	}
	req.TaskID = strings.TrimSpace(req.TaskID)

	if req.All {
		n, err := s.haltExecutor.HaltAll(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, haltResponse{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, haltResponse{Halted: n})
		return
	}

	if req.TaskID == "" {
		writeJSON(w, http.StatusBadRequest, haltResponse{Error: "task_id must not be empty (or set all=true)"})
		return
	}

	haltedNow, err := s.haltExecutor.HaltTask(r.Context(), req.TaskID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, haltResponse{Error: err.Error()})
		return
	}
	n := 0
	if haltedNow {
		n = 1
	}
	writeJSON(w, http.StatusOK, haltResponse{Halted: n})
}
