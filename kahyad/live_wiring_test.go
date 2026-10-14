package main

// live_wiring_test.go: FIX-I-14 review. A local answer (server, via
// SetTaskLiveRegistry) and a briefing run (briefing.Orchestrator.Live)
// mark their 'executing' task live in task.LiveRegistry.BeginInProcess;
// that only protects them if the resume scan and the outbox dispatcher
// consult the SAME registry. Before the fix the briefing registered
// nowhere, so the 30 s resume scan failed a still-running briefing. This
// test pins main.go's wiring of the one registry into all four places.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestLiveRegistryWiredIntoScanDispatcherServerAndBriefing(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	isIdent := func(e ast.Expr, name string) bool {
		id, ok := e.(*ast.Ident)
		return ok && id.Name == name
	}
	callName := func(c *ast.CallExpr) string {
		if sel, ok := c.Fun.(*ast.SelectorExpr); ok {
			if x, ok := sel.X.(*ast.Ident); ok {
				return x.Name + "." + sel.Sel.Name
			}
		}
		return ""
	}

	var defined, serverWired, scanWired, dispatcherWired, briefingWired bool
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			if len(n.Lhs) == 1 && isIdent(n.Lhs[0], "taskLive") && len(n.Rhs) == 1 {
				if c, ok := n.Rhs[0].(*ast.CallExpr); ok && callName(c) == "task.NewLiveRegistry" {
					defined = true
				}
			}
		case *ast.CallExpr:
			switch callName(n) {
			case "srv.SetTaskLiveRegistry":
				serverWired = len(n.Args) == 1 && isIdent(n.Args[0], "taskLive")
			case "task.NewResume":
				if len(n.Args) >= 6 {
					if c, ok := n.Args[5].(*ast.CallExpr); ok && callName(c) == "task.AnyLive" {
						for _, a := range c.Args {
							if isIdent(a, "taskLive") {
								scanWired = true
							}
						}
					}
				}
			case "outbox.NewDispatcher":
				dispatcherWired = len(n.Args) == 5 && isIdent(n.Args[4], "taskLive")
			}
		case *ast.CompositeLit:
			if sel, ok := n.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "Orchestrator" {
				if x, ok := sel.X.(*ast.Ident); ok && x.Name == "briefing" {
					for _, el := range n.Elts {
						if kv, ok := el.(*ast.KeyValueExpr); ok && isIdent(kv.Key, "Live") && isIdent(kv.Value, "taskLive") {
							briefingWired = true
						}
					}
				}
			}
		}
		return true
	})

	for name, ok := range map[string]bool{
		"taskLive := task.NewLiveRegistry()":                    defined,
		"srv.SetTaskLiveRegistry(taskLive)":                     serverWired,
		"task.NewResume(..., task.AnyLive(taskLive, ...), ...)": scanWired,
		"outbox.NewDispatcher(..., taskLive)":                   dispatcherWired,
		"&briefing.Orchestrator{..., Live: taskLive, ...}":      briefingWired,
	} {
		if !ok {
			t.Errorf("main.go does not wire %s", name)
		}
	}
}
