// evalmini_test.go covers POST /v1/eval/mini/run (W5-05).
package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"testing"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/eval"
)

// fakeEvalMiniRunner is a hermetic server.EvalMiniRunner double.
type fakeEvalMiniRunner struct {
	out   eval.Outcome
	err   error
	calls int
}

func (f *fakeEvalMiniRunner) Run(context.Context, string) (eval.Outcome, error) {
	f.calls++
	return f.out, f.err
}

func newEvalMiniTestServer(t *testing.T, runner EvalMiniRunner) *http.Client {
	t.Helper()
	cfg := config.Config{Socket: filepath.Join(shortSocketDir(t), "k.sock")}
	srv := New(cfg, testLogger(t), "v-evalmini-test", healthyDB)
	if runner != nil {
		srv.SetEvalMiniRunner(runner)
	}
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	go srv.Serve() //nolint:errcheck
	t.Cleanup(func() { srv.Shutdown() })
	return unixHTTPClient(cfg.Socket)
}

func TestHandleEvalMiniRunOK(t *testing.T) {
	runner := &fakeEvalMiniRunner{out: eval.Outcome{
		Report: eval.Report{Total: 2, PassCount: 1, Results: []eval.QuestionResult{
			{Q: "a", Pass: true}, {Q: "b", Pass: false, Abstained: true},
		}},
		PreviousFound: true, Regressed: true, Reasons: []string{`regression: "b" passed before, fails now`},
	}}
	client := newEvalMiniTestServer(t, runner)

	resp, err := client.Post("http://kahyad/v1/eval/mini/run", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /v1/eval/mini/run: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var body evalMiniRunResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Total != 2 || body.PassCount != 1 || !body.Regressed || !body.PreviousFound {
		t.Fatalf("body = %+v, want Total=2 PassCount=1 Regressed=true PreviousFound=true", body)
	}
	if len(body.Results) != 2 || body.Results[1].Q != "b" || !body.Results[1].Abstained {
		t.Fatalf("body.Results = %+v", body.Results)
	}
	if runner.calls != 1 {
		t.Fatalf("runner.calls = %d, want 1", runner.calls)
	}
}

func TestHandleEvalMiniRunRunnerError(t *testing.T) {
	runner := &fakeEvalMiniRunner{err: errors.New("baseline file missing")}
	client := newEvalMiniTestServer(t, runner)

	resp, err := client.Post("http://kahyad/v1/eval/mini/run", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /v1/eval/mini/run: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusInternalServerError)
	}
}

func TestHandleEvalMiniRunUnwired(t *testing.T) {
	client := newEvalMiniTestServer(t, nil)
	resp, err := client.Post("http://kahyad/v1/eval/mini/run", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /v1/eval/mini/run: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d (unwired dependency)", resp.StatusCode, http.StatusServiceUnavailable)
	}
}

func TestHandleEvalMiniRunWrongMethod(t *testing.T) {
	client := newEvalMiniTestServer(t, &fakeEvalMiniRunner{})
	resp, err := client.Get("http://kahyad/v1/eval/mini/run")
	if err != nil {
		t.Fatalf("GET /v1/eval/mini/run: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
	}
}

func TestHandleEvalMiniRunDenyAllRefuses(t *testing.T) {
	runner := &fakeEvalMiniRunner{}
	cfg := config.Config{Socket: filepath.Join(shortSocketDir(t), "k.sock")}
	srv := New(cfg, testLogger(t), "v-evalmini-denyall-test", healthyDB)
	srv.SetEvalMiniRunner(runner)
	srv.SetDenyAll()
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	go srv.Serve() //nolint:errcheck
	t.Cleanup(func() { srv.Shutdown() })
	client := unixHTTPClient(cfg.Socket)

	resp, err := client.Post("http://kahyad/v1/eval/mini/run", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /v1/eval/mini/run: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d (deny-all must refuse even a wired runner)", resp.StatusCode, http.StatusForbidden)
	}
	if runner.calls != 0 {
		t.Fatalf("runner.calls = %d, want 0 (deny-all must short-circuit before the runner ever runs)", runner.calls)
	}
}
