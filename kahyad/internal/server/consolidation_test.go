// consolidation_test.go covers GET/POST /v1/consolidation* (W5-02).
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/consolidation"
)

// fakeConsolidationRunner is a hermetic server.ConsolidationRunner double.
type fakeConsolidationRunner struct {
	diff  string
	found bool
	err   error

	approveCalls, rejectCalls int
	actionErr                 error
}

func (f *fakeConsolidationRunner) Show(context.Context) (string, bool, error) {
	return f.diff, f.found, f.err
}

func (f *fakeConsolidationRunner) Approve(context.Context, string) error {
	f.approveCalls++
	return f.actionErr
}

func (f *fakeConsolidationRunner) Reject(context.Context, string) error {
	f.rejectCalls++
	return f.actionErr
}

func newConsolidationTestServer(t *testing.T, runner ConsolidationRunner) *http.Client {
	t.Helper()
	cfg := config.Config{Socket: filepath.Join(shortSocketDir(t), "k.sock")}
	srv := New(cfg, testLogger(t), "v-consolidation-test", healthyDB)
	if runner != nil {
		srv.SetConsolidation(runner)
	}
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	go srv.Serve() //nolint:errcheck
	t.Cleanup(func() { srv.Shutdown() })
	return unixHTTPClient(cfg.Socket)
}

func TestHandleConsolidationShowFound(t *testing.T) {
	client := newConsolidationTestServer(t, &fakeConsolidationRunner{found: true, diff: "diff content"})

	resp, err := client.Get("http://kahyad/v1/consolidation")
	if err != nil {
		t.Fatalf("GET /v1/consolidation: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var body consolidationShowResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Found || body.Diff != "diff content" {
		t.Errorf("body = %+v, want found=true diff=%q", body, "diff content")
	}
}

func TestHandleConsolidationShowNotFound(t *testing.T) {
	client := newConsolidationTestServer(t, &fakeConsolidationRunner{found: false})

	resp, err := client.Get("http://kahyad/v1/consolidation")
	if err != nil {
		t.Fatalf("GET /v1/consolidation: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d (found=false is not an error)", resp.StatusCode, http.StatusOK)
	}
	var body consolidationShowResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Found {
		t.Errorf("body.Found = true, want false")
	}
}

func TestHandleConsolidationShowUnwired(t *testing.T) {
	client := newConsolidationTestServer(t, nil)
	resp, err := client.Get("http://kahyad/v1/consolidation")
	if err != nil {
		t.Fatalf("GET /v1/consolidation: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d (unwired dependency)", resp.StatusCode, http.StatusServiceUnavailable)
	}
}

func TestHandleConsolidationApproveOK(t *testing.T) {
	runner := &fakeConsolidationRunner{}
	client := newConsolidationTestServer(t, runner)

	resp, err := client.Post("http://kahyad/v1/consolidation/approve", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /v1/consolidation/approve: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if runner.approveCalls != 1 {
		t.Fatalf("approveCalls = %d, want 1", runner.approveCalls)
	}
}

func TestHandleConsolidationApproveNoPendingIs404(t *testing.T) {
	runner := &fakeConsolidationRunner{actionErr: consolidation.ErrNoPending}
	client := newConsolidationTestServer(t, runner)

	resp, err := client.Post("http://kahyad/v1/consolidation/approve", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /v1/consolidation/approve: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestHandleConsolidationRejectOK(t *testing.T) {
	runner := &fakeConsolidationRunner{}
	client := newConsolidationTestServer(t, runner)

	resp, err := client.Post("http://kahyad/v1/consolidation/reject", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /v1/consolidation/reject: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if runner.rejectCalls != 1 {
		t.Fatalf("rejectCalls = %d, want 1", runner.rejectCalls)
	}
}

func TestHandleConsolidationApproveWrongMethod(t *testing.T) {
	client := newConsolidationTestServer(t, &fakeConsolidationRunner{})
	resp, err := client.Get("http://kahyad/v1/consolidation/approve")
	if err != nil {
		t.Fatalf("GET /v1/consolidation/approve: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
	}
}
