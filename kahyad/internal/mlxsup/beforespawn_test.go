package mlxsup

import (
	"context"
	"errors"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// FIX-I-9: Config.BeforeSpawn gates EVERY spawn (the first EnsureRunning
// spawn and every crash-restart alike), under the supervisor's lock; a
// refusal starts no process and leaves the Supervisor retryable.

var errTestGateRefused = errors.New("test: spawn gate refused")

func TestBeforeSpawnErrorPreventsSpawn(t *testing.T) {
	port := freePort(t)
	var calls int32
	sup := New(Config{
		Name:         "qwen",
		Cmd:          []string{"python3", "testdata/healthy_server.py", strconv.Itoa(port)},
		HealthURL:    healthURL(port),
		PollInterval: 20 * time.Millisecond,
		StartupGrace: 500 * time.Millisecond,
		BeforeSpawn: func() error {
			atomic.AddInt32(&calls, 1)
			return errTestGateRefused
		},
		Log: testLogger(t),
	})
	t.Cleanup(sup.Stop)

	err := sup.EnsureRunning(context.Background())
	if !errors.Is(err, errTestGateRefused) {
		t.Fatalf("EnsureRunning() error = %v, want errors.Is(err, the BeforeSpawn error)", err)
	}
	if got := sup.State(); got != StateDown {
		t.Fatalf("State() = %q, want %q", got, StateDown)
	}
	if got := sup.spawnCountForTest(); got != 0 {
		t.Fatalf("spawnCount = %d, want 0 (a refused spawn starts no process)", got)
	}
	// A second call re-runs the gate (it is per spawn attempt, never cached).
	_ = sup.EnsureRunning(context.Background())
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("BeforeSpawn calls = %d, want 2", got)
	}
	if got := sup.spawnCountForTest(); got != 0 {
		t.Fatalf("spawnCount after second refused call = %d, want 0", got)
	}
}

func TestBeforeSpawnRunsOnCrashRestart(t *testing.T) {
	port := freePort(t)
	var calls int32
	sup := New(Config{
		Name:         "qwen",
		Cmd:          []string{"python3", "testdata/crash_immediately.py"},
		HealthURL:    healthURL(port),
		PollInterval: 50 * time.Millisecond,
		StartupGrace: 200 * time.Millisecond,
		MinBackoff:   10 * time.Millisecond,
		MaxBackoff:   20 * time.Millisecond,
		MaxRestarts:  0, // unlimited
		BeforeSpawn: func() error {
			atomic.AddInt32(&calls, 1)
			return nil
		},
		Log: testLogger(t),
	})
	t.Cleanup(sup.Stop)

	if err := sup.EnsureRunning(context.Background()); err == nil {
		t.Fatal("EnsureRunning() = nil, want an error (the child exits immediately)")
	}
	deadline := time.Now().Add(3 * time.Second)
	for atomic.LoadInt32(&calls) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("BeforeSpawn calls = %d after 3s, want >= 2 (the crash-restart path must run the spawn gate too)", atomic.LoadInt32(&calls))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if spawns, gates := sup.spawnCountForTest(), int(atomic.LoadInt32(&calls)); spawns > gates {
		t.Fatalf("spawnCount = %d > BeforeSpawn calls = %d: some spawn bypassed the gate", spawns, gates)
	}
}

// TestBeforeSpawnRefusalAfterCrashAllowsLaterRespawn is the validator's
// extra finding: supervise never clears s.cmd after a crash, so a refused
// crash-restart used to leave s.cmd pointing at the dead process - every
// later EnsureRunning then skipped the spawn and returned "exited before
// becoming healthy" until kahyad restarted (the secret lane down for good).
func TestBeforeSpawnRefusalAfterCrashAllowsLaterRespawn(t *testing.T) {
	port := freePort(t)
	var calls, refusals int32
	var refuse atomic.Bool
	refuse.Store(true)
	sup := New(Config{
		Name:         "qwen",
		Cmd:          []string{"python3", "testdata/crash_immediately.py"},
		HealthURL:    healthURL(port),
		PollInterval: 50 * time.Millisecond,
		StartupGrace: realConnectTimeout,
		MinBackoff:   10 * time.Millisecond,
		MaxBackoff:   20 * time.Millisecond,
		MaxRestarts:  0,
		BeforeSpawn: func() error {
			// The first spawn (the initial EnsureRunning) passes; every
			// later one - the crash-restart - is refused while refuse is
			// set ("free memory dropped while the child was crashing").
			if atomic.AddInt32(&calls, 1) > 1 && refuse.Load() {
				atomic.AddInt32(&refusals, 1)
				return errTestGateRefused
			}
			return nil
		},
		Log: testLogger(t),
	})
	t.Cleanup(sup.Stop)

	if err := sup.EnsureRunning(context.Background()); err == nil {
		t.Fatal("first EnsureRunning() = nil, want an error (the child exits immediately)")
	}

	// Wait for the crash-restart's gate to refuse and the state to settle.
	deadline := time.Now().Add(3 * time.Second)
	for atomic.LoadInt32(&refusals) < 1 || sup.State() != StateDown {
		if time.Now().After(deadline) {
			t.Fatalf("crash-restart gate never refused (calls=%d refusals=%d state=%q)", atomic.LoadInt32(&calls), atomic.LoadInt32(&refusals), sup.State())
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Let any in-flight supervise goroutine finish (it closes the old done).
	time.Sleep(100 * time.Millisecond)
	spawnsBefore := sup.spawnCountForTest()

	// Memory came back: the gate passes again, and the child is now healthy.
	refuse.Store(false)
	port2 := freePort(t)
	sup.mu.Lock()
	sup.cfg.Cmd = []string{"python3", "testdata/healthy_server.py", strconv.Itoa(port2)}
	sup.cfg.HealthURL = healthURL(port2)
	sup.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), realConnectTimeout)
	defer cancel()
	if err := sup.EnsureRunning(ctx); err != nil {
		t.Fatalf("EnsureRunning() after a refused crash-restart = %v, want nil (a later call must re-run the gate and respawn)", err)
	}
	if got := sup.spawnCountForTest(); got <= spawnsBefore {
		t.Fatalf("spawnCount = %d, want > %d (a NEW process must have been spawned)", got, spawnsBefore)
	}
	if got := sup.State(); got != StateOK {
		t.Fatalf("State() = %q, want %q", got, StateOK)
	}
}
