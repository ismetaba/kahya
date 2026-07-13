package invariants

// coverage_map_test.go is the coverage-map honesty check (W78-03
// deliverable 3). It parses docs/coverage.md's authoritative
// invariant->test map and fails if any cited pkg/TestName no longer
// resolves to a real test function in the repo, or if any invariant lacks
// a CI-hermetic citation. It runs under plain `make test` AND is the first
// step of `make invariants`; renaming a cited test makes it go red.

import (
	"testing"
)

// coverageMapPath returns docs/coverage.md relative to the repo root,
// located by walking up from the test's working directory.
func coverageMapPath(t *testing.T) (root, path string) {
	t.Helper()
	root, err := RepoRoot("")
	if err != nil {
		t.Fatalf("RepoRoot: %v", err)
	}
	return root, root + "/docs/coverage.md"
}

// TestCoverageMapAllTestsExist parses every citation in docs/coverage.md's
// invariant->test map and asserts each pkg/TestName resolves to a real
// top-level test function. This is the "honest map" guard: a cited test
// that is renamed, moved, or deleted turns this red.
func TestCoverageMapAllTestsExist(t *testing.T) {
	root, path := coverageMapPath(t)
	cites, err := ParseCoverageMap(path)
	if err != nil {
		t.Fatalf("ParseCoverageMap: %v", err)
	}
	if len(cites) < 16 {
		t.Fatalf("parsed only %d citations; the map must cover preamble #0 + 6 safety + 5 memory + 4 product principles", len(cites))
	}
	for _, c := range cites {
		ok, err := TestFuncExists(root, c.Pkg, c.Test)
		if err != nil {
			t.Errorf("coverage.md:%d [%s] resolving %s/%s: %v", c.Line, c.Invariant, c.Pkg, c.Test, err)
			continue
		}
		if !ok {
			t.Errorf("coverage.md:%d [%s] cites %s/%s which does NOT exist in the repo (renamed/deleted?)", c.Line, c.Invariant, c.Pkg, c.Test)
		}
	}
}

// TestCoverageMapEveryInvariantHasCIHermetic asserts every invariant id in
// the map has at least one CI-hermetic citation, so `make invariants` can
// actually run a real assertion for it in CI (Docker/MLX/Keychain absent).
func TestCoverageMapEveryInvariantHasCIHermetic(t *testing.T) {
	_, path := coverageMapPath(t)
	cites, err := ParseCoverageMap(path)
	if err != nil {
		t.Fatalf("ParseCoverageMap: %v", err)
	}
	// Single-sourced with the `make invariants` runner (parse.go), so the
	// in-suite guard and the standalone runner enforce the identical contract:
	// every required invariant present AND each with a ci-hermetic citation.
	for _, e := range ClassCoverageErrors(cites) {
		t.Error(e)
	}
}
