// evalmini.go implements W5-05's POST /v1/eval/mini/run route: kahyad's
// own kahyad/internal/eval.Runner does the actual work (load the ~20-line
// baseline, run it against the SAME searcher /v1/memory/search itself
// calls, compare against the previously-ledgered eval.mini.run event,
// append a fresh one) - this file only ever translates HTTP <-> that
// package's own Go API, exactly mirroring consolidation.go's identical
// "thin HTTP wrapper" shape. The CLI (`kahya eval mini`) talks ONLY to
// this route; it never opens brain.db (tasks/README.md's gate rule -
// kahyad is brain.db's sole writer, and here, sole reader of its own
// events table too).
package server

import (
	"context"
	"encoding/json"
	"net/http"

	"kahya/kahyad/internal/eval"
	"kahya/kahyad/internal/policy"
)

// EvalMiniRunner is the narrow surface this route needs -
// *kahyad/internal/eval.Runner satisfies it directly, with no adapter.
type EvalMiniRunner interface {
	Run(ctx context.Context, traceID string) (eval.Outcome, error)
}

// SetEvalMiniRunner wires POST /v1/eval/mini/run to r. Call before
// Prepare(); the route answers 503 until this is set, matching this
// package's existing "unwired dependency" convention (SetSearcher/
// SetReindexer/SetConsolidation).
func (s *Server) SetEvalMiniRunner(r EvalMiniRunner) {
	s.evalMini = r
}

// evalMiniQuestionResult mirrors kahyad/internal/eval.QuestionResult.
type evalMiniQuestionResult struct {
	Q         string `json:"q"`
	Pass      bool   `json:"pass"`
	Abstained bool   `json:"abstained,omitempty"`
	Err       string `json:"err,omitempty"`
}

// evalMiniRunResponse is POST /v1/eval/mini/run's JSON body.
type evalMiniRunResponse struct {
	Total         int                      `json:"total"`
	PassCount     int                      `json:"pass_count"`
	Results       []evalMiniQuestionResult `json:"results"`
	PreviousFound bool                     `json:"previous_found"`
	Regressed     bool                     `json:"regressed"`
	Reasons       []string                 `json:"reasons,omitempty"`
	Error         string                   `json:"error,omitempty"`
}

// handleEvalMiniRun implements POST /v1/eval/mini/run (`kahya eval mini`):
// runs the W5-05 retrieval mini-baseline against memory_search and
// ledgers exactly one eval.mini.run event (never eval.mini.pass - see
// kahyad/internal/eval's own doc comment). Deny-all (W3-01: policy.yaml
// failed to load) refuses this route exactly like /v1/memory/search
// itself does - this endpoint's own Runner calls the SAME searcher.
func (s *Server) handleEvalMiniRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.DenyAll() {
		writeJSONError(w, http.StatusForbidden, policy.ReasonDenyAll)
		return
	}
	if s.evalMini == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "eval mini runner not available")
		return
	}
	traceID := traceIDFromContext(r)
	out, err := s.evalMini.Run(r.Context(), traceID)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(evalMiniRunResponse{Error: err.Error()})
		return
	}
	results := make([]evalMiniQuestionResult, len(out.Report.Results))
	for i, qr := range out.Report.Results {
		results[i] = evalMiniQuestionResult{Q: qr.Q, Pass: qr.Pass, Abstained: qr.Abstained, Err: qr.Err}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(evalMiniRunResponse{
		Total:         out.Report.Total,
		PassCount:     out.Report.PassCount,
		Results:       results,
		PreviousFound: out.PreviousFound,
		Regressed:     out.Regressed,
		Reasons:       out.Reasons,
	})
}
