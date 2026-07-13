// metrics.go implements W78-04's read-only reporting endpoint: GET
// /metrics?since=<duration|date>. It is a thin HTTP wrapper over
// kahyad/internal/metrics.Reader, which aggregates the events ledger on a
// DEDICATED PRAGMA query_only=ON connection (main.go wires it). The `kahya
// metrics` CLI is the only consumer; it never opens brain.db itself (§4
// locked decision: one db-access path).
//
// Gating: this is read-only local reporting over the events ledger - the same
// least-privilege posture as GET /v1/log (which reads the JSONL logs). It is
// NOT a tool invocation, so it is not subject to the W3-01 deny-all tool
// gate; deny-all denies side-effecting tools, not a metrics readout. Access
// is bounded by the 0600 UDS itself, like every other kahyad route.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"kahya/kahyad/internal/metrics"
)

// DefaultMetricsSince is the window `kahya metrics` defaults to when --since
// is omitted: the last 14 days (the HANDOFF §6 "hafta 2" north-star window).
const DefaultMetricsSince = 14 * 24 * time.Hour

// MetricsReader is the narrow surface GET /metrics needs -
// *kahyad/internal/metrics.Reader satisfies it directly.
type MetricsReader interface {
	Compute(ctx context.Context, since, until time.Time) (metrics.Metrics, error)
}

// SetMetricsReader wires GET /metrics to reader (W78-04). Call before
// Prepare(); the route answers 503 until this is set, matching this package's
// existing "unwired dependency" convention.
func (s *Server) SetMetricsReader(reader MetricsReader) {
	s.metricsReader = reader
}

// handleMetrics answers GET /metrics?since=<duration|date>. `since` accepts a
// duration ("14d", "36h", "90m") or an absolute UTC date ("2026-07-01");
// empty/absent defaults to DefaultMetricsSince. The window's upper bound is
// "now". The body is the metrics.Metrics JSON with stable keys (veri-yok
// metrics serialize as JSON null).
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.metricsReader == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "metrics reader not available")
		return
	}

	now := time.Now().UTC()
	since, err := parseSince(strings.TrimSpace(r.URL.Query().Get("since")), now)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	m, err := s.metricsReader.Compute(r.Context(), since, now)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(m)
}

// parseSince turns a `since` query value into the window's lower bound
// relative to now. Accepted forms, in order:
//   - "" (empty)                -> now - DefaultMetricsSince
//   - "<n>d"                    -> now - n*24h  (Go's ParseDuration lacks "d")
//   - a Go duration ("36h","90m","1h30m") -> now - that duration
//   - an absolute date "YYYY-MM-DD" (UTC midnight)
//
// A negative/zero duration or an unparseable value is a 400-worthy error.
func parseSince(v string, now time.Time) (time.Time, error) {
	if v == "" {
		return now.Add(-DefaultMetricsSince), nil
	}

	// "<n>d" day form.
	if strings.HasSuffix(v, "d") {
		nStr := strings.TrimSuffix(v, "d")
		n, err := strconv.Atoi(nStr)
		if err != nil {
			return time.Time{}, fmt.Errorf("gecersiz since degeri: %q", v)
		}
		if n <= 0 {
			return time.Time{}, fmt.Errorf("since pozitif olmali: %q", v)
		}
		return now.Add(-time.Duration(n) * 24 * time.Hour), nil
	}

	// Absolute date form.
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return t, nil
	}

	// Generic Go duration ("36h", "90m", "1h30m").
	if d, err := time.ParseDuration(v); err == nil {
		if d <= 0 {
			return time.Time{}, fmt.Errorf("since pozitif olmali: %q", v)
		}
		return now.Add(-d), nil
	}

	return time.Time{}, fmt.Errorf("gecersiz since degeri: %q (ornek: 14d, 36h, 2026-07-01)", v)
}
