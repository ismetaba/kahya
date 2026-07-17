// Package invariants parses the authoritative §5 invariant -> test map in
// docs/coverage.md (the "## §5 invariant -> test map (W78-03)" section)
// and resolves every cited pkg/TestName against the real repository. It
// backs both the honesty-check test (TestCoverageMapAllTestsExist) and
// the `make invariants` runner (tests/invariants/cmd/runinvariants), so
// there is exactly one parser and one source of truth for the map.
//
// A citation is written in docs/coverage.md as a table cell of the form
//
//	<repo-relative-package-dir>/<TestFuncName>
//
// e.g. `kahyad/internal/policy/TestDBErrorPathDeniesFailClosed`, in a row
// whose class cell is either `ci-hermetic` or `local-integration`.
package invariants

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// SectionHeading marks the start of the authoritative invariant->test
// table; parsing stops at the next level-2 heading.
const SectionHeading = "## §5 invariant -> test map (W78-03)"

// ClassCIHermetic tests run in CI (no Docker/MLX/Keychain/network);
// ClassLocalIntegration tests are existence-checked but only executed in
// the full local `make test` / `make readiness`.
const (
	ClassCIHermetic       = "ci-hermetic"
	ClassLocalIntegration = "local-integration"
)

// citationRE matches a `<dir>/<TestName>` cell. The package dir is a
// slash path of ident-ish segments; the final segment is a Go test
// function name (Test, Benchmark, Example, or Fuzz-prefixed - only Test is
// used today, but the pattern is permissive on the prefix and strict on
// the shape).
var citationRE = regexp.MustCompile(`^([A-Za-z0-9_./-]+)/((?:Test|Benchmark|Example|Fuzz)[A-Za-z0-9_]*)$`)

// RequiredInvariants is the full §5 set every coverage map must cover: the
// enforcement preamble (#0), six safety invariants, five memory invariants,
// and four product principles. Single-sourced here so the honesty test and
// the `make invariants` runner agree on what "complete" means.
var RequiredInvariants = []string{
	"#0",
	"Safety #1", "Safety #2", "Safety #3", "Safety #4", "Safety #5", "Safety #6",
	"Memory #1", "Memory #2", "Memory #3", "Memory #4", "Memory #5",
	"Product A", "Product B", "Product C", "Product D",
}

// ClassCoverageErrors reports every way the citation set fails the coverage
// contract: a required invariant absent from the map, or a present invariant
// with no CI-hermetic citation (so `make invariants` could not run a real
// assertion for it in CI). Empty slice = fully covered.
func ClassCoverageErrors(cites []Citation) []string {
	seen := map[string]bool{}
	hermetic := map[string]bool{}
	for _, c := range cites {
		seen[c.Invariant] = true
		if c.Class == ClassCIHermetic {
			hermetic[c.Invariant] = true
		}
	}
	var errs []string
	for _, inv := range RequiredInvariants {
		if !seen[inv] {
			errs = append(errs, fmt.Sprintf("required invariant %q is absent from the map", inv))
		}
	}
	for inv := range seen {
		if !hermetic[inv] {
			errs = append(errs, fmt.Sprintf("invariant %q has no ci-hermetic citation (every invariant must be runnable in CI)", inv))
		}
	}
	return errs
}

// Citation is one cited test in the invariant->test map.
type Citation struct {
	Invariant string // the invariant id cell (e.g. "#0", "Safety #1")
	Pkg       string // repo-relative package dir, e.g. "kahyad/internal/policy"
	Test      string // Go test function name, e.g. "TestFoo"
	Task      string // owning task cell
	Class     string // ClassCIHermetic | ClassLocalIntegration
	Line      int    // 1-based line number in docs/coverage.md
}

// RepoRoot walks up from start (or the cwd if start is empty) until it
// finds a directory containing go.mod.
func RepoRoot(start string) (string, error) {
	dir := start
	if dir == "" {
		var err error
		if dir, err = os.Getwd(); err != nil {
			return "", err
		}
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("invariants: go.mod not found above %q", start)
		}
		dir = parent
	}
}

// ParseCoverageMap reads docs/coverage.md and returns every citation in
// the authoritative invariant->test map section. It errors if the section
// is missing or if a row inside it has a malformed class cell, so the map
// cannot silently degrade into an unparsed blob.
func ParseCoverageMap(path string) ([]Citation, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var (
		cites     []Citation
		inSection bool
		sawHead   bool
		lineNo    int
	)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## ") {
			if trimmed == SectionHeading {
				inSection = true
				sawHead = true
				continue
			}
			if inSection {
				break // next level-2 heading ends the section
			}
			continue
		}
		if !inSection || !strings.HasPrefix(trimmed, "|") {
			continue
		}
		cells := splitRow(trimmed)
		if len(cells) < 4 {
			continue
		}
		// Find the citation cell and the class cell anywhere in the row so
		// the exact column order can evolve without breaking parsing.
		var (
			c   Citation
			ok  bool
			cls string
		)
		for _, cell := range cells {
			if m := citationRE.FindStringSubmatch(cell); m != nil {
				c.Pkg, c.Test, ok = m[1], m[2], true
			}
			switch cell {
			case ClassCIHermetic, ClassLocalIntegration:
				cls = cell
			}
		}
		if !ok {
			continue // header/separator/prose row inside the section
		}
		if cls == "" {
			return nil, fmt.Errorf("docs/coverage.md:%d: citation %q has no ci-hermetic/local-integration class cell", lineNo, cells[0])
		}
		c.Invariant = strings.TrimSpace(cells[0])
		c.Class = cls
		c.Line = lineNo
		// Task cell: the last non-empty, non-class, non-citation cell.
		for _, cell := range cells {
			if cell == cls || citationRE.MatchString(cell) || cell == "" {
				continue
			}
			c.Task = cell
		}
		cites = append(cites, c)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if !sawHead {
		return nil, fmt.Errorf("docs/coverage.md: authoritative section %q not found", SectionHeading)
	}
	if len(cites) == 0 {
		return nil, fmt.Errorf("docs/coverage.md: no citations parsed under %q", SectionHeading)
	}
	return cites, nil
}

