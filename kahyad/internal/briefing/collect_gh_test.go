package briefing

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestGHCollectorParsesPRsAndRuns(t *testing.T) {
	runner := &fakeGHRunner{
		PRJSON:  []byte(`[{"number":12,"title":"bump deps"},{"number":13,"title":"fix flake"}]`),
		RunJSON: []byte(`[{"workflowName":"CI","conclusion":"success"}]`),
	}
	c := GHCollector{Runner: runner, Repos: []string{"kahya/gold-token"}}

	prs, runs, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(prs) != 2 || prs[0].Repo != "kahya/gold-token" || prs[0].Number != 12 || prs[0].Title != "bump deps" {
		t.Fatalf("prs = %+v, unexpected", prs)
	}
	if len(runs) != 1 || runs[0].Workflow != "CI" || runs[0].Conclusion != "success" {
		t.Fatalf("runs = %+v, unexpected", runs)
	}

	// Fixed read-only args only (task spec: "gh pr list", "gh run list
	// --limit 10").
	if len(runner.Calls) != 2 {
		t.Fatalf("gh invocations = %d, want 2 (one pr list, one run list)", len(runner.Calls))
	}
	if runner.Calls[0][0] != "pr" || runner.Calls[0][1] != "list" {
		t.Errorf("first call = %v, want to start with [pr list]", runner.Calls[0])
	}
	if runner.Calls[1][0] != "run" || runner.Calls[1][1] != "list" {
		t.Errorf("second call = %v, want to start with [run list]", runner.Calls[1])
	}
}

func TestGHCollectorNoRunnerOrNoReposIsNoop(t *testing.T) {
	prs, runs, err := (GHCollector{}).Collect(context.Background())
	if err != nil || prs != nil || runs != nil {
		t.Fatalf("Collect(no runner/repos) = %v, %v, %v, want nil, nil, nil", prs, runs, err)
	}
}

func TestGHCollectorTitleIsLengthAndCharclassCapped(t *testing.T) {
	longTitle := ""
	for i := 0; i < 50; i++ {
		longTitle += "very long title with\nnewline "
	}
	runner := &fakeGHRunner{PRJSON: []byte(`[{"number":1,"title":"` + jsonEscape(longTitle) + `"}]`)}
	c := GHCollector{Runner: runner, Repos: []string{"kahya/x"}}

	prs, _, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(prs) != 1 {
		t.Fatalf("prs = %+v, want 1 entry", prs)
	}
	if n := len([]rune(prs[0].Title)); n > ghTitleMaxLen {
		t.Errorf("Title rune length = %d, want <= %d", n, ghTitleMaxLen)
	}
	for _, r := range prs[0].Title {
		if r == '\n' {
			t.Fatalf("Title %q still contains a raw newline", prs[0].Title)
		}
	}
}

// jsonEscape is a tiny test-only helper for embedding a raw string
// (possibly containing newlines) inside a hand-written JSON fixture.
func jsonEscape(s string) string {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		if r == '\n' {
			out = append(out, '\\', 'n')
			continue
		}
		out = append(out, string(r)...)
	}
	return string(out)
}

// hangingGHRunner hangs every call whose subcommand (args[0]) is in hang
// until its ctx ends, answers the rest from canned JSON, and records every
// call (goroutine-safe).
type hangingGHRunner struct {
	hang    map[string]bool
	runJSON []byte

	mu    sync.Mutex
	calls [][]string
}

func (r *hangingGHRunner) Run(ctx context.Context, args []string) ([]byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, args)
	r.mu.Unlock()
	if len(args) > 0 && r.hang[args[0]] {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return r.runJSON, nil
}

func (r *hangingGHRunner) callsFor(repo string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.calls {
		for i := 0; i+1 < len(c); i++ {
			if c[i] == "--repo" && c[i+1] == repo {
				n++
			}
		}
	}
	return n
}

// alwaysHangingGHRunner hangs every gh call until its ctx ends - gh stuck
// on a stalled CONNECT through the egress proxy.
type alwaysHangingGHRunner struct{}

