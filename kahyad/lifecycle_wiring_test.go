package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// mainSource parses main.go and returns the file plus its raw bytes, so a
// wiring test can read a call's argument text exactly as written.
func mainSource(t *testing.T) (*token.FileSet, *ast.File, []byte) {
	t.Helper()
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", src, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	return fset, f, src
}

// mainCall is one call site in main.go: its byte offset and the source
// text of its arguments.
type mainCall struct {
	offset int
	args   string
}

// mainCalls returns the argument text of every call in main.go whose callee
// reads recv.method, in source order.
func mainCalls(t *testing.T, recv, method string) []string {
	t.Helper()
	var out []string
	for _, c := range mainCallSites(t, recv, method) {
		out = append(out, c.args)
	}
	return out
}

// calleeIs reports whether fun is recv.method, or the plain identifier
// method when recv is "".
func calleeIs(fun ast.Expr, recv, method string) bool {
	if recv == "" {
		id, ok := fun.(*ast.Ident)
		return ok && id.Name == method
	}
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != method {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && x.Name == recv
}

// mainCallSites is mainCalls with each call's offset.
func mainCallSites(t *testing.T, recv, method string) []mainCall {
	t.Helper()
	fset, f, src := mainSource(t)
	var out []mainCall
	ast.Inspect(f, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if !calleeIs(c.Fun, recv, method) {
			return true
		}
		var args []string
		for _, a := range c.Args {
			args = append(args, string(src[fset.Position(a.Pos()).Offset:fset.Position(a.End()).Offset]))
		}
		out = append(out, mainCall{offset: fset.Position(c.Pos()).Offset, args: strings.Join(args, ", ")})
		return true
	})
	return out
}

// onlyCallSite fails unless main.go calls recv.method exactly once, with
// exactly wantArgs (when non-empty), and returns that site.
func onlyCallSite(t *testing.T, recv, method, wantArgs string) mainCall {
	t.Helper()
	sites := mainCallSites(t, recv, method)
	if len(sites) != 1 {
		t.Fatalf("%s.%s calls = %d (%+v), want exactly 1", recv, method, len(sites), sites)
	}
	if wantArgs != "" && sites[0].args != wantArgs {
		t.Fatalf("%s.%s(%s), want %s.%s(%s)", recv, method, sites[0].args, recv, method, wantArgs)
	}
	return sites[0]
}

// TestLifecycleContextWiring (FIX-I-5, G00 C-5): main.go creates ONE
// SIGINT/SIGTERM lifecycle context at the very top of run() - before the
// startup lock and the DB, so every later block inherits it - and hands it
// to the outbox dispatcher and srv.Run; the boot orphan sweep runs before
// the startup resume scan; the tick scheduler is stopped explicitly right
// after srv.Run returns (its running dispatch tick ends with the
// dispatcher's base context).
func TestLifecycleContextWiring(t *testing.T) {
	_, _, src := mainSource(t)
	notify := onlyCallSite(t, "signal", "NotifyContext", "context.Background(), syscall.SIGINT, syscall.SIGTERM")
	if !strings.Contains(string(src[:notify.offset]), "lifecycleCtx, stopSignals := ") {
		t.Error("signal.NotifyContext's result must be lifecycleCtx, stopSignals")
	}
	for _, later := range [][2]string{{"server", "AcquireStartupLock"}, {"store", "Open"}} {
		for _, c := range mainCallSites(t, later[0], later[1]) {
			if c.offset < notify.offset {
				t.Errorf("%s.%s runs before the lifecycle context exists (want NotifyContext at the top of run())", later[0], later[1])
			}
		}
	}
	onlyCallSite(t, "taskDispatcher", "SetBaseContext", "lifecycleCtx")
	run := onlyCallSite(t, "srv", "Run", "lifecycleCtx")

	identity := onlyCallSite(t, "haltExecutor", "SetWorkerIdentity", "")
	sweep := onlyCallSite(t, "haltExecutor", "KillOrphanedWorkersAtBoot", "context.Background()")
	if sweep.offset < identity.offset {
		t.Error("the orphan sweep runs before the halt executor's identity check is wired")
	}
	bootScan := -1
	for _, c := range mainCallSites(t, "", "runResumeScan") {
		if c.args == "context.Background()" {
			bootScan = c.offset
		}
	}
	if bootScan < 0 || sweep.offset > bootScan {
		t.Errorf("the boot orphan sweep (offset %d) must run before the startup resume scan (offset %d)", sweep.offset, bootScan)
	}
	if sweep.offset > run.offset {
		t.Error("the orphan sweep must run before srv.Run binds the socket")
	}
	stopAfterRun := false
	for _, c := range mainCallSites(t, "sched", "StopTicks") {
		if c.offset > run.offset {
			stopAfterRun = true
		}
	}
	if !stopAfterRun {
		t.Error("sched.StopTicks() must be called explicitly after srv.Run returns")
	}
}

