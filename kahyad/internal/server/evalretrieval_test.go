// evalretrieval_test.go covers POST /v1/eval/retrieval and
// POST /v1/eval/export-ritual (W78-01).
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/eval"
)

type fakeEvalRetrievalRunner struct {
	out   eval.RetrievalOutcome
	err   error
	calls int
}

func (f *fakeEvalRetrievalRunner) Run(context.Context, string) (eval.RetrievalOutcome, error) {
	f.calls++
	return f.out, f.err
}

type fakeRitualExporter struct {
	lines []string
	err   error
}

func (f *fakeRitualExporter) ExportRitualCandidates(context.Context) ([]string, error) {
	return f.lines, f.err
}

func newEvalRetrievalTestServer(t *testing.T, runner EvalRetrievalRunner, exporter EvalRitualExporter, denyAll bool) *http.Client {
	t.Helper()
	cfg := config.Config{Socket: filepath.Join(shortSocketDir(t), "k.sock")}
	srv := New(cfg, testLogger(t), "v-evalretrieval-test", healthyDB)
	if runner != nil || exporter != nil {
		srv.SetEvalRetrievalRunner(runner, exporter)
	}
	if denyAll {
		srv.SetDenyAll()
	}
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	go srv.Serve() //nolint:errcheck
	t.Cleanup(func() { srv.Shutdown() })
	return unixHTTPClient(cfg.Socket)
}

func TestHandleEvalRetrievalRunOK(t *testing.T) {
	runner := &fakeEvalRetrievalRunner{out: eval.RetrievalOutcome{
		Report: eval.RetrievalReport{Total: 2, Correct: 2, Precision: 1.0, Items: []eval.ItemResult{
			{ID: "a", Answerable: true, Correct: true},
			{ID: "b", Answerable: false, Correct: true, Abstained: true},
		}},
		DatasetSHA256: "d1", ModelVer: "m1", FusionSHA256: "f1",
	}}
	client := newEvalRetrievalTestServer(t, runner, &fakeRitualExporter{}, false)

	resp, err := client.Post("http://kahyad/v1/eval/retrieval", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /v1/eval/retrieval: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var body evalRetrievalRunResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Precision != 1.0 || body.Total != 2 || body.Correct != 2 {
		t.Fatalf("body = %+v", body)
	}
	if body.DatasetSHA256 != "d1" || body.ModelVer != "m1" || body.FusionSHA256 != "f1" {
		t.Fatalf("identity fields = %+v", body)
	}
	if len(body.Items) != 2 || !body.Items[1].Abstained {
		t.Fatalf("items = %+v", body.Items)
	}
	if runner.calls != 1 {
		t.Fatalf("runner.calls = %d, want 1", runner.calls)
	}
}

func TestHandleEvalRetrievalRunUnwired(t *testing.T) {
	client := newEvalRetrievalTestServer(t, nil, nil, false)
	resp, err := client.Post("http://kahyad/v1/eval/retrieval", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d (unwired)", resp.StatusCode, http.StatusServiceUnavailable)
	}
}

func TestHandleEvalRetrievalRunDenyAllRefuses(t *testing.T) {
	runner := &fakeEvalRetrievalRunner{}
	client := newEvalRetrievalTestServer(t, runner, &fakeRitualExporter{}, true)
	resp, err := client.Post("http://kahyad/v1/eval/retrieval", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d (deny-all)", resp.StatusCode, http.StatusForbidden)
	}
	if runner.calls != 0 {
		t.Fatalf("runner.calls = %d, want 0 (deny-all short-circuits)", runner.calls)
	}
}

func TestHandleEvalExportRitualOK(t *testing.T) {
	exporter := &fakeRitualExporter{lines: []string{`{"id":"ritual-1"}`, `{"id":"ritual-2"}`}}
	client := newEvalRetrievalTestServer(t, &fakeEvalRetrievalRunner{}, exporter, false)

	resp, err := client.Post("http://kahyad/v1/eval/export-ritual", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var body evalExportRitualResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Lines) != 2 {
		t.Fatalf("lines = %+v, want 2", body.Lines)
	}
}

func TestHandleEvalExportRitualDenyAllRefuses(t *testing.T) {
	client := newEvalRetrievalTestServer(t, &fakeEvalRetrievalRunner{}, &fakeRitualExporter{}, true)
	resp, err := client.Post("http://kahyad/v1/eval/export-ritual", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d (deny-all)", resp.StatusCode, http.StatusForbidden)
	}
}
