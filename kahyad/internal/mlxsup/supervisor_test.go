package mlxsup

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"kahya/kahyad/internal/logx"
)

// freePort asks the OS for an unused TCP port by binding to :0 and
// immediately releasing it - the same "ask, then let a fake server rebind
// it" trick kahyad/internal/anthproxy's own tests use (a small,
// well-understood TOCTOU race that is not worth avoiding here: nothing
// else on a CI/dev box is racing to grab the exact same ephemeral port in
// the handful of milliseconds before the test's own fixture process binds
// it).
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
	log, err := logx.New(t.TempDir(), "test-mlxsup-boot-0000000000000")
	if err != nil {
		t.Fatalf("logx.New: %v", err)
	}
	t.Cleanup(func() { log.Close() })
	return log
}

func healthURL(port int) string {
	return "http://127.0.0.1:" + strconv.Itoa(port) + "/health"
}

// realConnectTimeout bounds every test below that needs a GENUINE
// cross-process 127.0.0.1 TCP round trip to actually succeed (as opposed
// to the crash/timeout tests, which only ever hit an immediate "nothing's
// listening" refusal and stay fast). This dev machine's sandboxed tool
// environment adds a large, one-time, remarkably consistent delay
// (independently reproduced at ~35s across many trials with plain nc,
// curl, a raw Python socket, and Go's own net/http client alike) before
// the FIRST successful connection to ANOTHER PROCESS's IPv4 loopback
// listener - same-process loopback (e.g. net/http/httptest, used by
// kahyad/internal/embed's client tests) is completely unaffected, and
// this is not expected to reproduce on a normal dev machine or CI runner.
// Production's own default (Config{}'s zero-valued StartupGrace resolves
// to 60s in New) already budgets for exactly this shape of "first
// connection is slow" - "first model load is slow" per HANDOFF §4 ⚑ - so
// these tests simply reuse that same real-world-justified number rather
// than inventing a separate, environment-specific constant.
const realConnectTimeout = 60 * time.Second

// TestNewDisabledOnEmptyCmd guards the "disabled" state: a Supervisor
// built with no Cmd must never attempt to spawn anything and must report
// itself disabled forever.
func TestNewDisabledOnEmptyCmd(t *testing.T) {
	sup := New(Config{Name: "embed", Log: testLogger(t)})
	if got := sup.State(); got != StateDisabled {
		t.Fatalf("State() = %q, want %q", got, StateDisabled)
	}
	if err := sup.EnsureRunning(context.Background()); err != ErrDisabled {
		t.Fatalf("EnsureRunning() error = %v, want ErrDisabled", err)
	}
	if got := sup.State(); got != StateDisabled {
		t.Fatalf("State() after EnsureRunning = %q, want %q", got, StateDisabled)
	}
}

// TestEnsureRunningHappyPathThenStop covers the full happy-path lifecycle
// in one test (deliberately consolidated - see realConnectTimeout - so a
// full `go test` run only pays this environment's one-time slow-first-
// connection cost once, not once per scenario): lazy start (W12-11 step
// 2), a cached second call once healthy, and Stop's shutdown contract
// (acceptance criterion: "pgrep -f 'mlx/embed' after ... kahyad shutdown
// -> empty") including that the background restart loop does NOT bring
// the child back afterward.
func TestEnsureRunningHappyPathThenStop(t *testing.T) {
	port := freePort(t)
	sup := New(Config{
		Name:         "embed",
		Cmd:          []string{"python3", "testdata/healthy_server.py", strconv.Itoa(port)},
		HealthURL:    healthURL(port),
		PollInterval: 500 * time.Millisecond,
		StartupGrace: realConnectTimeout,
		MinBackoff:   10 * time.Millisecond,
		MaxBackoff:   50 * time.Millisecond,
		Log:          testLogger(t),
	})

	if got := sup.State(); got != StateDown {
		t.Fatalf("State() before EnsureRunning = %q, want %q", got, StateDown)
	}

	ctx, cancel := context.WithTimeout(context.Background(), realConnectTimeout)
	defer cancel()
	if err := sup.EnsureRunning(ctx); err != nil {
		t.Fatalf("EnsureRunning() error = %v", err)
	}
	if got := sup.State(); got != StateOK {
		t.Fatalf("State() after EnsureRunning = %q, want %q", got, StateOK)
	}

	// A second call, now that the child is already healthy, must return
	// immediately (cached state, no fresh probe) without spawning a
	// second process.
	quickCtx, quickCancel := context.WithTimeout(context.Background(), time.Second)
	defer quickCancel()
	quickStart := time.Now()
	if err := sup.EnsureRunning(quickCtx); err != nil {
		t.Fatalf("second EnsureRunning() error = %v", err)
	}
	if elapsed := time.Since(quickStart); elapsed > time.Second {
		t.Errorf("second EnsureRunning() took %v, want near-instant (cached OK state)", elapsed)
	}

	sup.Stop()
	if got := sup.State(); got != StateDown {
		t.Fatalf("State() after Stop = %q, want %q", got, StateDown)
	}

	// Wait comfortably longer than MaxBackoff: if Stop failed to suppress
	// the restart loop, the child would have been respawned by now.
	time.Sleep(200 * time.Millisecond)
	if got := sup.State(); got != StateDown {
		t.Fatalf("State() after waiting past backoff = %q, want %q (no restart after Stop)", got, StateDown)
	}
}

