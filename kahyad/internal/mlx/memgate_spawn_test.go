package mlx

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// FIX-I-9: the fail-closed free-memory gate belongs to the SPAWN, not to
// every call. Once the ~17GB model is resident, free memory is necessarily
// below RequiredFreeBytes; a gate re-run on a warm server would refuse
// every request until the idle-TTL unload.

// countingMemCheck returns the sufficient fixture on its first call and the
// insufficient one on every later call - exactly the shape of "17GB model now
// resident, free memory dropped below RequiredFreeBytes".
func countingMemCheck() (check CheckFunc, calls *int32) {
	var n int32
	return func() (MemStatus, error) {
		if atomic.AddInt32(&n, 1) == 1 {
			return ParseVMStat(realVMStatFixture)
		}
		return ParseVMStat(lowMemVMStatFixture)
	}, &n
}

func TestEnsureRunningSkipsMemcheckWhenAlreadyWarm(t *testing.T) {
	check, calls := countingMemCheck()
	port := freePort(t)
	sup := New(Config{
		Cmd:          []string{"python3", "testdata/fake_qwen_server.py", strconv.Itoa(port)},
		Port:         port,
		PollInterval: 200 * time.Millisecond,
		StartupGrace: realConnectTimeout,
		MemCheck:     check,
		Log:          testLogger(t),
	})
	t.Cleanup(sup.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), realConnectTimeout)
	defer cancel()
	if err := sup.EnsureRunning(ctx); err != nil {
		t.Fatalf("first EnsureRunning: %v", err)
	}
	if err := sup.EnsureRunning(ctx); err != nil {
		t.Fatalf("second EnsureRunning while warm = %v, want nil (memcheck must gate the spawn, not the call)", err)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("memcheck calls = %d, want exactly 1 (once per spawn)", got)
	}
}

func TestDoWarmCallSucceedsUnderPostLoadMemoryPressure(t *testing.T) {
	check, calls := countingMemCheck()
	port := freePort(t)
	sup := New(Config{
		Cmd:          []string{"python3", "testdata/fake_qwen_server.py", strconv.Itoa(port)},
		Port:         port,
		PollInterval: 200 * time.Millisecond,
		StartupGrace: realConnectTimeout,
		MemCheck:     check,
		Log:          testLogger(t),
	})
	t.Cleanup(sup.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), realConnectTimeout)
	defer cancel()
	var ran int32
	fn := func(context.Context) error {
		atomic.AddInt32(&ran, 1)
		return nil
	}
	if err := sup.Do(ctx, 300*time.Millisecond, fn); err != nil {
		t.Fatalf("first (cold) Do: %v", err)
	}
	if err := sup.Do(ctx, 300*time.Millisecond, fn); err != nil {
		t.Fatalf("second (warm) Do = %v, want nil (the resident model's own footprint must not refuse warm calls)", err)
	}
	if got := atomic.LoadInt32(&ran); got != 2 {
		t.Fatalf("fn ran %d times, want 2", got)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("memcheck calls = %d, want exactly 1 (once per spawn)", got)
	}
}
