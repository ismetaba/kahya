// readiness.go implements W78-06's read-only dogfood-readiness endpoint: GET
// /readiness?since=<duration|date>. It assembles the BRAIN.DB-DERIVED gates
// only - the build gates (from the recorded eval.retrieval.result /
// eval.redteam.result / restore.drill.result evidence rows) and the §9
// usage gates (from the W78-04 metrics aggregates over the window). The
// data-loss usage gate is NOT computed here: incidents live in docs/dogfood.md
// (a file the daemon has no view of), so the CLI folds that gate in. The
// north-star readout is REPORTED, never gating (§9 is the contract).
//
// It mirrors metrics.go exactly: a thin HTTP wrapper over pure logic in
// kahyad/internal/readiness, reading through injected read-only seams (the
// evidence EventReader on the query_only connection + the shared read-only
// metrics.Reader). Gating: same least-privilege read-only posture as GET
// /metrics - not a tool invocation, so deny-all does not apply; access is
// bounded by the 0600 UDS.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"kahya/kahyad/internal/metrics"
	"kahya/kahyad/internal/readiness"
)

// ReadinessEvidenceReader is the narrow read seam GET /readiness needs to read
// the recorded build-gate evidence rows. main.go wires an adapter over
// store/sqlcgen.Queries.ListEventsByKind on the query_only connection.
type ReadinessEvidenceReader interface {
	ListEventsByKind(ctx context.Context, kind string) ([]readiness.EventRow, error)
}

// SetReadinessReader wires GET /readiness (W78-06). evidence reads the build-
// gate evidence rows; metricsReader is the SAME read-only metrics.Reader used
// by GET /metrics (reused - no second connection). Call before Prepare(); the
// route answers 503 until both are set, matching this package's "unwired
// dependency" convention.
func (s *Server) SetReadinessReader(evidence ReadinessEvidenceReader, metricsReader MetricsReader) {
	s.readinessEvidence = evidence
	s.readinessMetrics = metricsReader
}

// handleReadiness answers GET /readiness?since=<duration|date>. `since` uses
// the SAME parsing as GET /metrics (a duration "14d"/"36h", an absolute date,
// or empty -> DefaultMetricsSince = the 14-day dogfood window); the window's
// upper bound is now. The body is a readiness.Report JSON with the build gates,
// the daemon-derivable §9 usage gates (data_loss_ok is null - the CLI fills
// it), and the reported north-star readout.
func (s *Server) handleReadiness(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.readinessEvidence == nil || s.readinessMetrics == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "readiness reader not available")
		return
	}

	now := time.Now().UTC()
	since, err := parseSince(strings.TrimSpace(r.URL.Query().Get("since")), now)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	bg, err := readiness.EvaluateBuildGates(r.Context(), s.readinessEvidence, now)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	m, err := s.readinessMetrics.Compute(r.Context(), since, now)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	view := metricsView(m)

	report := readiness.Report{
		BuildGates: bg,
		UsageGates: readiness.EvaluateUsageGates(view),
		NorthStar:  readiness.EvaluateNorthStar(view),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(report)
}

// metricsView projects the full metrics.Metrics onto the SUBSET the readiness
// gates consume (readiness.MetricsView), so kahyad/internal/readiness need not
// import kahyad/internal/metrics (and therefore database/sql + the sqlite
// driver) - keeping the CLI, which imports readiness for the dogfood parser,
// off the brain.db-access dependency graph.
func metricsView(m metrics.Metrics) readiness.MetricsView {
	days := make([]readiness.DayCount, len(m.CommandsPerDay))
	for i, d := range m.CommandsPerDay {
		days[i] = readiness.DayCount{Day: d.Day, Count: d.Count}
	}
	return readiness.MetricsView{
		Since:                    m.Since,
		Until:                    m.Until,
		CommandsPerDay:           days,
		CommandsTotal:            m.CommandsTotal,
		RememberedMoments:        m.RememberedMoments,
		ClarificationTurnRate:    m.ClarificationTurnRate,
		PaletteToFirstTokenP50Ms: m.PaletteToFirstTokenP50Ms,
	}
}
