package mlx

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// FIX-U-2: checkIdle's "zero in-flight" decision and the unload itself
// happen under ONE hold of s.mu, and Do registers in-flight BEFORE
// EnsureRunning - so an idle-unload can never cut a call that is about to
// use the server (connection reset / "exited before becoming healthy",
// surfaced to the user as a bogus "yerel model için bellek yok").

func TestDoRegistersInFlightBeforeEnsureRunning(t *testing.T) {
	entered := make(chan struct{})
	proceed := make(chan struct{})
	var once sync.Once
	check := func() (MemStatus, error) {
		once.Do(func() {
			close(entered)
			<-proceed
		})
		return ParseVMStat(realVMStatFixture)
	}
	port := freePort(t)
	sup := New(Config{
		Cmd:          []string{"python3", "testdata/fake_qwen_server.py", strconv.Itoa(port)},
		Port:         port,
		MemCheck:     check,
		PollInterval: 200 * time.Millisecond,
		StartupGrace: realConnectTimeout,
		Log:          testLogger(t),
	})
	t.Cleanup(sup.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), realConnectTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sup.Do(ctx, 0, func(context.Context) error { return nil }) }()

	select {
	case <-entered:
	case <-time.After(realConnectTimeout):
		t.Fatal("memcheck never entered")
	}
	// The memcheck runs inside EnsureRunning (mlxsup's BeforeSpawn, under
	// mlxsup's lock); this reads mlx's own s.mu - a different lock.
	sup.mu.Lock()
	got := sup.inFlight
	sup.mu.Unlock()
	close(proceed)
	if err := <-done; err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	if got != 1 {
		t.Fatalf("inFlight while Do() was inside EnsureRunning = %d, want 1 (Do must register in-flight BEFORE EnsureRunning so checkIdle can never see zero in-flight for a call that is about to use the server)", got)
	}
	sup.mu.Lock()
	after := sup.inFlight
	sup.mu.Unlock()
	if after != 0 {
		t.Fatalf("inFlight after Do() returned = %d, want 0", after)
	}
}

func TestIdleUnloadDoesNotRaceAcquire(t *testing.T) {
	port := freePort(t)
	var unloads int64
	sup := New(Config{
		Cmd:               []string{"python3", "testdata/fake_qwen_server.py", strconv.Itoa(port)},
		Port:              port,
		MemCheck:          sufficientMemCheck,
		PollInterval:      50 * time.Millisecond,
		StartupGrace:      realConnectTimeout,
		IdleTTL:           time.Millisecond,
		IdleCheckInterval: time.Millisecond,
		OnUnloaded:        func() { atomic.AddInt64(&unloads, 1) },
		Log:               testLogger(t),
	})
	t.Cleanup(sup.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 2*realConnectTimeout)
	defer cancel()
	client := &http.Client{Timeout: 5 * time.Second}
	modelsURL := sup.BaseURL() + "/models"
	deadline := time.Now().Add(20 * time.Second)
	var calls int
	for atomic.LoadInt64(&unloads) < 5 {
		if time.Now().After(deadline) {
			t.Fatalf("only %d idle-unloads in 20s (want >= 5): the stress never exercised the race", atomic.LoadInt64(&unloads))
		}
		calls++
		err := sup.Do(ctx, 0, func(callCtx context.Context) error {
			req, err := http.NewRequestWithContext(callCtx, http.MethodGet, modelsURL, nil)
			if err != nil {
				return err
			}
			resp, err := client.Do(req)
			if err != nil {
				return err
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("GET /v1/models status %d", resp.StatusCode)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("Do() #%d failed while the idle monitor was unloading concurrently (unloads so far = %d): %v", calls, atomic.LoadInt64(&unloads), err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Logf("%d Do() calls across %d idle-unloads, no error", calls, atomic.LoadInt64(&unloads))
}
