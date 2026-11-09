package invariants

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// docSplit is one finding of splitDocComments: Decl's doc comment opens
// with Victim, another declaration of the same file that has no doc
// comment of its own.
type docSplit struct {
	Line   int
	Decl   string
	Victim string
}

// splitDocComments reports the "split doc comment" pattern in one parsed
// file: a declaration was inserted between another declaration and that
// one's doc comment, so the comment now heads the inserted declaration
// (godoc shows "isTerminalStatus reports ..." on a const) and its real
// owner has none. A doc comment is reported when its first word is not a
// name its own declaration declares, IS a top-level name the same file
// declares, and that name has a declaration with no doc comment. The last
// condition keeps ordinary prose that happens to open with a documented
// identifier ("Event kinds ...", above a const block next to a documented
// type Event) out of the report.
func splitDocComments(fset *token.FileSet, f *ast.File) []docSplit {
	// undocumented[name] is true when some top-level declaration of name
	// carries no doc comment; declared holds every top-level name.
	declared := map[string]bool{}
	undocumented := map[string]bool{}
	note := func(name string, documented bool) {
		if name == "_" {
			return
		}
		declared[name] = true
		if !documented {
			undocumented[name] = true
		}
	}
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			note(d.Name.Name, d.Doc != nil)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					note(s.Name.Name, d.Doc != nil || s.Doc != nil || s.Comment != nil)
				case *ast.ValueSpec:
					for _, n := range s.Names {
						note(n.Name, d.Doc != nil || s.Doc != nil || s.Comment != nil)
					}
				}
			}
		}
	}

	var out []docSplit
	check := func(doc *ast.CommentGroup, own []string) {
		if doc == nil || len(own) == 0 {
			return
		}
		fields := strings.Fields(doc.Text())
		if len(fields) == 0 {
			return
		}
		first := strings.TrimRight(fields[0], ".,:;")
		first = strings.TrimSuffix(first, "'s")
		for _, name := range own {
			if name == first {
				return
			}
		}
		if declared[first] && undocumented[first] {
			out = append(out, docSplit{Line: fset.Position(doc.Pos()).Line, Decl: own[0], Victim: first})
		}
	}
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			check(d.Doc, []string{d.Name.Name})
		case *ast.GenDecl:
			var all []string
			for _, spec := range d.Specs {
				var names []string
				var doc *ast.CommentGroup
				switch s := spec.(type) {
				case *ast.TypeSpec:
					names, doc = []string{s.Name.Name}, s.Doc
				case *ast.ValueSpec:
					for _, n := range s.Names {
						names = append(names, n.Name)
					}
					doc = s.Doc
				}
				check(doc, names)
				all = append(all, names...)
			}
			check(d.Doc, all)
		}
	}
	return out
}

// TestDocCommentsSitOnTheirOwnDeclaration (FIX-FINAL doc-comment-split-
// reintroduced): no file under kahyad/ or mcp/ carries a split doc comment
// (splitDocComments). The pattern was fixed once (f39cc17) and came back
// twice in the same batch - a const dropped between isTerminalStatus and
// its comment in halt/executor.go, a const and a var between
// Client.ApprovalDecision and its comment in cmd/kahya/client.go - because
// nothing but a reader notices it.
func TestDocCommentsSitOnTheirOwnDeclaration(t *testing.T) {
	root, err := RepoRoot("")
	if err != nil {
		t.Fatalf("RepoRoot: %v", err)
	}
	files := 0
	for _, dir := range []string{"kahyad", "mcp"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if name := d.Name(); name == "sqlcgen" || name == "testdata" || strings.HasPrefix(name, ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(p, ".go") {
				return nil
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, p, nil, parser.ParseComments)
			if err != nil {
				return err
			}
			files++
			rel, _ := filepath.Rel(root, p)
			for _, s := range splitDocComments(fset, f) {
				t.Errorf("%s:%d: the doc comment above %s opens with %q, which this file declares without a doc comment: move %s (and its own comment) above that comment",
					rel, s.Line, s.Decl, s.Victim, s.Decl)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	if files < 100 {
		t.Fatalf("checked only %d Go files under kahyad/ and mcp/: the walk is not reaching the tree", files)
	}
}

// TestSplitDocCommentsFindsThePattern pins splitDocComments itself on the
// two shapes that reached the tree (a const, and a const plus a var,
// inserted under a function's doc comment) and on the look-alikes it must
// leave alone.
func TestSplitDocCommentsFindsThePattern(t *testing.T) {
	const src = `package p

// isTerminal reports whether s is terminal.
// attempts bounds the retries.
const attempts = 8

func isTerminal(s string) bool { return s == "done" }

// Decide calls the decision route.
// codeTyped mirrors the server's refusal code.
const codeTyped = "typed"

// errTyped is Decide's sentinel.
var errTyped = 1

func (c *client) Decide() {}

type client struct{}

// Event is one ledger row.
type Event struct{}

// Event kinds the ledger accepts.
const (
	kindA = "a"
	kindB = "b"
)

// helper's doc opens with its own name.
func helper() {}

// run starts the loop; see helper.
func run() {}

// Not an identifier of this file at all.
func other() {}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range splitDocComments(fset, f) {
		got = append(got, s.Decl+"<-"+s.Victim)
	}
	sort.Strings(got)
	want := []string{"attempts<-isTerminal", "codeTyped<-Decide"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("splitDocComments = %v, want %v", got, want)
	}
}
