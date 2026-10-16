package invariants

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// taskLabelFilter is the `docker ps --filter` value that matches Kahya
// sandbox containers by their kahya.task_id label.
const taskLabelFilter = "label=kahya.task_id"

// TestNoUnscopedSandboxContainerSweep (FIX-FINAL ops-config-r2-nonprod-
// shutdown-kills-every-sandbox-container): every daemon - prod, dev, the
// restore drill's scratch, each hermetic harness - shares one Docker
// runtime, and a restore copy even shares prod's task ids. A kahya.task_id
// filter alone matched every Kahya container on the machine, so any
// non-prod daemon's shutdown (or halt) killed prod's running shell_docker
// jobs. The only production code allowed to spell that filter is the
// argument of mcp/shell's ownedContainerListArgs, which ANDs it with the
// daemon's own kahya.owner label; no script, Makefile or other Go file may
// filter (and so kill) by the task label alone.
func TestNoUnscopedSandboxContainerSweep(t *testing.T) {
	root, err := RepoRoot("")
	if err != nil {
		t.Fatalf("RepoRoot: %v", err)
	}
	var helperCalls int
	check := func(p string) error {
		rel, _ := filepath.Rel(root, p)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if !strings.Contains(string(b), taskLabelFilter) {
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			t.Errorf("%s filters containers by %q (an unscoped sweep would match every daemon's containers)", rel, taskLabelFilter)
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, p, b, 0)
		if err != nil {
			t.Errorf("parse %s: %v", rel, err)
			return nil
		}
		allowed := map[token.Pos]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "ownedContainerListArgs" && len(c.Args) == 2 {
				helperCalls++
				ast.Inspect(c.Args[1], func(m ast.Node) bool {
					if lit, ok := m.(*ast.BasicLit); ok {
						allowed[lit.Pos()] = true
					}
					return true
				})
			}
			return true
		})
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v, err := strconv.Unquote(lit.Value)
			if err != nil || !strings.Contains(v, taskLabelFilter) {
				return true
			}
			if !allowed[lit.Pos()] || rel != filepath.Join("mcp", "shell", "runner.go") {
				t.Errorf("%s:%d spells %q outside mcp/shell's ownedContainerListArgs (an unscoped container sweep)", rel, fset.Position(lit.Pos()).Line, v)
			}
			return true
		})
		return nil
	}
	for _, top := range []string{"kahyad", "mcp", "scripts", "worker", "dev", "docker", "eval", "hammerspoon", "Makefile"} {
		base := filepath.Join(root, top)
		info, err := os.Stat(base)
		if err != nil {
			continue
		}
		if !info.IsDir() {
			if err := check(base); err != nil {
				t.Fatal(err)
			}
			continue
		}
		err = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if name := d.Name(); strings.HasPrefix(name, ".") || name == "node_modules" || name == "__pycache__" {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(p, "_test.go") {
				return nil
			}
			switch filepath.Ext(p) {
			case ".go", ".sh", ".py", ".lua", ".mk":
				return check(p)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", top, err)
		}
	}
	if helperCalls < 2 {
		t.Errorf("ownedContainerListArgs call sites = %d, want the shutdown sweep and the halt kill (the scan is stale)", helperCalls)
	}
}
