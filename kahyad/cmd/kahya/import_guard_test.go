package main

import (
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestCLIDoesNotImportDatabasePackages enforces the §4 locked decision /
// W78-01 acceptance criterion: the `kahya` CLI is a THIN UDS client and must
// NEVER open brain.db directly. It proves this by parsing the imports of
// every non-test .go file in this package and asserting none of the forbidden
// sqlite/store packages appear. kahyad is brain.db's sole reader/writer; the
// CLI reaches memory only over the UDS control socket.
func TestCLIDoesNotImportDatabasePackages(t *testing.T) {
	forbidden := map[string]string{
		"database/sql":                        "raw SQL access",
		"github.com/mattn/go-sqlite3":         "the sqlite driver",
		"kahya/kahyad/internal/store":         "the store package (opens brain.db)",
		"kahya/kahyad/internal/store/sqlcgen": "generated brain.db queries",
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	fset := token.NewFileSet()
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scanned++
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s: bad import literal %s", name, imp.Path.Value)
			}
			if why, bad := forbidden[path]; bad {
				t.Errorf("%s imports forbidden package %q (%s): the kahya CLI must reach brain.db only over the UDS, never directly", name, path, why)
			}
		}
	}
	if scanned == 0 {
		t.Fatal("scanned 0 non-test .go files - the guard would be vacuously green")
	}
}