// TestEnsureRunningWaitsAcrossPollTicks proves the poll loop actually
// keeps polling rather than giving up after one try - the slow-start
// fixture only starts answering /health after its own explicit delay, on
// top of whatever this environment's own connection delay adds (see
// realConnectTimeout).
func TestEnsureRunningWaitsAcrossPollTicks(t *testing.T) {
	port := freePort(t)
	sup := New(Config{
		Name:         "embed",
		Cmd:          []string{"python3", "testdata/slow_start_server.py", strconv.Itoa(port), "0.3"},
		HealthURL:    healthURL(port),
		PollInterval: 200 * time.Millisecond,
		StartupGrace: realConnectTimeout,
		Log:          testLogger(t),
	})
	t.Cleanup(sup.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), realConnectTimeout)
	defer cancel()
	if err := sup.EnsureRunning(ctx); err != nil {
		t.Fatalf("EnsureRunning() error = %v", err)
	}
	if got := sup.State(); got != StateOK {
		t.Fatalf("State() = %q, want %q", got, StateOK)
	}
}

// TestEnsureRunningTimesOutOnStartupGrace guards the bounded-wait half of
// step 2: a child that never becomes healthy within Config.StartupGrace
// must make EnsureRunning return an error rather than block forever. A
// crashing child (never listens on HealthURL at all, so every health
// probe is an immediate "nothing's listening" refusal, not a slow
// cross-process connection) is used here so the test stays fast and
// deterministic.
func TestEnsureRunningTimesOutOnStartupGrace(t *testing.T) {
	port := freePort(t)
	sup := New(Config{
		Name:         "embed",
		Cmd:          []string{"python3", "testdata/crash_immediately.py"},
		HealthURL:    healthURL(port),
		PollInterval: 20 * time.Millisecond,
		StartupGrace: 150 * time.Millisecond,
		MinBackoff:   10 * time.Second, // keep the background restart loop from firing mid-test
		MaxBackoff:   10 * time.Second,
		Log:          testLogger(t),
	})
	t.Cleanup(sup.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sup.EnsureRunning(ctx); err == nil {
		t.Fatal("EnsureRunning() error = nil, want a startup-grace timeout error")
	}
}

// TestSupervisorRestartsOnCrashWithBackoff guards the crash/backoff path
// (W12-11 step 2 + acceptance criterion: "kill the embed service process:
// kahyad log shows restart with backoff"): a child that exits immediately,
// every time, must be respawned repeatedly by the background supervise
// loop without any caller ever calling EnsureRunning again - proving the
// restart is autonomous, not caller-driven. Like the timeout test above,
// this fixture never listens on HealthURL, so it stays fast.
func TestSupervisorRestartsOnCrashWithBackoff(t *testing.T) {
	port := freePort(t)
	sup := New(Config{
		Name:         "embed",
		Cmd:          []string{"python3", "testdata/crash_immediately.py"},
		HealthURL:    healthURL(port),
		PollInterval: 10 * time.Millisecond,
		StartupGrace: 60 * time.Millisecond,
		MinBackoff:   20 * time.Millisecond,
		MaxBackoff:   80 * time.Millisecond,
		Log:          testLogger(t),
	})
	t.Cleanup(sup.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = sup.EnsureRunning(ctx) // expected to time out - the child never answers /health

	// Give the background restart-with-backoff loop a few cycles to run:
	// each cycle is spawn -> immediate exit -> sleep(backoff) -> respawn,
	// so within ~2s it must have cycled through at least one more restart
	// (proving the loop is alive on its own, independent of any caller).
	initial := sup.spawnCountForTest()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if sup.spawnCountForTest() > initial {
			return // observed at least one autonomous respawn
		}
		if time.Now().After(deadline) {
			t.Fatalf("supervisor never respawned; spawnCount stuck at %d", initial)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
