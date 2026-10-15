package consolidation

import (
	"context"
	"fmt"

	"kahya/kahyad/internal/backup"
)

// CommitAll is a TEST FIXTURE helper: it stages the whole tree (`git add
// -A`) and commits it as author. Production code never stages the whole
// ~/Kahya tree (FIX-P-1: Run stages only the memory directory through
// StagePaths/CommitPaths; TestProductionCodeNeverStagesWholeTree pins it),
// so this lives in a _test.go file. Tests use it to simulate a same-day
// user commit made before the nightly run.
func CommitAll(ctx context.Context, git backup.GitRunner, repoDir, author, message string) error {
	if _, stderr, err := git.Run(ctx, repoDir, "add", "-A"); err != nil {
		return fmt.Errorf("fixture: git add -A: %w (%s)", err, stderr)
	}
	name, email := parseAuthor(author)
	args := []string{"-c", "user.name=" + name, "-c", "user.email=" + email, "commit", "--author=" + author, "-m", message}
	if _, stderr, err := git.Run(ctx, repoDir, args...); err != nil {
		return fmt.Errorf("fixture: git commit --author=%q: %w (%s)", author, err, stderr)
	}
	return nil
}
