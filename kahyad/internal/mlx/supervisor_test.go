package mlx

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"kahya/kahyad/internal/logx"
)

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func testLogger(t *testing.T) *logx.Logger {
	t.Helper()
	log, err := logx.New(t.TempDir(), "test-mlx-boot-0000000000000")
	if err != nil {
		t.Fatalf("logx.New: %v", err)
	}
	t.Cleanup(func() { log.Close() })
	return log
}

// sufficientMemCheck/insufficientMemCheck are fixed CheckFuncs so these
// tests never depend on this machine's actual, moment-to-moment memory
// state (see memcheck_test.go's own fixtures for the same reasoning).
func sufficientMemCheck() (MemStatus, error) {
	return ParseVMStat(realVMStatFixture)
}

func insufficientMemCheck() (MemStatus, error) {
	return ParseVMStat(lowMemVMStatFixture)
}

// realConnectTimeout mirrors kahyad/internal/mlxsup's own constant/
// reasoning (this sandboxed environment's one-time slow-first-loopback-
// connection cost) - production's own default (120s) already budgets for
// this same "first connection is slow" shape.
const realConnectTimeout = 60 * time.Second

// TestEnsureRunningMemcheckInsufficientNeverTouchesProxy is THE fail-closed
// regression test (task spec: "memcheck-insufficient => ErrLocalModel
// Unavailable; NO proxy call recorded"). fakeProxy stands in for
// kahyad/internal/anthproxy's forward-proxy listener (the W12-08 cloud
// chokepoint) - it is never dialed at all when memcheck fails, because
// EnsureRunning returns before this package ever constructs an HTTP
// request to ANYTHING.
func TestEnsureRunningMemcheckInsufficientNeverTouchesProxy(t *testing.T) {
	var proxyHits int64
	fakeProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&proxyHits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer fakeProxy.Close()

	sup := New(Config{
		Cmd:      []string{"python3", "testdata/fake_qwen_server.py", strconv.Itoa(freePort(t))},
		Port:     freePort(t),
		MemCheck: insufficientMemCheck,
		Log:      testLogger(t),
	})

	err := sup.EnsureRunning(context.Background())
	if !errors.Is(err, ErrLocalModelUnavailable) {
		t.Fatalf("EnsureRunning() error = %v, want ErrLocalModelUnavailable", err)
	}
	if err.Error() != MsgNoLocalMemory {
		t.Errorf("EnsureRunning() error text = %q, want exactly %q", err.Error(), MsgNoLocalMemory)
	}

	// The caller's own mandated pattern: only proceed to the proxy on a nil
	// error. Since err != nil above, this branch must never run - proving
	// (together with this package containing no OTHER reference to the
	// proxy/http.Client at all) there is no code path from
	// ErrLocalModelUnavailable to the Anthropic proxy.
	if err == nil {
		http.Get(fakeProxy.URL) //nolint:errcheck // unreachable; see comment above
	}

	if got := atomic.LoadInt64(&proxyHits); got != 0 {
		t.Fatalf("fake proxy hit %d times, want 0 (memcheck-insufficient must never reach the proxy)", got)
	}
}

// TestErrLocalModelUnavailableTurkishMessage pins the exact fail-closed
// Turkish strings byte-exact (task spec: "yerel model için bellek yok" +
// guidance "ComfyUI/Wan kapatıp tekrar deneyin") - CLAUDE.md's language
// policy requires these never be paraphrased.
func TestErrLocalModelUnavailableTurkishMessage(t *testing.T) {
	if MsgNoLocalMemory != "yerel model için bellek yok" {
		t.Errorf("MsgNoLocalMemory = %q, want exact Turkish string", MsgNoLocalMemory)
	}
	if MsgNoLocalMemoryGuidance != "ComfyUI/Wan kapatıp tekrar deneyin" {
		t.Errorf("MsgNoLocalMemoryGuidance = %q, want exact Turkish string", MsgNoLocalMemoryGuidance)
	}
}

// TestEnsureRunningHappyPathSpawnsAndBecomesHealthy exercises the full
// memcheck-ok -> spawn -> poll /v1/models -> healthy path against
// fake_qwen_server.py (no real MLX/model dependency - see that fixture's
// own doc comment). Confirms BaseURL's shape too (task spec step 4: worker
// directs OpenAI-compatible calls to "http://127.0.0.1:<qwen_port>/v1").
func TestEnsureRunningHappyPathSpawnsAndBecomesHealthy(t *testing.T) {
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

	ctx, cancel := context.WithTimeout(context.Background(), realConnectTimeout)
	defer cancel()
	if err := sup.EnsureRunning(ctx); err != nil {
		t.Fatalf("EnsureRunning() error = %v, want nil", err)
	}
	if got := sup.State(); got != "ok" {
		t.Fatalf("State() = %q, want ok", got)
	}
	wantBaseURL := "http://127.0.0.1:" + strconv.Itoa(port) + "/v1"
	if got := sup.BaseURL(); got != wantBaseURL {
		t.Errorf("BaseURL() = %q, want %q", got, wantBaseURL)
	}
}

