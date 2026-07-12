package consolidation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"kahya/kahyad/internal/mlx"
)

// fakeLocalRunner is an in-memory LocalRunner: Do either calls fn (BaseURL
// then points at a local httptest.Server standing in for mlx_lm.server's
// own OpenAI-compatible endpoint - never a real MLX process) or, when
// doErr is set, fails BEFORE fn is ever invoked - exactly mirroring
// kahyad/internal/mlx.Supervisor.Do's own fail-closed-before-any-call
// contract for an unavailable local model.
type fakeLocalRunner struct {
	baseURL string
	doErr   error
	called  bool
}

func (f *fakeLocalRunner) Do(ctx context.Context, warmBudget time.Duration, fn func(ctx context.Context) error) error {
	f.called = true
	if f.doErr != nil {
		return f.doErr
	}
	return fn(ctx)
}

func (f *fakeLocalRunner) BaseURL() string { return f.baseURL }

// fakeQwenChatServer stands in for mlx_lm.server's /chat/completions
// endpoint (the exact response shape kahyad/internal/secretlane.
// postChatCompletion already expects) - a local httptest.Server, never a
// real MLX process or network call.
func fakeQwenChatServer(t *testing.T, content string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"role": "assistant", "content": content}},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestLocalSessionConsolidateSuccess(t *testing.T) {
	srv := fakeQwenChatServer(t, `{"files": {"finans.md": "new secret content"}}`)
	defer srv.Close()

	runner := &fakeLocalRunner{baseURL: srv.URL}
	session := LocalSession{Sup: runner, Model: "qwen3-30b-a3b"}

	rewrites, err := session.Consolidate(context.Background(), "trace-1", map[string]string{"finans.md": "old secret content"})
	if err != nil {
		t.Fatalf("Consolidate() error = %v", err)
	}
	if rewrites["finans.md"] != "new secret content" {
		t.Fatalf("rewrites = %+v, unexpected", rewrites)
	}
	if !runner.called {
		t.Fatal("LocalRunner.Do was never called")
	}
}

// TestLocalSessionConsolidateLocalUnavailableNeverCallsHTTP is the
// fail-closed test: when the local model is unavailable, LocalSession
// propagates mlx.ErrLocalModelUnavailable (errors.Is-comparable) and the
// underlying Do call structurally never even attempts an HTTP request -
// there is no code path in this type that could fall back to anything
// else.
func TestLocalSessionConsolidateLocalUnavailableNeverCallsHTTP(t *testing.T) {
	runner := &fakeLocalRunner{doErr: mlx.ErrLocalModelUnavailable}
	session := LocalSession{Sup: runner, Model: "qwen3-30b-a3b"}

	_, err := session.Consolidate(context.Background(), "trace-1", map[string]string{"finans.md": "old secret content"})
	if !errors.Is(err, mlx.ErrLocalModelUnavailable) {
		t.Fatalf("Consolidate() error = %v, want errors.Is(mlx.ErrLocalModelUnavailable)", err)
	}
}

func TestLocalSessionEmptyFilesNeverCallsRunner(t *testing.T) {
	runner := &fakeLocalRunner{baseURL: "http://unused.invalid"}
	session := LocalSession{Sup: runner}
	rewrites, err := session.Consolidate(context.Background(), "trace-1", map[string]string{})
	if err != nil {
		t.Fatalf("Consolidate() error = %v", err)
	}
	if len(rewrites) != 0 {
		t.Fatalf("rewrites = %+v, want empty", rewrites)
	}
	if runner.called {
		t.Fatal("LocalRunner.Do was called for an empty file set")
	}
}
