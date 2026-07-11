package mlx

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"
)

// TestQwenClassifierAdapterEndToEnd exercises the full Supervisor ->
// QwenClassifierAdapter -> HTTPQwenClassifier path against
// fake_qwen_server.py's canned "secret_lane: false" response - no real
// MLX/model dependency (see that fixture's own doc comment).
func TestQwenClassifierAdapterEndToEnd(t *testing.T) {
	port := freePort(t)
	sup := New(Config{
		Cmd:          []string{"python3", "testdata/fake_qwen_server.py", strconv.Itoa(port)},
		Port:         port,
		MemCheck:     sufficientMemCheck,
		PollInterval: 200 * time.Millisecond,
		StartupGrace: realConnectTimeout,
		Log:          testLogger(t),
	})
	t.Cleanup(sup.Stop)

	adapter := NewQwenClassifierAdapter(sup, "qwen3-30b-a3b")
	v, err := adapter.Classify(context.Background(), "hiçbir kalıba uymayan sıradan bir metin")
	if err != nil {
		t.Fatalf("Classify() error = %v", err)
	}
	if v.SecretLane {
		t.Errorf("Classify() SecretLane = true, want false (fake server's canned response)")
	}
	if got := sup.State(); got != "ok" {
		t.Errorf("State() after Classify() = %q, want ok (adapter must have brought the server up)", got)
	}
}

// TestQwenClassifierAdapterMemcheckInsufficientFailsClosed proves the
// adapter propagates ErrLocalModelUnavailable (never silently degrades to
// "not secret") when memcheck fails - it never even attempts a spawn or an
// HTTP call.
func TestQwenClassifierAdapterMemcheckInsufficientFailsClosed(t *testing.T) {
	port := freePort(t)
	sup := New(Config{
		Cmd:      []string{"python3", "testdata/fake_qwen_server.py", strconv.Itoa(port)},
		Port:     port,
		MemCheck: insufficientMemCheck,
		Log:      testLogger(t),
	})
	t.Cleanup(sup.Stop)

	adapter := NewQwenClassifierAdapter(sup, "qwen3-30b-a3b")
	_, err := adapter.Classify(context.Background(), "her hangi bir metin")
	if !errors.Is(err, ErrLocalModelUnavailable) {
		t.Fatalf("Classify() error = %v, want ErrLocalModelUnavailable", err)
	}
}