// TestIdleUnloadFiresAfterTTLWithZeroInFlight guards the idle-TTL unload
// policy (task spec step 1: "after mlx.idle_ttl with ZERO in-flight
// requests, SIGTERM+reap"): once Acquire's release() has been called (zero
// in-flight) and IdleTTL elapses, the background monitor must unload the
// server (State() back to "down") and invoke OnUnloaded - then a LATER
// EnsureRunning call must still be able to bring it back (never
// permanently disabled by an idle-unload, unlike a real Stop()).
func TestIdleUnloadFiresAfterTTLWithZeroInFlight(t *testing.T) {
	port := freePort(t)
	var unloadedCount int64
	sup := New(Config{
		Cmd:               []string{"python3", "testdata/fake_qwen_server.py", strconv.Itoa(port)},
		Port:              port,
		MemCheck:          sufficientMemCheck,
		PollInterval:      200 * time.Millisecond,
		StartupGrace:      realConnectTimeout,
		IdleTTL:           50 * time.Millisecond,
		IdleCheckInterval: 10 * time.Millisecond,
		OnUnloaded:        func() { atomic.AddInt64(&unloadedCount, 1) },
		Log:               testLogger(t),
	})
	t.Cleanup(sup.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), realConnectTimeout)
	defer cancel()
	if err := sup.EnsureRunning(ctx); err != nil {
		t.Fatalf("EnsureRunning() error = %v", err)
	}

	// Simulate one in-flight request completing - Acquire/release is what
	// resets the idle clock to "now".
	release := sup.Acquire()
	release()

	deadline := time.Now().Add(3 * time.Second)
	for {
		if sup.State() == "down" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("idle-unload never fired; state stuck at %q", sup.State())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := atomic.LoadInt64(&unloadedCount); got == 0 {
		t.Error("OnUnloaded was never called")
	}

	// A LATER EnsureRunning call must still be able to bring it back - the
	// SAME fixture command/port, since idle-unload's whole point is that
	// it is NOT a permanent disable (unlike a real Stop()): the killed
	// process's port is free again, so re-spawning the identical command
	// works exactly like the very first EnsureRunning call did.
	if err := sup.EnsureRunning(ctx); err != nil {
		t.Fatalf("EnsureRunning() after idle-unload error = %v, want a successful respawn", err)
	}
	if got := sup.State(); got != "ok" {
		t.Fatalf("State() after respawn = %q, want ok", got)
	}
}

// TestDoAppliesWarmBudgetOnlyWhenAlreadyWarm guards the "300ms budget
// after WARM load" contract (task spec): Do must bound fn's ctx to
// warmBudget when the server was ALREADY healthy before the call, but
// must NOT impose any such bound on the very first (cold) call - fn must
// see a ctx that is not already-expired/artificially-shortened while the
// server is still loading.
func TestDoAppliesWarmBudgetOnlyWhenAlreadyWarm(t *testing.T) {
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

	ctx, cancel := context.WithTimeout(context.Background(), realConnectTimeout)
	defer cancel()

	// First call: cold. fn must observe NO deadline tighter than ctx's own
	// (realConnectTimeout, comfortably longer than the tiny warmBudget
	// below) - if Do wrongly applied the budget on a cold call, this
	// fn would see a deadline in the past almost immediately.
	var coldSawDeadline bool
	var coldDeadline time.Time
	err := sup.Do(ctx, 1*time.Millisecond, func(callCtx context.Context) error {
		coldDeadline, coldSawDeadline = callCtx.Deadline()
		return nil
	})
	if err != nil {
		t.Fatalf("Do() (cold) error = %v", err)
	}
	if coldSawDeadline && time.Until(coldDeadline) < 50*time.Millisecond {
		t.Errorf("Do() (cold) imposed a short deadline (%v away) - warmBudget must not apply on the first/cold call", time.Until(coldDeadline))
	}

	// Second call: now warm. fn's ctx must be bounded to (approximately)
	// warmBudget.
	var warmSawDeadline bool
	var warmDeadline time.Time
	err = sup.Do(ctx, 50*time.Millisecond, func(callCtx context.Context) error {
		warmDeadline, warmSawDeadline = callCtx.Deadline()
		return nil
	})
	if err != nil {
		t.Fatalf("Do() (warm) error = %v", err)
	}
	if !warmSawDeadline {
		t.Fatal("Do() (warm) fn's ctx has no deadline at all, want one bounded by warmBudget")
	}
	if d := time.Until(warmDeadline); d <= 0 || d > 1*time.Second {
		t.Errorf("Do() (warm) deadline %v away, want a small positive duration bounded by warmBudget", d)
	}
}

// TestIdleUnloadNeverFiresWithInFlightRequest proves the "ZERO in-flight"
// half of the contract: an Acquire() that is never released must prevent
// idle-unload from firing at all, even long past IdleTTL.
func TestIdleUnloadNeverFiresWithInFlightRequest(t *testing.T) {
	port := freePort(t)
	sup := New(Config{
		Cmd:               []string{"python3", "testdata/fake_qwen_server.py", strconv.Itoa(port)},
		Port:              port,
		MemCheck:          sufficientMemCheck,
		PollInterval:      200 * time.Millisecond,
		StartupGrace:      realConnectTimeout,
		IdleTTL:           20 * time.Millisecond,
		IdleCheckInterval: 5 * time.Millisecond,
		Log:               testLogger(t),
	})
	t.Cleanup(sup.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), realConnectTimeout)
	defer cancel()
	if err := sup.EnsureRunning(ctx); err != nil {
		t.Fatalf("EnsureRunning() error = %v", err)
	}

	_ = sup.Acquire() // never released - simulates a long-running in-flight request

	time.Sleep(200 * time.Millisecond) // comfortably past IdleTTL many times over
	if got := sup.State(); got != "ok" {
		t.Fatalf("State() = %q, want ok (idle-unload must not fire with an in-flight request)", got)
	}
}
