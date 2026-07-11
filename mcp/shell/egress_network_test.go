package shell

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// scriptedExecutor is egress_network_test.go's own fake Executor: unlike
// shell_test_helpers_test.go's fakeExecutor (one canned response PER
// docker subcommand, e.g. "run"/"ps"/"info"), this one answers by
// argv-prefix match so a single test can script DIFFERENT outcomes for
// "docker network inspect" vs "docker ps ..." vs "docker exec ... getent"
// - all needed independently to exercise EgressNetworkEnsurer's several
// branches. Any argv with no matching prefix defaults to ExitCode 0
// (success) - the common "everything else just works" case.
type scriptedExecutor struct {
	mu        sync.Mutex
	calls     []execCall
	responses map[string]execResponse
}

type execResponse struct {
	result Result
	err    error
}

func newScriptedExecutor() *scriptedExecutor {
	return &scriptedExecutor{responses: map[string]execResponse{}}
}

func (s *scriptedExecutor) on(argsPrefix string, res Result, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.responses[argsPrefix] = execResponse{result: res, err: err}
}

func (s *scriptedExecutor) Run(_ context.Context, name string, args []string, stdin []byte) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, execCall{name: name, args: append([]string(nil), args...), stdin: stdin})
	key := strings.Join(args, " ")
	// Longest-matching-prefix wins (deterministic — Go map iteration order
	// is randomized, so without this, two registered prefixes that BOTH
	// match the same call (e.g. "network inspect" and the more specific
	// "network inspect kahya-egress --format") would non-deterministically
	// pick either one). This lets a test register a generic prefix for one
	// call shape (a bare existence check) and a MORE SPECIFIC one for a
	// different call sharing the same leading tokens (a --format lookup),
	// with each routed to its own canned response.
	bestPrefix := ""
	var best execResponse
	found := false
	for prefix, resp := range s.responses {
		if strings.HasPrefix(key, prefix) && len(prefix) >= len(bestPrefix) {
			bestPrefix, best, found = prefix, resp, true
		}
	}
	if found {
		return best.result, best.err
	}
	return Result{ExitCode: 0}, nil
}

func (s *scriptedExecutor) callsWithPrefix(prefix string) []execCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []execCall
	for _, c := range s.calls {
		if strings.HasPrefix(strings.Join(c.args, " "), prefix) {
			out = append(out, c)
		}
	}
	return out
}

const testPinnedDigest = "sha256:testpinneddigest0000000000000000000000000000000000000000000"

func TestEgressEnsure_CreatesNetworkAndSidecarWhenAbsent(t *testing.T) {
	exec := newScriptedExecutor()
	exec.on("network inspect", Result{ExitCode: 1}, nil)           // absent
	exec.on("ps -q", Result{ExitCode: 0, Stdout: []byte("")}, nil) // not running
	// More specific than the bare "network inspect" existence-check
	// prefix above (longest-prefix-match routes this call here instead) —
	// BLOCKER E's subnet/gateway lookup, made AFTER the network exists
	// (whether freshly created, as here, or pre-existing).
	exec.on("network inspect "+EgressNetworkName+" --format", Result{ExitCode: 0, Stdout: []byte("172.20.0.0/16 172.20.0.1\n")}, nil)
	exec.on("ssh -- sudo iptables -C INPUT", Result{ExitCode: 1}, nil) // gateway DROP rule absent

	e := NewEgressNetworkEnsurer(exec, testPinnedDigest)
	if err := e.Ensure(context.Background(), 3128); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}

	if len(exec.callsWithPrefix("network create --internal "+EgressNetworkName)) != 1 {
		t.Errorf("expected exactly one 'docker network create --internal %s' call", EgressNetworkName)
	}
	// BLOCKER E: the gateway-isolation DROP rule is inserted, targeting the
	// network's ACTUAL (mocked) subnet/gateway, in the VM's INPUT chain
	// (never DOCKER-USER/FORWARD — see ensureGatewayIsolation's own doc
	// comment for why traffic to the gateway's own address is never
	// forwarded at all).
	dropCalls := exec.callsWithPrefix("ssh -- sudo iptables -I INPUT -s 172.20.0.0/16 -d 172.20.0.1 -j DROP")
	if len(dropCalls) != 1 {
		t.Fatalf("expected exactly one gateway-isolation iptables -I INPUT call, got %d (all calls: %v)", len(dropCalls), exec.calls)
	}
	runCalls := exec.callsWithPrefix("run -d --name " + EgressSidecarName)
	if len(runCalls) != 1 {
		t.Fatalf("expected exactly one sidecar 'docker run -d' call, got %d", len(runCalls))
	}
	joined := strings.Join(runCalls[0].args, " ")
	for _, want := range []string{
		"--network " + EgressNetworkName,
		egressSidecarBaseImage + "@" + testPinnedDigest,
		"TCP-LISTEN:3128,fork,reuseaddr",
		"TCP:host.docker.internal:3128",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("sidecar run args missing %q, got: %s", want, joined)
		}
	}
	if len(exec.callsWithPrefix("network connect bridge "+EgressSidecarName)) != 1 {
		t.Error("expected exactly one 'docker network connect bridge' call attaching the default bridge")
	}
	if len(exec.callsWithPrefix("exec "+EgressSidecarName+" getent hosts host.docker.internal")) != 1 {
		t.Error("expected the host.docker.internal reachability check to run")
	}
}

