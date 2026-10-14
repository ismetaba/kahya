package shell

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// fakeReceipts mirrors mcp/fs's receipts_test.go fake (FIX-I-4): one
// receipt per (task, tool, args hash) once an effect succeeds.
type fakeReceipts struct {
	mu                  sync.Mutex
	stored              map[string]json.RawMessage
	replayCalls         int
	executeCalls        int
	effectRuns          int
	hashes              []string
	effectCtxCancelable bool
}

func (f *fakeReceipts) key(in ReceiptInput) string {
	return in.TaskID + "|" + in.Tool + "|" + in.ArgsHash
}

func (f *fakeReceipts) Replay(_ context.Context, in ReceiptInput) (json.RawMessage, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replayCalls++
	raw, ok := f.stored[f.key(in)]
	return raw, ok, nil
}

func (f *fakeReceipts) Execute(ctx context.Context, in ReceiptInput, effect func(context.Context) (json.RawMessage, error)) (json.RawMessage, bool, error) {
	f.mu.Lock()
	f.executeCalls++
	f.hashes = append(f.hashes, in.ArgsHash)
	if raw, ok := f.stored[f.key(in)]; ok {
		f.mu.Unlock()
		return raw, true, nil
	}
	f.effectCtxCancelable = ctx.Done() != nil
	f.mu.Unlock()
	raw, err := effect(ctx)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.effectRuns++
	if err != nil {
		return nil, false, err
	}
	if f.stored == nil {
		f.stored = map[string]json.RawMessage{}
	}
	f.stored[f.key(in)] = raw
	return raw, false, nil
}

// rotatingPolicy mints a fresh token per Check, so a second real execution
// would succeed - only a replay keeps the run count at 1.
type rotatingPolicy struct {
	fakePolicyClient
	n int
}

func (p *rotatingPolicy) Check(ctx context.Context, tool, scope, taskID, traceID string, in []byte) (PolicyDecision, error) {
	p.mu.Lock()
	p.n++
	p.decision = allowDecision(fmt.Sprintf("tok-%d", p.n))
	p.mu.Unlock()
	return p.fakePolicyClient.Check(ctx, tool, scope, taskID, traceID, in)
}

func newReceiptRunner(t *testing.T) (*Runner, string, *rotatingPolicy, *fakeLedger, *fakeExecutor, *fakeDigestChecker, *fakeReceipts) {
	t.Helper()
	home, workdir := baseFixture(t)
	exec := &fakeExecutor{runResult: Result{ExitCode: 0, Stdout: []byte("tests passed\n")}}
	digest := &fakeDigestChecker{digest: "sha256:same"}
	pc := &rotatingPolicy{}
	led := &fakeLedger{}
	r := newTestRunner(home, nil, &pc.fakePolicyClient, led, newFakeLogger(), exec, digest, &fakeHealthChecker{healthy: true}, "sha256:same")
	r.Policy = pc
	rex := &fakeReceipts{}
	r.Receipts = rex
	return r, workdir, pc, led, exec, digest, rex
}

// TestRunReplaysReceiptSkipsDocker: a resumed session re-issuing the SAME
// shell_docker call of the SAME task gets the stored output - no second
// container, no second policy decision, not even a digest/daemon check.
func TestRunReplaysReceiptSkipsDocker(t *testing.T) {
	r, workdir, pc, led, exec, digest, rex := newReceiptRunner(t)
	in := RunInput{Script: "make test", Workdir: workdir}

	out1, err := r.Run(context.Background(), "trace-1", "task-1", in)
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	digestCallsAfterFirst := digest.calls
	out2, err := r.Run(context.Background(), "trace-1", "task-1", in)
	if err != nil {
		t.Fatalf("second (resumed) Run: %v", err)
	}
	if !out2.Replayed || out1.Replayed {
		t.Fatalf("Replayed flags = %v/%v, want false/true", out1.Replayed, out2.Replayed)
	}
	out2.Replayed = false
	if out2 != out1 {
		t.Fatalf("replayed output = %+v, want the stored %+v", out2, out1)
	}
	if n := exec.callCount("run"); n != 1 {
		t.Fatalf("docker run invocations = %d, want 1", n)
	}
	if digest.calls != digestCallsAfterFirst {
		t.Fatalf("a replay consulted the digest checker (%d -> %d)", digestCallsAfterFirst, digest.calls)
	}
	if pc.checkCalls != 1 || pc.consumeCalls != 1 {
		t.Fatalf("Check/ConsumeToken = %d/%d, want 1/1", pc.checkCalls, pc.consumeCalls)
	}
	if got := len(led.find("shell_exec")); got != 1 {
		t.Fatalf("shell_exec ledger rows = %d, want 1", got)
	}
	if got := len(led.find("shell_docker_replayed")); got != 1 {
		t.Fatalf("shell_docker_replayed = %d, want 1", got)
	}
	if rex.effectCtxCancelable {
		t.Fatal("the container run used a cancelable context (must be detached from the MCP request)")
	}
}

