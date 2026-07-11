package osascript

// osascript_test_helpers_test.go: fakes shared by every _test.go file in
// this package — mirrors mcp/fs/server_test.go's and mcp/shell/
// shell_test_helpers_test.go's identical fakeLedger/fakePolicyClient/
// fakeLogger/fakeExecutor convention (duplicated by hand rather than
// exported from either package, matching their own "kept in sync by hand
// across packages that intentionally don't share code" posture).

import (
	"context"
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

// ---- fakePolicyClient ----

type fakePolicyClient struct {
	mu           sync.Mutex
	decision     PolicyDecision
	checkErr     error
	consumeErr   error
	checkCalls   int
	consumeCalls int

	// lastCheckToolInput/lastConsumeToolInput capture the EXACT bytes
	// this package's Runner passed at each call — tests assert on these
	// to prove the tool_input envelope carries exactly what the spec
	// says and nothing more (e.g. shortcuts_run's {name, input_path}).
	lastCheckToolInput   []byte
	lastConsumeToolInput []byte
}

func (f *fakePolicyClient) Check(_ context.Context, _, _, _, _ string, toolInput []byte) (PolicyDecision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkCalls++
	f.lastCheckToolInput = append([]byte(nil), toolInput...)
	if f.checkErr != nil {
		return PolicyDecision{}, f.checkErr
	}
	return f.decision, nil
}

func (f *fakePolicyClient) ConsumeToken(_ context.Context, _, _, _, _, _, _ string, toolInput []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.consumeCalls++
	f.lastConsumeToolInput = append([]byte(nil), toolInput...)
	return f.consumeErr
}

func allowDecision(token string) PolicyDecision {
	return PolicyDecision{Result: PolicyResultAllow, Class: "W2", Token: token}
}

// ---- fakeLogger ----

type logLine struct {
	traceID string
	event   string
	args    []any
}

type fakeLogger struct {
	traceID string
	lines   *[]logLine
	mu      *sync.Mutex
}

func newFakeLogger() *fakeLogger {
	lines := make([]logLine, 0)
	return &fakeLogger{lines: &lines, mu: &sync.Mutex{}}
}

func (l *fakeLogger) With(traceID string) Logger {
	return &fakeLogger{traceID: traceID, lines: l.lines, mu: l.mu}
}

func (l *fakeLogger) Info(event string, args ...any)  { l.record(event, args) }
func (l *fakeLogger) Warn(event string, args ...any)  { l.record(event, args) }
func (l *fakeLogger) Error(event string, args ...any) { l.record(event, args) }

func (l *fakeLogger) record(event string, args []any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	*l.lines = append(*l.lines, logLine{traceID: l.traceID, event: event, args: args})
}

// ---- fakeExecutor ----

type execCall struct {
	name  string
	args  []string
	stdin []byte
}

// fakeExecutor is the unit-test stand-in for Executor: never shells out,
// records every invocation, and answers a canned Result/error — or, when
// blockUntilCtxDone is set, blocks until ctx.Done() and returns ctx.Err()
// (the timeout/kill unit test's stand-in for a hung osascript process, per
// this task's own instruction: "stub executor that sleeps").
type fakeExecutor struct {
	mu    sync.Mutex
	calls []execCall

	result            Result
	err               error
	blockUntilCtxDone bool
}

func (f *fakeExecutor) Run(ctx context.Context, name string, args []string, stdin []byte) (Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, execCall{name: name, args: append([]string(nil), args...), stdin: append([]byte(nil), stdin...)})
	block := f.blockUntilCtxDone
	f.mu.Unlock()

	if block {
		<-ctx.Done()
		return Result{ExitCode: -1}, ctx.Err()
	}
	return f.result, f.err
}

func (f *fakeExecutor) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeExecutor) lastCall() (execCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return execCall{}, false
	}
	return f.calls[len(f.calls)-1], true
}