func TestEgressEnsure_SkipsCreateWhenAlreadyPresent(t *testing.T) {
	exec := newScriptedExecutor()
	exec.on("network inspect", Result{ExitCode: 0}, nil)                         // already exists
	exec.on("ps -q", Result{ExitCode: 0, Stdout: []byte("abc123def456\n")}, nil) // already running
	exec.on("network inspect "+EgressNetworkName+" --format", Result{ExitCode: 0, Stdout: []byte("172.20.0.0/16 172.20.0.1\n")}, nil)
	exec.on("ssh -- sudo iptables -C INPUT", Result{ExitCode: 0}, nil) // gateway DROP rule already present

	e := NewEgressNetworkEnsurer(exec, testPinnedDigest)
	if err := e.Ensure(context.Background(), 3128); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}

	if n := len(exec.callsWithPrefix("network create")); n != 0 {
		t.Errorf("expected zero 'docker network create' calls when the network already exists, got %d", n)
	}
	if n := len(exec.callsWithPrefix("run -d --name " + EgressSidecarName)); n != 0 {
		t.Errorf("expected zero sidecar 'docker run' calls when it is already running, got %d", n)
	}
	// The reachability check still runs every time (cheap, self-healing).
	if len(exec.callsWithPrefix("exec "+EgressSidecarName+" getent")) != 1 {
		t.Error("expected the host.docker.internal reachability check to still run")
	}
	// BLOCKER E: the DROP rule already exists (per the -C script above) —
	// no duplicate -I insert (idempotency).
	if n := len(exec.callsWithPrefix("ssh -- sudo iptables -I INPUT")); n != 0 {
		t.Errorf("expected zero iptables -I INPUT calls when the DROP rule already exists, got %d", n)
	}
}

func TestEgressEnsure_FailsWhenHostDockerInternalUnreachable(t *testing.T) {
	exec := newScriptedExecutor()
	exec.on("network inspect", Result{ExitCode: 0}, nil)
	exec.on("ps -q", Result{ExitCode: 0, Stdout: []byte("abc123")}, nil)
	exec.on("exec "+EgressSidecarName+" getent", Result{ExitCode: 1}, nil) // unreachable

	e := NewEgressNetworkEnsurer(exec, testPinnedDigest)
	err := e.Ensure(context.Background(), 3128)
	if err == nil {
		t.Fatal("expected an error when host.docker.internal is unreachable")
	}
	if !strings.Contains(err.Error(), reasonEgressNetworkFailed) {
		t.Errorf("error = %q, want it to contain %q", err.Error(), reasonEgressNetworkFailed)
	}
}

func TestEgressEnsure_FailsWhenDigestNotPinned(t *testing.T) {
	exec := newScriptedExecutor()
	e := NewEgressNetworkEnsurer(exec, "")

	err := e.Ensure(context.Background(), 3128)
	if err == nil {
		t.Fatal("expected an error when PinnedDigest is empty")
	}
	if !strings.Contains(err.Error(), reasonEgressNetworkFailed) {
		t.Errorf("error = %q, want it to contain %q", err.Error(), reasonEgressNetworkFailed)
	}
	if len(exec.calls) != 0 {
		t.Errorf("expected zero docker calls when the digest is not pinned, got %d", len(exec.calls))
	}
}

