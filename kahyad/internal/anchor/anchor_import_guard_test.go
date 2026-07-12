package anchor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// guardedSymbol is the anchor deploy-key accessor's own symbol name
// (keychain.go's AnchorDeployKey) - the permanent §5-#4 Keychain-isolation
// guard (HANDOFF §5 safety #4 ⚑: "Bu kimlik Keychain'de ayrı öğedir, yalnız
// çapa-yazma kod yolunda okunur"). This test walks every .go source file in
// the repo and fails if this literal string ever appears outside this
// package's own directory - i.e. the anchor deploy key is read ONLY from
// kahyad/internal/anchor, forever.
const guardedSymbol = "AnchorDeployKey"

// repoRoot resolves the repository root from this test file's own path
// (kahyad/internal/anchor/anchor_import_guard_test.go is always three
// directories below the root), rather than assuming a particular working
// directory - `go test ./...` from the repo root and `go test .` from
// inside this package must both find the same files.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	// kahyad/internal/anchor -> repo root is three levels up.
	return filepath.Clean(filepath.Join(wd, "..", "..", ".."))
}

// TestAnchorDeployKeyReferencedOnlyFromThisPackage is the guard test
// itself (task spec step 4 / deliverable
// anchor_import_guard_test.go): every .go file anywhere in the repo that
// mentions "AnchorDeployKey" must live under kahyad/internal/anchor.
func TestAnchorDeployKeyReferencedOnlyFromThisPackage(t *testing.T) {
	root := repoRoot(t)
	anchorDir := filepath.Join(root, "kahyad", "internal", "anchor")

	var offenders []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip VCS/build/vendor-ish directories that are never part of
			// this codebase's own Go source tree, and any git worktree
			// checkouts under .claude/worktrees (stale copies of this same
			// repo, not the tree this test is guarding).
			switch d.Name() {
			case ".git", "node_modules", "worktrees":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !strings.Contains(string(b), guardedSymbol) {
			return nil
		}
		rel, relErr := filepath.Rel(anchorDir, path)
		if relErr != nil || strings.HasPrefix(rel, "..") {
			offenders = append(offenders, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}

	if len(offenders) != 0 {
		t.Errorf("%q referenced outside kahyad/internal/anchor (HANDOFF §5 safety #4 Keychain isolation): %v", guardedSymbol, offenders)
	}
}
