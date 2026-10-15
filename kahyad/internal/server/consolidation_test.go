// consolidation_test.go covers GET/POST /v1/consolidation* (W5-02).
package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"reflect"
	"testing"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/consolidation"
)

// fakeConsolidationRunner is a hermetic server.ConsolidationRunner double.
type fakeConsolidationRunner struct {
	diff  string
	found bool
	err   error

	summary    consolidation.RunTotals
	summaryOK  bool
	summaryErr error

	approveCalls, rejectCalls int
	actionErr                 error
}

func (f *fakeConsolidationRunner) Show(context.Context) (string, bool, error) {
	return f.diff, f.found, f.err
}

func (f *fakeConsolidationRunner) PendingRunSummary(context.Context) (consolidation.RunTotals, bool, error) {
	return f.summary, f.summaryOK, f.summaryErr
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

// TestHandleConsolidationShowCarriesRunSummary (FIX-B-5 phase 3): GET
// /v1/consolidation carries the pending night's {files, batches, deferred};
// it is left out when that night wrote none or the ledger read fails (the
// diff is still shown).
func TestHandleConsolidationShowCarriesRunSummary(t *testing.T) {
	show := func(t *testing.T, runner *fakeConsolidationRunner) map[string]any {
		t.Helper()
		client := newConsolidationTestServer(t, runner)
		resp, err := client.Get("http://kahyad/v1/consolidation")
		if err != nil {
			t.Fatalf("GET /v1/consolidation: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body["diff"] != "diff content" {
			t.Fatalf("diff = %v, want the diff", body["diff"])
		}
		return body
	}
	body := show(t, &fakeConsolidationRunner{found: true, diff: "diff content",
		summary: consolidation.RunTotals{Files: 12, Batches: 3, Deferred: 4}, summaryOK: true})
	want := map[string]any{"files": float64(12), "batches": float64(3), "deferred": float64(4)}
	if got, _ := body["summary"].(map[string]any); !reflect.DeepEqual(got, want) {
		t.Fatalf("summary = %v, want %v", body["summary"], want)
	}
	if body := show(t, &fakeConsolidationRunner{found: true, diff: "diff content"}); body["summary"] != nil {
		t.Errorf("summary = %v, want it left out when the night wrote none", body["summary"])
	}
	if body := show(t, &fakeConsolidationRunner{found: true, diff: "diff content", summaryErr: errors.New("ledger down")}); body["summary"] != nil {
		t.Errorf("summary = %v, want it left out on a ledger read error", body["summary"])
	}
}
