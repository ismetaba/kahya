package mlx

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"kahya/kahyad/internal/secretlane"
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

// TestClassifyWarmBudgetDefaultsAndOverride (FIX-P-4): the warm
// classification budget defaults to secretlane.DefaultBudget (HANDOFF S4
// "<300ms") and an explicit Config value (cfg.qwen_classify_budget_ms)
// overrides it. New spawns nothing, so no Cmd is needed.
func TestClassifyWarmBudgetDefaultsAndOverride(t *testing.T) {
	def := New(Config{})
	t.Cleanup(def.Stop)
	if got := def.ClassifyWarmBudget(); got != secretlane.DefaultBudget {
		t.Errorf("default ClassifyWarmBudget() = %v, want %v", got, secretlane.DefaultBudget)
	}
	over := New(Config{ClassifyWarmBudget: 1500 * time.Millisecond})
	t.Cleanup(over.Stop)
	if got := over.ClassifyWarmBudget(); got != 1500*time.Millisecond {
		t.Errorf("override ClassifyWarmBudget() = %v, want 1.5s", got)
	}
	neg := New(Config{ClassifyWarmBudget: -time.Second})
	t.Cleanup(neg.Stop)
	if got := neg.ClassifyWarmBudget(); got != secretlane.DefaultBudget {
		t.Errorf("negative ClassifyWarmBudget() = %v, want the default %v", got, secretlane.DefaultBudget)
	}
}

// TestQwenClassifierAdapterAppliesConfiguredWarmBudget (FIX-P-4): the
// adapter bounds a WARM call by the supervisor's configured
// ClassifyWarmBudget, never a hard-coded 300ms. The fake server answers
// every classification after 600ms (the slow end of the live-measured
// warm range): with the default budget the warm call fails closed
// (deadline), with a 2s budget the same warm call succeeds. The first,
// cold call of each supervisor gets no budget at all (Do's contract).
func TestQwenClassifierAdapterAppliesConfiguredWarmBudget(t *testing.T) {
	for _, tc := range []struct {
		name    string
		budget  time.Duration
		wantErr bool
	}{
		{"default_300ms_fails_closed", 0, true},
		{"configured_2s_succeeds", 2 * time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			port := freePort(t)
			sup := New(Config{
				Cmd:                []string{"python3", "testdata/fake_qwen_server.py", strconv.Itoa(port), "600"},
				Port:               port,
				MemCheck:           sufficientMemCheck,
				PollInterval:       200 * time.Millisecond,
				StartupGrace:       realConnectTimeout,
				ClassifyWarmBudget: tc.budget,
				Log:                testLogger(t),
			})
			t.Cleanup(sup.Stop)
			adapter := NewQwenClassifierAdapter(sup, "qwen3-30b-a3b")

			// Cold call: loads the server; no warm budget applies.
			if _, err := adapter.Classify(context.Background(), "soğuk çağrı"); err != nil {
				t.Fatalf("cold Classify() error = %v, want nil (no budget on a cold load)", err)
			}
			_, err := adapter.Classify(context.Background(), "sıcak çağrı")
			if tc.wantErr {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("warm Classify() error = %v, want a deadline error under the default budget", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("warm Classify() error = %v, want nil under a 2s ClassifyWarmBudget", err)
			}
		})
	}
}

// TestQwenClassifierAdapterWithBudgetOverridesTheInteractiveBudget (G13
// MEM-3): the extractor's adapter bounds a warm call by its OWN budget, not
// the supervisor's interactive qwen_classify_budget_ms - on the SAME warm
// supervisor (default 300ms) a 600ms classification fails closed through the
// default adapter and succeeds through a 2s-budget one.
func TestQwenClassifierAdapterWithBudgetOverridesTheInteractiveBudget(t *testing.T) {
	port := freePort(t)
	sup := New(Config{
		Cmd:          []string{"python3", "testdata/fake_qwen_server.py", strconv.Itoa(port), "600"},
		Port:         port,
		MemCheck:     sufficientMemCheck,
		PollInterval: 200 * time.Millisecond,
		StartupGrace: realConnectTimeout,
		Log:          testLogger(t),
	})
	t.Cleanup(sup.Stop)
	interactive := NewQwenClassifierAdapter(sup, "qwen3-30b-a3b")
	batch := NewQwenClassifierAdapterWithBudget(sup, "qwen3-30b-a3b", 2*time.Second)

	if _, err := batch.Classify(context.Background(), "soğuk çağrı"); err != nil {
		t.Fatalf("cold Classify() error = %v", err)
	}
	if _, err := interactive.Classify(context.Background(), "sıcak çağrı"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("interactive warm Classify() error = %v, want a deadline error under the 300ms default", err)
	}
	if _, err := batch.Classify(context.Background(), "sıcak çağrı"); err != nil {
		t.Fatalf("batch warm Classify() error = %v, want nil under its own 2s budget", err)
	}
}