func (alwaysHangingGHRunner) Run(ctx context.Context, _ []string) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestGHCollectorPerCallTimeoutSkipsHungCall (FIX-P-8): one hung gh call is
// cut by the per-call timeout, the other subcommand still answers, and the
// failure is reported (never silent).
func TestGHCollectorPerCallTimeoutSkipsHungCall(t *testing.T) {
	runner := &hangingGHRunner{
		hang:    map[string]bool{"pr": true},
		runJSON: []byte(`[{"workflowName":"CI","conclusion":"success"}]`),
	}
	c := GHCollector{Runner: runner, Repos: []string{"kahya/x"}, CallTimeout: 100 * time.Millisecond}

	start := time.Now()
	prs, runs, err := c.Collect(context.Background())
	if el := time.Since(start); el > time.Second {
		t.Fatalf("Collect took %v, want < 1s (per-call timeout 100ms)", el)
	}
	if len(prs) != 0 || len(runs) != 1 {
		t.Fatalf("Collect = %d prs, %d runs; want 0, 1", len(prs), len(runs))
	}
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "gh pr list kahya/x") {
		t.Fatalf("Collect error = %v, want the pr call's deadline error", err)
	}
}

// TestGHCollectorReportsUndecodableOutput (FIX-P-8): a gh reply that is not
// the expected JSON is reported, not silently dropped; an empty reply is
// simply "no items".
func TestGHCollectorReportsUndecodableOutput(t *testing.T) {
	c := GHCollector{Runner: &fakeGHRunner{PRJSON: []byte("not json"), RunJSON: []byte("  \n")}, Repos: []string{"kahya/x"}}
	prs, runs, err := c.Collect(context.Background())
	if len(prs) != 0 || len(runs) != 0 {
		t.Fatalf("Collect = %v, %v; want nothing", prs, runs)
	}
	if err == nil || !strings.Contains(err.Error(), "gh pr list kahya/x: decode") || strings.Contains(err.Error(), "gh run list") {
		t.Fatalf("Collect error = %v, want only the pr decode error", err)
	}
}

// TestGHCollectorOverallBudgetStopsAfterFirstRepo (FIX-P-8): once the
// caller's section budget is spent, the remaining repos are skipped
// without another gh invocation, and the budget error is reported.
func TestGHCollectorOverallBudgetStopsAfterFirstRepo(t *testing.T) {
	runner := &hangingGHRunner{hang: map[string]bool{"pr": true, "run": true}}
	c := GHCollector{Runner: runner, Repos: []string{"kahya/first", "kahya/second"}, CallTimeout: 100 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	_, _, err := c.Collect(ctx)
	if n := runner.callsFor("kahya/second"); n != 0 {
		t.Errorf("gh calls for kahya/second = %d, want 0 (section budget spent on the first repo)", n)
	}
	if n := runner.callsFor("kahya/first"); n != 2 {
		t.Errorf("gh calls for kahya/first = %d, want 2", n)
	}
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "gh collect: kahya/second") {
		t.Fatalf("Collect error = %v, want the budget error naming the skipped repo", err)
	}
}

// TestExecGHRunnerKillsProcessOnDeadline (FIX-P-8): the real exec runner
// kills gh at the ctx deadline, and - with exec.Cmd.WaitDelay - stops
// waiting on a stdout pipe a grandchild keeps open. The second subtest
// asserts on elapsed time: `sh` exits 0 at once (cat reads /dev/null) and
// only the background sleep holds the pipe; before the fix Output() waited
// the full 5s and returned nil.
func TestExecGHRunnerKillsProcessOnDeadline(t *testing.T) {
	t.Run("hung_process_killed", func(t *testing.T) {
		if _, err := exec.LookPath("sleep"); err != nil {
			t.Skip("sleep not on PATH")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := ExecGHRunner{GHPath: "sleep"}.Run(ctx, []string{"30"})
		if el := time.Since(start); el > 2*time.Second {
			t.Fatalf("Run took %v, want < 2s", el)
		}
		if err == nil {
			t.Fatal("Run error = nil, want the killed process's error")
		}
	})
	t.Run("grandchild_holding_stdout", func(t *testing.T) {
		if _, err := exec.LookPath("sh"); err != nil {
			t.Skip("sh not on PATH")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := ExecGHRunner{GHPath: "sh"}.Run(ctx, []string{"-c", "sleep 5 & cat"})
		if el := time.Since(start); el >= 200*time.Millisecond+ghKillWaitDelay+time.Second {
			t.Fatalf("Run took %v, want < %v (WaitDelay must stop the pipe wait)", el, 200*time.Millisecond+ghKillWaitDelay+time.Second)
		}
		if !errors.Is(err, exec.ErrWaitDelay) {
			t.Fatalf("Run error = %v, want exec.ErrWaitDelay (the pipe was closed by force)", err)
		}
	})
}