func splitRow(row string) []string {
	row = strings.Trim(row, "|")
	parts := strings.Split(row, "|")
	out := make([]string, len(parts))
	for i, p := range parts {
		// Strip markdown code ticks so `code`-wrapped citations resolve.
		out[i] = strings.TrimSpace(strings.ReplaceAll(p, "`", ""))
	}
	return out
}

// coveredFilePathRE matches a backtick-quoted, repo-relative file path: a
// slash-containing path ending in a known committed-file extension, with no
// whitespace, glob (`*`), or leading `/`/`~`. It is deliberately narrow so
// backtick-wrapped tokens that are NOT committed files are never mistaken for
// deliverables: tool names (`memory_search`) and columns (`source_tier`) have
// no slash; pkg/TestName citations (`kahyad/internal/policy/TestFoo`) have a
// slash but no file extension; globs (`*.md`) and home-relative paths
// (`~/.zshrc`) are excluded by the character class and leading-char rule.
var coveredFilePathRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*(?:/[A-Za-z0-9_.-]+)+\.(?:md|html|txt|go|sql|ya?ml|json|sh|plist|py|toml)$`)

// backtickTokenRE extracts each backtick-delimited span from a raw line
// (splitRow strips backticks, losing the token boundaries, so committed-file
// scanning re-reads them from the untouched line text).
var backtickTokenRE = regexp.MustCompile("`([^`]+)`")

// CoveredFileDeliverable is one committed-file claim in a coverage-map row
// whose status cell is exactly "covered": a repo-relative path the row
// asserts is committed. TestCoverageMapCommittedFilesExist os.Stats each one
// so a "covered" status can never outrun the file it claims.
type CoveredFileDeliverable struct {
	Path string // repo-relative file path, e.g. "docs/ipc.md"
	Line int    // 1-based line number in docs/coverage.md
}

// ParseCoveredFileDeliverables scans docs/coverage.md for table rows whose
// status cell is exactly "covered" and returns every backtick-quoted
// repo-relative file path (coveredFilePathRE) cited in such a row. Rows with
// any other status — "partial", "GAP …", or a §5-map class cell
// (ci-hermetic/local-integration, which never equals "covered") — are
// ignored, so a legitimately-uncommitted deliverable simply must not be
// marked "covered". This is the file analogue of ParseCoverageMap's cited-
// test honesty guard.
func ParseCoveredFileDeliverables(path string) ([]CoveredFileDeliverable, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []CoveredFileDeliverable
	lineNo := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		lineNo++
		raw := sc.Text()
		trimmed := strings.TrimSpace(raw)
		if !strings.HasPrefix(trimmed, "|") {
			continue
		}
		covered := false
		for _, cell := range splitRow(trimmed) {
			if cell == "covered" {
				covered = true
				break
			}
		}
		if !covered {
			continue
		}
		for _, m := range backtickTokenRE.FindAllStringSubmatch(raw, -1) {
			tok := strings.TrimSpace(m[1])
			if coveredFilePathRE.MatchString(tok) {
				out = append(out, CoveredFileDeliverable{Path: tok, Line: lineNo})
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// TestFuncExists reports whether pkgDir (repo-relative) contains a
// *_test.go file declaring a top-level func named testName. It parses
// source with go/parser and therefore sees test functions regardless of
// their build constraints (e2e/acceptance/invariants tags), so a
// local-integration test that CI never compiles is still checkable.
func TestFuncExists(repoRoot, pkgDir, testName string) (bool, error) {
	absDir := filepath.Join(repoRoot, filepath.FromSlash(pkgDir))
	entries, err := os.ReadDir(absDir)
	if err != nil {
		return false, fmt.Errorf("package dir %q: %w", pkgDir, err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, filepath.Join(absDir, e.Name()), nil, 0)
		if perr != nil {
			return false, fmt.Errorf("parse %s/%s: %w", pkgDir, e.Name(), perr)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil {
				continue // only top-level (non-method) funcs are test funcs
			}
			if fn.Name.Name == testName {
				return true, nil
			}
		}
	}
	return false, nil
}