// TestDispatcherTaskTimeoutWiredFromConfig (FIX-I-3): main.go must hand the
// outbox dispatcher the configured task_timeout_min, so a redispatched
// worker is bounded exactly like a first spawn.
func TestDispatcherTaskTimeoutWiredFromConfig(t *testing.T) {
	calls := mainCalls(t, "taskDispatcher", "SetTaskTimeout")
	if len(calls) != 1 {
		t.Fatalf("taskDispatcher.SetTaskTimeout calls = %d (%q), want exactly 1", len(calls), calls)
	}
	if want := "time.Duration(cfg.TaskTimeoutMin) * time.Minute"; calls[0] != want {
		t.Errorf("taskDispatcher.SetTaskTimeout(%s), want SetTaskTimeout(%s)", calls[0], want)
	}
}

// TestHaltWorkerIdentityWiredFromWorkerCmd (FIX-I-13): the halt executor
// must check a persisted pgid against the configured worker command before
// it ever SIGKILLs it.
func TestHaltWorkerIdentityWiredFromWorkerCmd(t *testing.T) {
	calls := mainCalls(t, "haltExecutor", "SetWorkerIdentity")
	if len(calls) != 1 || calls[0] != "spawn.NewWorkerIdentity(cfg.WorkerCmd)" {
		t.Fatalf("haltExecutor.SetWorkerIdentity calls = %q, want exactly [spawn.NewWorkerIdentity(cfg.WorkerCmd)]", calls)
	}
}

// TestRealToolsWiredWithReceipts (FIX-I-4): every real side-effectful tool
// must run its effect through the W4-02 receipt lifecycle in production -
// an unwired Receipts field silently falls back to the direct path, which
// re-executes a completed call on resume.
func TestRealToolsWiredWithReceipts(t *testing.T) {
	if calls := mainCalls(t, "srv", "SetReceipts"); len(calls) != 1 || calls[0] != "taskReceipts" {
		t.Fatalf("srv.SetReceipts calls = %q, want exactly [taskReceipts]", calls)
	}
	if calls := mainCalls(t, "taskReceipts", "SetJSONLLogger"); len(calls) != 1 {
		t.Fatalf("taskReceipts.SetJSONLLogger calls = %q, want exactly 1", calls)
	}
	if calls := mainCalls(t, "srv", "ReceiptExecutor"); len(calls) != 1 {
		t.Fatalf("srv.ReceiptExecutor calls = %q, want exactly 1 (recExec)", calls)
	}
	_, f, _ := mainSource(t)
	wired := map[string]int{}
	ast.Inspect(f, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		sel, ok := as.Lhs[0].(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Receipts" {
			return true
		}
		x, ok := sel.X.(*ast.Ident)
		rhs, rok := as.Rhs[0].(*ast.Ident)
		if ok && rok && rhs.Name == "recExec" {
			wired[x.Name]++
		}
		return true
	})
	for _, tool := range []string{"fsTool", "shellRunner", "hostExec", "osascriptRunner"} {
		if wired[tool] != 1 {
			t.Errorf("%s.Receipts = recExec assignments = %d, want exactly 1", tool, wired[tool])
		}
	}
}
