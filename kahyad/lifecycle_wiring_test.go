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

// mainCalls returns every call in main.go whose callee reads recv.method,
// with the source text of its arguments.
func mainCalls(t *testing.T, recv, method string) []string {
	t.Helper()
	fset, f, src := mainSource(t)
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := c.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != method {
			return true
		}
		if x, ok := sel.X.(*ast.Ident); !ok || x.Name != recv {
			return true
		}
		var args []string
		for _, a := range c.Args {
			args = append(args, string(src[fset.Position(a.Pos()).Offset:fset.Position(a.End()).Offset]))
		}
		out = append(out, strings.Join(args, ", "))
		return true
	})
	return out
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
