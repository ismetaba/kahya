package embed

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeSupervisor is a minimal Supervisor stand-in (all these tests run
// against an httptest.Server that is ALWAYS already listening, so
// EnsureRunning has nothing real to lazily start - it just records
// whether/how it was called).
type fakeSupervisor struct {
	err   error
	calls int
}

func (f *fakeSupervisor) EnsureRunning(ctx context.Context) error {
	f.calls++
	return f.err
}

// TestEmbedBatchReordersByResponseIndex guards against silently trusting
// response array order: the stub deliberately returns "data" entries out
// of order, and EmbedBatch must still return vectors in the SAME order as
// the input texts (by "index", not by array position).
func TestEmbedBatchReordersByResponseIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/embeddings" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var req embeddingsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Model != modelName {
			t.Errorf("request model = %q, want %q", req.Model, modelName)
		}
		resp := embeddingsResponse{Data: []embeddingDatum{
			{Index: 2, Embedding: []float32{2}},
			{Index: 0, Embedding: []float32{0}},
			{Index: 1, Embedding: []float32{1}},
		}}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	sup := &fakeSupervisor{}
	c := New(srv.URL, sup)

	vecs, err := c.EmbedBatch(context.Background(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("EmbedBatch: %v", err)
	}
	if sup.calls != 1 {
		t.Errorf("EnsureRunning called %d times, want 1", sup.calls)
	}
	want := [][]float32{{0}, {1}, {2}}
	for i := range want {
		if len(vecs[i]) != 1 || vecs[i][0] != want[i][0] {
			t.Errorf("vecs[%d] = %v, want %v", i, vecs[i], want[i])
		}
	}
}

// TestEmbedBatchEmptyInputNoOp guards the documented empty-input shortcut:
// zero texts must never dial the network or the supervisor.
func TestEmbedBatchEmptyInputNoOp(t *testing.T) {
	sup := &fakeSupervisor{}
	c := New("http://127.0.0.1:1", sup) // deliberately unreachable - must never be dialed
	vecs, err := c.EmbedBatch(context.Background(), nil)
	if err != nil {
		t.Fatalf("EmbedBatch(nil) error = %v", err)
	}
	if vecs != nil {
		t.Errorf("EmbedBatch(nil) = %v, want nil", vecs)
	}
	if sup.calls != 0 {
		t.Errorf("EnsureRunning called %d times, want 0 (empty input is a local no-op)", sup.calls)
	}
}

// TestEmbedBatchOverMaxRejectedLocally guards the "batch <= 64" cap (W12-11
// step 1): a caller passing more than MaxBatch texts must be rejected
// before any HTTP call, so a broken caller can never silently overload the
// embed service or the model_ver invariant's batching contract.
func TestEmbedBatchOverMaxRejectedLocally(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should never be contacted for an over-limit batch")
	}))
	defer srv.Close()

	sup := &fakeSupervisor{}
	c := New(srv.URL, sup)
	texts := make([]string, MaxBatch+1)
	for i := range texts {
		texts[i] = "x"
	}
	if _, err := c.EmbedBatch(context.Background(), texts); err == nil {
		t.Fatal("EmbedBatch with MaxBatch+1 texts: error = nil, want a batch-too-large error")
	}
	if sup.calls != 0 {
		t.Errorf("EnsureRunning called %d times, want 0 (rejected before lazy-start)", sup.calls)
	}
}

// TestEmbedBatchSupervisorErrorPropagates guards the degraded-fallback
// contract's client half (W12-11 step 4): if the embed service cannot be
// started/reached, EmbedBatch must return an error (never panic, never
// silently return zero vectors) so search.Searcher can fall back to
// FTS-only fusion.
func TestEmbedBatchSupervisorErrorPropagates(t *testing.T) {
	sup := &fakeSupervisor{err: errors.New("mlxsup: did not become healthy")}
	c := New("http://127.0.0.1:1", sup)
	if _, err := c.EmbedBatch(context.Background(), []string{"a"}); err == nil {
		t.Fatal("EmbedBatch error = nil, want the supervisor's error wrapped")
	}
}

// TestEmbedBatchServerErrorStatusPropagates guards the non-200 path: the
// stub's {"error": "..."} body must surface in the returned error rather
// than being silently swallowed.
func TestEmbedBatchServerErrorStatusPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "batch too large"})
	}))
	defer srv.Close()

	c := New(srv.URL, &fakeSupervisor{})
	_, err := c.EmbedBatch(context.Background(), []string{"a"})
	if err == nil {
		t.Fatal("EmbedBatch error = nil, want the server's 400 to propagate")
	}
	if got := err.Error(); !strings.Contains(got, "batch too large") {
		t.Errorf("error = %q, want it to mention the server's message", got)
	}
}

// TestEmbedQuerySingleText guards the KNN leg's exact call shape (W12-11
// step 4: "embed the query (1 call)").
func TestEmbedQuerySingleText(t *testing.T) {
	var gotInputs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embeddingsRequest
		json.NewDecoder(r.Body).Decode(&req)
		gotInputs = req.Input
		json.NewEncoder(w).Encode(embeddingsResponse{Data: []embeddingDatum{
			{Index: 0, Embedding: []float32{0.1, 0.2, 0.3}},
		}})
	}))
	defer srv.Close()

	c := New(srv.URL, &fakeSupervisor{})
	vec, err := c.EmbedQuery(context.Background(), "altın projesinde saga nasıl kurulmuştu?")
	if err != nil {
		t.Fatalf("EmbedQuery: %v", err)
	}
	if len(gotInputs) != 1 || gotInputs[0] != "altın projesinde saga nasıl kurulmuştu?" {
		t.Errorf("server saw input %v, want exactly one matching query", gotInputs)
	}
	if len(vec) != 3 {
		t.Errorf("len(vec) = %d, want 3", len(vec))
	}
}