// TestEgressEnsure_GatewayIsolation_FailsWhenSubnetGatewayUnavailable
// proves BLOCKER E's fix fails CLOSED (never silently skips the
// isolation step) when the network's subnet/gateway cannot be determined
// — e.g. an unexpected `docker network inspect --format` output shape.
func TestEgressEnsure_GatewayIsolation_FailsWhenSubnetGatewayUnavailable(t *testing.T) {
	exec := newScriptedExecutor()
	exec.on("network inspect", Result{ExitCode: 0}, nil)
	exec.on("ps -q", Result{ExitCode: 0, Stdout: []byte("abc123")}, nil)
	// Malformed/unexpected --format output (neither "absent" nor a valid
	// "subnet gateway" pair).
	exec.on("network inspect "+EgressNetworkName+" --format", Result{ExitCode: 0, Stdout: []byte("garbage")}, nil)

	e := NewEgressNetworkEnsurer(exec, testPinnedDigest)
	err := e.Ensure(context.Background(), 3128)
	if err == nil {
		t.Fatal("expected an error when the network's subnet/gateway cannot be parsed")
	}
	if !strings.Contains(err.Error(), reasonEgressNetworkFailed) {
		t.Errorf("error = %q, want it to contain %q", err.Error(), reasonEgressNetworkFailed)
	}
	if len(exec.callsWithPrefix("run -d --name "+EgressSidecarName)) != 0 {
		t.Error("expected the sidecar to never be started when gateway isolation could not be verified")
	}
}

// TestEgressEnsure_GatewayIsolation_FailsWhenIptablesInsertFails proves
// BLOCKER E's fix fails CLOSED when the colima-VM iptables insert itself
// fails (e.g. no colima backend, sudo denied) — never silently degrading
// to "needs_network:true works, but the gateway pivot is unmitigated".
func TestEgressEnsure_GatewayIsolation_FailsWhenIptablesInsertFails(t *testing.T) {
	exec := newScriptedExecutor()
	exec.on("network inspect", Result{ExitCode: 0}, nil)
	exec.on("ps -q", Result{ExitCode: 0, Stdout: []byte("abc123")}, nil)
	exec.on("network inspect "+EgressNetworkName+" --format", Result{ExitCode: 0, Stdout: []byte("172.20.0.0/16 172.20.0.1\n")}, nil)
	exec.on("ssh -- sudo iptables -C INPUT", Result{ExitCode: 1}, nil) // absent -> insert attempted
	exec.on("ssh -- sudo iptables -I INPUT", Result{ExitCode: 1, Stderr: []byte("iptables: Permission denied")}, nil)

	e := NewEgressNetworkEnsurer(exec, testPinnedDigest)
	err := e.Ensure(context.Background(), 3128)
	if err == nil {
		t.Fatal("expected an error when the gateway-isolation iptables insert fails")
	}
	if !strings.Contains(err.Error(), reasonEgressNetworkFailed) {
		t.Errorf("error = %q, want it to contain %q", err.Error(), reasonEgressNetworkFailed)
	}
	if len(exec.callsWithPrefix("run -d --name "+EgressSidecarName)) != 0 {
		t.Error("expected the sidecar to never be started when gateway isolation failed to apply")
	}
}

func TestEgressEnsure_FailsWhenNetworkCreateFails(t *testing.T) {
	exec := newScriptedExecutor()
	exec.on("network inspect", Result{ExitCode: 1}, nil)
	exec.on("network create", Result{ExitCode: 1, Stderr: []byte("permission denied")}, nil)

	e := NewEgressNetworkEnsurer(exec, testPinnedDigest)
	err := e.Ensure(context.Background(), 3128)
	if err == nil {
		t.Fatal("expected an error when docker network create fails")
	}
	if !strings.Contains(err.Error(), reasonEgressNetworkFailed) {
		t.Errorf("error = %q, want it to contain %q", err.Error(), reasonEgressNetworkFailed)
	}
	if len(exec.callsWithPrefix("run -d --name "+EgressSidecarName)) != 0 {
		t.Error("expected the sidecar to never be started when the network itself failed to create")
	}
}
