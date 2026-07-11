package shell

// shell_test_helpers_test.go: fakes shared by runner_test.go and
// hostexec_test.go — mirrors mcp/fs/server_test.go's identical
// fakeLedger/fakePolicyClient convention (duplicated by hand rather than
// exported from mcp/fs, matching that package's own "kept in sync by
// hand across packages that intentionally don't share code" posture).

import (
	"context"
	"errors"
	"sync"
)

// ---- fakeLedger ----

type fakeLedgerEvent struct {
	traceID string
	kind    string
	payload map[string]any
}

type fakeLedger struct {
	mu     sync.Mutex
	events []fakeLedgerEvent
}

func (f *fakeLedger) LogEvent(_ context.Context, traceID, kind string, payload map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, fakeLedgerEvent{traceID: traceID, kind: kind, payload: payload})
	return nil
}

func (f *fakeLedger) find(kind string) []fakeLedgerEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeLedgerEvent
	for _, e := range f.events {
		if e.kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// ---- fakePolicyClient (mirrors mcp/fs/server_test.go's identical type) ----

type fakePolicyClient struct {
	mu             sync.Mutex
	decision       PolicyDecision
	resultByTool   map[string]PolicyDecision
	checkErr       error
	consumeErr     error
	consumedTokens map[string]bool
	checkCalls     int
	consumeCalls   int
}

func (f *fakePolicyClient) Check(_ context.Context, tool, _, _, _ string, _ []byte) (PolicyDecision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkCalls++
	if f.checkErr != nil {
		return PolicyDecision{}, f.checkErr
	}
	if d, ok := f.resultByTool[tool]; ok {
		return d, nil
	}
	return f.decision, nil
}

func (f *fakePolicyClient) ConsumeToken(_ context.Context, token, _, _, _, _, _ string, _ []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.consumeCalls++
	if f.consumeErr != nil {
		return f.consumeErr
	}
	if f.consumedTokens == nil {
		f.consumedTokens = make(map[string]bool)
	}
	if f.consumedTokens[token] {
		return errors.New("policy: approval token invalid, expired, or already consumed")
	}
	f.consumedTokens[token] = true
	return nil
}

func allowDecision(token string) PolicyDecision {
	return PolicyDecision{Result: PolicyResultAllow, Class: "W2", Token: token}
}

// ---- fakeLogger (captures lines so tests can grep JSONL-equivalent
// content — this task's acceptance criteria explicitly want a "docker run
// transcript in JSONL logs" check; this fake is the unit-test stand-in
// for that transcript). ----

type logLine struct {
	traceID string
	event   string
	args    []any
}

type fakeLogger struct {
	mu      sync.Mutex
	traceID string
	lines   *[]logLine
}

func newFakeLogger() *fakeLogger {
	lines := make([]logLine, 0)
	return &fakeLogger{lines: &lines}
}

func (l *fakeLogger) With(traceID string) Logger {
	return &fakeLogger{traceID: traceID, lines: l.lines}
}

func (l *fakeLogger) Info(event string, args ...any)  { l.record(event, args) }
func (l *fakeLogger) Warn(event string, args ...any)  { l.record(event, args) }
func (l *fakeLogger) Error(event string, args ...any) { l.record(event, args) }

func (l *fakeLogger) record(event string, args []any) {
	// lines is a shared *[]logLine across every With(...) child, but this
	// package's tests are single-goroutine per-call, so a plain mutex
	// (not held across the whole slice's lifetime) is sufficient; no
	// package-level shared fakeLogger is ever reused concurrently across
	// tests.
	*l.lines = append(*l.lines, logLine{traceID: l.traceID, event: event, args: args})
}

func (l *fakeLogger) find(event string) []logLine {
	var out []logLine
	for _, ln := range *l.lines {
		if ln.event == event {
			out = append(out, ln)
		}
	}
	return out
}

// argValue returns the value following key in args ("key", value, "key2",
// value2, ...) alternating pairs, or nil if key is absent.
func argValue(args []any, key string) any {
	for i := 0; i+1 < len(args); i += 2 {
		if k, ok := args[i].(string); ok && k == key {
			return args[i+1]
		}
	}
	return nil
}

// ---- fakeExecutor ----

type execCall struct {
	name  string
	args  []string
	stdin []byte
}

// fakeExecutor is the unit-test stand-in for Executor: never shells out,
// records every invocation, and answers from a per-name-prefix ("docker
// run", "docker kill", "docker image inspect", "docker info", "docker ps",
// or a fixed default) canned Result/error — exactly how this task's
// acceptance criteria ("digest pin UNIT-testable without docker", "stub
// executor asserting zero invocations") are satisfied.
type fakeExecutor struct {
	mu    sync.Mutex
	calls []execCall

	// runResult/runErr answer a "docker run ..." invocation (args[0]=="run").
	runResult Result
	runErr    error
	// runBlocksUntilCtxDone, if set, makes a "docker run" call block until
	// ctx.Done() and then return ctx.Err() — the timeout/kill unit test's
	// stand-in for a real long-running container.
	runBlocksUntilCtxDone bool

	// killResult/killErr answer "docker kill <name>".
	killErr error

	// infoHealthy answers "docker info" (defaults to true — healthy).
	infoHealthy bool
	infoSet     bool

	// psOutput answers "docker ps -q --filter label=kahya.task_id".
	psOutput string
}

func (f *fakeExecutor) Run(ctx context.Context, name string, args []string, stdin []byte) (Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, execCall{name: name, args: append([]string(nil), args...), stdin: stdin})
	f.mu.Unlock()

	if len(args) == 0 {
		return Result{}, nil
	}
	switch args[0] {
	case "info":
		healthy := true
		f.mu.Lock()
		if f.infoSet {
			healthy = f.infoHealthy
		}
		f.mu.Unlock()
		if healthy {
			return Result{ExitCode: 0}, nil
		}
		return Result{ExitCode: 1}, nil
	case "kill":
		return Result{}, f.killErr
	case "ps":
		return Result{Stdout: []byte(f.psOutput)}, nil
	case "run":
		if f.runBlocksUntilCtxDone {
			<-ctx.Done()
			return Result{ExitCode: -1}, ctx.Err()
		}
		return f.runResult, f.runErr
	case "image":
		// "docker image inspect --format {{.Id}} <ref>" — handled by
		// digest-checker-specific fakes (fakeDigestChecker below), not
		// this generic executor's own branch; present only so an
		// accidental direct call doesn't panic.
		return Result{}, nil
	default:
		return Result{}, nil
	}
}

func (f *fakeExecutor) callCount(argsPrefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if len(c.args) > 0 && c.args[0] == argsPrefix {
			n++
		}
	}
	return n
}

func (f *fakeExecutor) callsFor(argsPrefix string) []execCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []execCall
	for _, c := range f.calls {
		if len(c.args) > 0 && c.args[0] == argsPrefix {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeExecutor) totalCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// ---- fakeDigestChecker ----

type fakeDigestChecker struct {
	digest string
	err    error
	calls  int
	mu     sync.Mutex
}

func (f *fakeDigestChecker) Digest(_ context.Context, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.digest, f.err
}

// ---- fakeHealthChecker ----

type fakeHealthChecker struct {
	healthy bool
}

func (f *fakeHealthChecker) Healthy(context.Context) bool { return f.healthy }