// TestRunReceiptKeyedOnFullInvocation: the token binds only the script, but
// the receipt key is the whole call - the same script in another workdir,
// with another timeout, env or network request is a different call.
func TestRunReceiptKeyedOnFullInvocation(t *testing.T) {
	r, workdir, _, _, exec, _, rex := newReceiptRunner(t)
	other := filepath.Join(filepath.Dir(workdir), "other-workdir")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	calls := []RunInput{
		{Script: "make test", Workdir: workdir},
		{Script: "make test", Workdir: other},
		{Script: "make test", Workdir: workdir, TimeoutS: 60},
		{Script: "make test", Workdir: workdir, EnvAllowlist: []string{"GOFLAGS"}},
	}
	for i, in := range calls {
		if _, err := r.Run(context.Background(), "trace-1", "task-1", in); err != nil {
			t.Fatalf("Run #%d: %v", i, err)
		}
	}
	if n := exec.callCount("run"); n != len(calls) {
		t.Fatalf("docker run invocations = %d, want %d (each differs from the others)", n, len(calls))
	}
	seen := map[string]bool{}
	for _, h := range rex.hashes {
		seen[h] = true
	}
	if len(seen) != len(calls) {
		t.Fatalf("distinct receipt keys = %d, want %d", len(seen), len(calls))
	}
	// Env order is not part of the call's identity.
	if a, b := shellDockerReceiptHash(RunInput{Script: "x", EnvAllowlist: []string{"A", "B"}}, "/w", 300),
		shellDockerReceiptHash(RunInput{Script: "x", EnvAllowlist: []string{"B", "A"}}, "/w", 300); a != b {
		t.Fatal("env_allowlist order changed the receipt key")
	}
}

// TestRunEffectErrorWritesNoReceipt: a docker invocation that errors is a
// failed effect - nothing is stored, the next identical call runs again.
func TestRunEffectErrorWritesNoReceipt(t *testing.T) {
	r, workdir, _, _, exec, _, rex := newReceiptRunner(t)
	exec.runErr = errors.New("docker: daemon gone")
	in := RunInput{Script: "echo hi", Workdir: workdir}
	if _, err := r.Run(context.Background(), "trace-1", "task-1", in); err == nil {
		t.Fatal("Run with a failing docker invocation = nil error")
	}
	exec.runErr = nil
	if out, err := r.Run(context.Background(), "trace-1", "task-1", in); err != nil || out.Replayed {
		t.Fatalf("retry after a failed effect = (%+v, %v), want a fresh run", out, err)
	}
	if rex.effectRuns != 2 {
		t.Fatalf("effect runs = %d, want 2", rex.effectRuns)
	}
}

func TestHostExecReplaysReceiptSkipsExecutor(t *testing.T) {
	home, _ := baseFixture(t)
	target := filepath.Join(home, "notes.txt")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	exec := &fakeExecutor{}
	pc := &rotatingPolicy{}
	led := &fakeLedger{}
	h := newTestHostExec(home, &pc.fakePolicyClient, led, newFakeLogger(), exec)
	h.Policy = pc
	rex := &fakeReceipts{}
	h.Receipts = rex
	in := HostExecArgs{Command: "stat", Args: []string{target}}

	out1, err := h.Handle(context.Background(), "trace-1", "task-1", in)
	if err != nil {
		t.Fatalf("first Handle: %v", err)
	}
	out2, err := h.Handle(context.Background(), "trace-1", "task-1", in)
	if err != nil {
		t.Fatalf("second Handle: %v", err)
	}
	if !out2.Replayed || len(out2.Argv) != len(out1.Argv) {
		t.Fatalf("second Handle = %+v, want the stored %+v replayed", out2, out1)
	}
	if n := len(exec.callsNamed("stat")); n != 1 {
		t.Fatalf("stat invocations = %d, want 1", n)
	}
	if pc.checkCalls != 1 {
		t.Fatalf("Check calls = %d, want 1", pc.checkCalls)
	}
	if got := len(led.find("hostexec_replayed")); got != 1 {
		t.Fatalf("hostexec_replayed = %d, want 1", got)
	}
}

// TestHostExecGitConfigRefusalClaimsNoReceipt (C-3): the repo-config scan
// stays outside the receipt effect - its refusal is not an execution.
func TestHostExecGitConfigRefusalClaimsNoReceipt(t *testing.T) {
	home, repo := repoFixture(t)
	exec := &fakeExecutor{gitConfigList: &Result{ExitCode: 0, Stdout: []byte("local\x00core.fsmonitor\n/tmp/x\x00")}}
	pc := &rotatingPolicy{}
	h := newTestHostExec(home, &pc.fakePolicyClient, &fakeLedger{}, newFakeLogger(), exec)
	h.Policy = pc
	rex := &fakeReceipts{}
	h.Receipts = rex
	if _, err := h.Handle(context.Background(), "trace-1", "task-1", HostExecArgs{Command: "git", RepoPath: repo, Args: []string{"status"}}); err == nil {
		t.Fatal("git with a dangerous repo config = nil error")
	}
	if rex.executeCalls != 0 {
		t.Fatalf("receipt Execute calls = %d, want 0 for a refusal", rex.executeCalls)
	}
}
