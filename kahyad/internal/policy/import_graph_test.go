package policy

import (
	"os/exec"
	"strings"
	"testing"
)

// forbiddenPolicyImports is the HANDOFF S5 product principle, made
// mechanical: "Hafiza bir izni asla dusuremez" (memory never lowers a
// permission). kahyad/internal/policy's decision inputs must be limited
// to policy registration + autonomy_state + class/scope/taint flags -
// NEVER anything derived from the memory/search/index pipeline (facts,
// embeddings, <hafiza> injection content). This test asserts that
// mechanically, on the package's FULL transitive import graph (`go list
// -deps`), not just its direct imports - a future indirect dependency
// (e.g. a helper package that itself imports kahyad/internal/search)
// would be just as much a violation as importing it directly, and a code
// reviewer skimming a diff is exactly the failure mode this check exists
// to close off.
var forbiddenPolicyImports = []string{
	"kahya/kahyad/internal/search",
	"kahya/kahyad/internal/indexer",
	"kahya/kahyad/internal/embed",
	"kahya/mcp/memory",
}

// TestPolicyPackageDoesNotImportMemoryDerivedPackages runs `go list -deps`
// against this package's own import path and fails if the transitive
// closure contains any forbidden package. `go list` (part of the standard
// go toolchain, no extra dependency needed) resolves purely from the
// local module - no network access required.
func TestPolicyPackageDoesNotImportMemoryDerivedPackages(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "-tags", "sqlite_fts5", "kahya/kahyad/internal/policy")
	cmd.Dir = repoRoot(t)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("go list -deps failed: %v\nstderr=%s", err, ee.Stderr)
		}
		t.Fatalf("go list -deps failed: %v", err)
	}

	deps := make(map[string]bool)
	for _, line := range strings.Split(string(out), "\n") {
		deps[strings.TrimSpace(line)] = true
	}

	for _, forbidden := range forbiddenPolicyImports {
		if deps[forbidden] {
			t.Errorf("kahyad/internal/policy transitively imports forbidden package %q (HANDOFF S5: memory must never reach a policy decision)", forbidden)
		}
	}
}
