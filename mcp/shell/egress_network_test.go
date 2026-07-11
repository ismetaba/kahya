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

func (s *scriptedExecutor) Run(_ context.Context, _ string, args []string, stdin []byte) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, execCall{name: "docker", args: append([]string(nil), args...), stdin: stdin})
	key := strings.Join(args, " ")
	for prefix, resp := range s.responses {
		if strings.HasPrefix(key, prefix) {
			return resp.result, resp.err
		}
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

	e := NewEgressNetworkEnsurer(exec, testPinnedDigest)
	if err := e.Ensure(context.Background(), 3128); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}

	if len(exec.callsWithPrefix("network create --internal "+EgressNetworkName)) != 1 {
		t.Errorf("expected exactly one 'docker network create --internal %s' call", EgressNetworkName)
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
