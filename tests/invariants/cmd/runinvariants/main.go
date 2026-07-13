// Command runinvariants backs `make invariants`. It parses the
// authoritative §5 invariant->test map in docs/coverage.md and:
//
//  1. HONESTY CHECK: verifies every cited pkg/TestName (ci-hermetic AND
//     local-integration) resolves to a real test function in the repo.
//     A dangling citation aborts with exit 1 before any test runs - this
//     is what keeps docs/coverage.md from rotting into a set of names that
//     no longer exist.
//  2. RUN: executes exactly the CI-hermetic cited tests, grouped by
//     package, via `go test -run '^(TestA|TestB|...)$'`. local-integration
//     citations are existence-checked only (they need Docker/MLX/Keychain/
//     real bins, which CI lacks) and are run by the full local `make test`.
//
// Exit codes: 0 all good; 1 a dangling citation or a failing hermetic test;
// 2 a usage/parse error.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"kahya/tests/invariants"
)

// goBuildTags mirrors the Makefile's GOTAGS - every Go build/test in this
// repo needs sqlite_fts5 (FTS5 virtual tables in migration 0002).
const goBuildTags = "sqlite_fts5"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "make invariants:", err)
		os.Exit(1)
	}
}

func run() error {
	root, err := invariants.RepoRoot("")
	if err != nil {
		return err
	}
	cites, err := invariants.ParseCoverageMap(root + "/docs/coverage.md")
	if err != nil {
		return err
	}

	// (1) Honesty check over ALL citations.
	var dangling []string
	for _, c := range cites {
		ok, existErr := invariants.TestFuncExists(root, c.Pkg, c.Test)
		if existErr != nil {
			dangling = append(dangling, fmt.Sprintf("  coverage.md:%d [%s] %s/%s: %v", c.Line, c.Invariant, c.Pkg, c.Test, existErr))
			continue
		}
		if !ok {
			dangling = append(dangling, fmt.Sprintf("  coverage.md:%d [%s] cites %s/%s which does not exist (renamed/deleted?)", c.Line, c.Invariant, c.Pkg, c.Test))
		}
	}
	if len(dangling) > 0 {
		return fmt.Errorf("coverage-map honesty check FAILED - dangling citations:\n%s", strings.Join(dangling, "\n"))
	}
	fmt.Printf("coverage-map honesty check OK: all %d cited tests exist\n", len(cites))

	// (2) Class coverage: every required invariant present AND every present
	// invariant has a ci-hermetic citation. Enforced HERE (not only in the
	// tests/invariants suite under `make test`) so `make invariants` is
	// self-contained and cannot go green while an invariant lost its only
	// hermetic test.
	if errs := invariants.ClassCoverageErrors(cites); len(errs) > 0 {
		return fmt.Errorf("coverage-map class coverage FAILED:\n  %s", strings.Join(errs, "\n  "))
	}

	// (3) Run the CI-hermetic cited tests, grouped by package, and VERIFY each
	// cited test actually RAN and PASSED. Existence (parsed AST) is not enough:
	// a citation classed ci-hermetic but located behind a non-default build tag
	// (e2e/acceptance) would compile out under -tags sqlite_fts5, and a bare
	// `go test -run` exits 0 with "no tests to run" - a silent vacuous green.
	// Parsing -json and asserting a "pass" event per cited name closes that.
	byPkg := map[string]map[string]bool{}
	for _, c := range cites {
		if c.Class != invariants.ClassCIHermetic {
			continue
		}
		if byPkg[c.Pkg] == nil {
			byPkg[c.Pkg] = map[string]bool{}
		}
		byPkg[c.Pkg][c.Test] = true
	}
	if len(byPkg) == 0 {
		return fmt.Errorf("no ci-hermetic invariant tests to run - the map must cite at least one runnable test")
	}
	pkgs := make([]string, 0, len(byPkg))
	for p := range byPkg {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)

	total := 0
	for _, pkg := range pkgs {
		names := make([]string, 0, len(byPkg[pkg]))
		for n := range byPkg[pkg] {
			names = append(names, n)
		}
		sort.Strings(names)
		runExpr := "^(" + strings.Join(names, "|") + ")$"
		fmt.Printf("\n== invariant tests: %s (%d) ==\n", pkg, len(names))
		passed, err := runAndCollect(root, pkg, runExpr)
		if err != nil {
			return fmt.Errorf("hermetic invariant tests failed in %s: %w", pkg, err)
		}
		// Every cited hermetic test must have actually executed and passed -
		// not skipped, not compiled-out, not "no tests to run".
		var missing []string
		for _, n := range names {
			if !passed[n] {
				missing = append(missing, n)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("in %s, ci-hermetic citation(s) did NOT run+pass under -tags %s (mis-classified / behind a build tag / skipped?): %s",
				pkg, goBuildTags, strings.Join(missing, ", "))
		}
		total += len(names)
	}
	fmt.Printf("\nall %d CI-hermetic invariant tests ran and passed across %d packages\n", total, len(pkgs))
	return nil
}

// runAndCollect runs `go test -json` for runExpr in pkg and returns the set of
// top-level test names that emitted a "pass" action. It streams the raw JSON
// events to stdout (as readable lines) so a failure is still legible, and
// returns an error if the overall `go test` failed.
func runAndCollect(root, pkg, runExpr string) (map[string]bool, error) {
	cmd := exec.Command("go", "test", "-tags", goBuildTags, "-count=1", "-run", runExpr, "-json", "./"+pkg)
	cmd.Dir = root
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr
	runErr := cmd.Run()

	passed := map[string]bool{}
	sc := bufio.NewScanner(&out)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var ev struct {
			Action string `json:"Action"`
			Test   string `json:"Test"`
			Output string `json:"Output"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		// Echo human-readable test output so a failing run is still legible.
		if ev.Action == "output" && ev.Output != "" {
			fmt.Print(ev.Output)
		}
		// Only top-level test names (no "/" subtest suffix) count as coverage.
		if ev.Action == "pass" && ev.Test != "" && !strings.Contains(ev.Test, "/") {
			passed[ev.Test] = true
		}
	}
	return passed, runErr
}
