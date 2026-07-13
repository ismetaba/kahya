// readiness_test.go covers GET /readiness (W78-06): the read-only readiness
// readout assembled from the recorded build-gate evidence rows + the metrics
// aggregates.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/metrics"
	"kahya/kahyad/internal/readiness"
)

// fakeEvidenceReader is an in-memory ReadinessEvidenceReader keyed by kind.
type fakeEvidenceReader struct {
	byKind map[string][]readiness.EventRow
	err    error
}

func (f fakeEvidenceReader) ListEventsByKind(_ context.Context, kind string) ([]readiness.EventRow, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.byKind[kind], nil
}

func evRow(id int64, payload string) readiness.EventRow {
	return readiness.EventRow{ID: id, Payload: payload, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
}

func newReadinessTestServer(t *testing.T, evidence ReadinessEvidenceReader, mr MetricsReader) *http.Client {
	t.Helper()
	cfg := config.Config{Socket: filepath.Join(shortSocketDir(t), "k.sock")}
	srv := New(cfg, testLogger(t), "v-readiness-test", healthyDB)
	if evidence != nil || mr != nil {
		srv.SetReadinessReader(evidence, mr)
	}
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	go srv.Serve() //nolint:errcheck
	t.Cleanup(func() { srv.Shutdown() })
	return unixHTTPClient(cfg.Socket)
}

func getReadiness(t *testing.T, client *http.Client) (readiness.Report, int) {
	t.Helper()
	resp, err := client.Get("http://kahyad/readiness")
	if err != nil {
		t.Fatalf("GET /readiness: %v", err)
	}
	defer resp.Body.Close()
	var rep readiness.Report
	_ = json.NewDecoder(resp.Body).Decode(&rep)
	return rep, resp.StatusCode
}

// TestHandleReadinessBuildGatesGreen proves the endpoint reports green build
// gates when fresh, green evidence rows exist, and leaves data_loss_ok nil.
func TestHandleReadinessBuildGatesGreen(t *testing.T) {
	evidence := fakeEvidenceReader{byKind: map[string][]readiness.EventRow{
		"eval.retrieval.result": {evRow(1, `{"precision":0.9}`)},
		"eval.redteam.result":   {evRow(2, `{"bypasses":0}`)},
		"restore.drill.result":  {evRow(3, `{"ok":true}`)},
	}}
	mr := &fakeMetricsReader{out: metrics.Metrics{
		Since: time.Now().Add(-14 * 24 * time.Hour), Until: time.Now(),
	}}
	client := newReadinessTestServer(t, evidence, mr)

	rep, status := getReadiness(t, client)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !rep.BuildGates.AllGreen() {
		t.Errorf("build gates should be all green: %+v", rep.BuildGates)
	}
	if rep.UsageGates.DataLossOK != nil {
		t.Error("data_loss_ok must be null from the daemon")
	}
	// Usage gates RED at task time (empty ledger).
	if rep.UsageGates.CommandsPerDayOK || rep.UsageGates.WindowOK || rep.UsageGates.RememberedOK {
		t.Errorf("usage gates should be red on an empty window: %+v", rep.UsageGates)
	}
	if mr.calls != 1 {
		t.Errorf("metrics reader called %d times, want 1", mr.calls)
	}
}

// TestHandleReadinessMissingRowRed proves a missing evidence row => red gate.
func TestHandleReadinessMissingRowRed(t *testing.T) {
	evidence := fakeEvidenceReader{byKind: map[string][]readiness.EventRow{
		"eval.retrieval.result": {evRow(1, `{"precision":0.9}`)},
		// redteam + restore absent
	}}
	mr := &fakeMetricsReader{out: metrics.Metrics{Since: time.Now().Add(-14 * 24 * time.Hour), Until: time.Now()}}
	client := newReadinessTestServer(t, evidence, mr)

	rep, status := getReadiness(t, client)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if rep.BuildGates.AllGreen() {
		t.Fatal("build gates should NOT be all green with missing rows")
	}
	if rep.BuildGates.Redteam.Reason != "veri yok" {
		t.Errorf("missing redteam reason = %q, want 'veri yok'", rep.BuildGates.Redteam.Reason)
	}
}

// TestHandleReadinessUnwired503 proves the route answers 503 until wired.
func TestHandleReadinessUnwired503(t *testing.T) {
	client := newReadinessTestServer(t, nil, nil)
	_, status := getReadiness(t, client)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (unwired)", status)
	}
}

// TestHandleReadinessUsageGreenWithWindow proves the §9 usage gates go green
// once the metrics reader reports a sustained 2-week window.
func TestHandleReadinessUsageGreenWithWindow(t *testing.T) {
	now := time.Now().UTC()
	var days []metrics.DayCount
	total := 0
	for i := 0; i < 14; i++ {
		d := now.Add(time.Duration(-(i + 1)) * 24 * time.Hour).Format("2006-01-02")
		days = append(days, metrics.DayCount{Day: d, Count: 12})
		total += 12
	}
	evidence := fakeEvidenceReader{byKind: map[string][]readiness.EventRow{
		"eval.retrieval.result": {evRow(1, `{"precision":0.9}`)},
		"eval.redteam.result":   {evRow(2, `{"bypasses":0}`)},
		"restore.drill.result":  {evRow(3, `{"ok":true}`)},
	}}
	mr := &fakeMetricsReader{out: metrics.Metrics{
		Since: now.Add(-14 * 24 * time.Hour), Until: now,
		CommandsPerDay: days, CommandsTotal: total, RememberedMoments: 14,
	}}
	client := newReadinessTestServer(t, evidence, mr)

	rep, status := getReadiness(t, client)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !rep.UsageGates.CommandsPerDayOK || !rep.UsageGates.RememberedOK || !rep.UsageGates.WindowOK {
		t.Errorf("usage gates should be green with a full window: %+v", rep.UsageGates)
	}
}

// TestReadinessMethodNotAllowed proves non-GET is rejected.
func TestReadinessMethodNotAllowed(t *testing.T) {
	client := newReadinessTestServer(t, fakeEvidenceReader{}, &fakeMetricsReader{})
	resp, err := client.Post("http://kahyad/readiness", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /readiness: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}
