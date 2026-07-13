// evalretrieval.go implements W78-01's two UDS routes, both thin HTTP
// wrappers over kahyad/internal/eval (exactly mirroring evalmini.go's shape):
//
//   - POST /v1/eval/retrieval (`kahya eval retrieval`): runs the full
//     retrieval-QA eval against the SAME searcher /v1/memory/search itself
//     calls and ledgers one eval.retrieval.result event.
//   - POST /v1/eval/export-ritual (`kahya eval export-ritual`): drafts
//     candidate dataset JSONL lines from the W5-03 ritual's human labels for
//     MANUAL curation (never writes a file, never auto-appends).
//
// The CLI talks ONLY to these routes; it never opens brain.db (tasks/README.md
// gate rule - kahyad is brain.db's sole reader/writer).
package server

import (
	"context"
	"encoding/json"
	"net/http"

	"kahya/kahyad/internal/eval"
	"kahya/kahyad/internal/policy"
)

// EvalRetrievalRunner is the narrow surface POST /v1/eval/retrieval needs -
// *kahyad/internal/eval.RetrievalRunner satisfies it directly.
type EvalRetrievalRunner interface {
	Run(ctx context.Context, traceID string) (eval.RetrievalOutcome, error)
}

// EvalRitualExporter is the narrow surface POST /v1/eval/export-ritual needs.
type EvalRitualExporter interface {
	ExportRitualCandidates(ctx context.Context) ([]string, error)
}

// SetEvalRetrievalRunner wires both W78-01 routes to their runners. Call
// before Prepare(); the routes answer 503 until this is set, matching this
// package's existing "unwired dependency" convention.
func (s *Server) SetEvalRetrievalRunner(r EvalRetrievalRunner, exporter EvalRitualExporter) {
	s.evalRetrieval = r
	s.evalRitualExporter = exporter
}

// evalRetrievalItemResult mirrors kahyad/internal/eval.ItemResult.
type evalRetrievalItemResult struct {
	ID         string `json:"id"`
	Answerable bool   `json:"answerable"`
	Correct    bool   `json:"correct"`
	Abstained  bool   `json:"abstained"`
}

// evalRetrievalRunResponse is POST /v1/eval/retrieval's JSON body.
type evalRetrievalRunResponse struct {
	Precision     float64                   `json:"precision"`
	Total         int                       `json:"total"`
	Correct       int                       `json:"correct"`
	ModelVer      string                    `json:"model_ver"`
	FusionSHA256  string                    `json:"fusion_sha256"`
	DatasetSHA256 string                    `json:"dataset_sha256"`
	Items         []evalRetrievalItemResult `json:"items"`
	Error         string                    `json:"error,omitempty"`
}

// handleEvalRetrievalRun implements POST /v1/eval/retrieval. Deny-all (W3-01)
// refuses this route exactly like /v1/memory/search does - its own runner
// calls the SAME searcher.
func (s *Server) handleEvalRetrievalRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.DenyAll() {
		writeJSONError(w, http.StatusForbidden, policy.ReasonDenyAll)
		return
	}
	if s.evalRetrieval == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "eval retrieval runner not available")
		return
	}
	traceID := traceIDFromContext(r)
	out, err := s.evalRetrieval.Run(r.Context(), traceID)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(evalRetrievalRunResponse{Error: err.Error()})
		return
	}
	items := make([]evalRetrievalItemResult, len(out.Report.Items))
	for i, it := range out.Report.Items {
		items[i] = evalRetrievalItemResult{ID: it.ID, Answerable: it.Answerable, Correct: it.Correct, Abstained: it.Abstained}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(evalRetrievalRunResponse{
		Precision:     out.Report.Precision,
		Total:         out.Report.Total,
		Correct:       out.Report.Correct,
		ModelVer:      out.ModelVer,
		FusionSHA256:  out.FusionSHA256,
		DatasetSHA256: out.DatasetSHA256,
		Items:         items,
	})
}

// evalExportRitualResponse is POST /v1/eval/export-ritual's JSON body: one
// candidate JSONL draft line per ritual-labeled fact.
type evalExportRitualResponse struct {
	Lines []string `json:"lines"`
	Error string   `json:"error,omitempty"`
}

// handleEvalExportRitual implements POST /v1/eval/export-ritual. This is a
// read-only draft producer (no ledger write, no file write); deny-all still
// refuses it, matching every other memory-touching route.
func (s *Server) handleEvalExportRitual(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.DenyAll() {
		writeJSONError(w, http.StatusForbidden, policy.ReasonDenyAll)
		return
	}
	if s.evalRitualExporter == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "eval ritual exporter not available")
		return
	}
	lines, err := s.evalRitualExporter.ExportRitualCandidates(r.Context())
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(evalExportRitualResponse{Error: err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(evalExportRitualResponse{Lines: lines})
}
