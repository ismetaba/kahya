package backup

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// gitignoreBackupsEntry is the single line task spec step 5 requires in
// <kahyaDir>/.gitignore: DB snapshots travel via Time Machine (step 3),
// markdown travels via git — pushing daily brain-*.db binaries through
// the private remote would bloat it for no reason.
const gitignoreBackupsEntry = "backups/"

// EnsureGitignoreEntry idempotently appends gitignoreBackupsEntry
// ("backups/") to <kahyaDir>/.gitignore, creating the file if it does not
// exist. It is a pure, side-effect-scoped helper — it never runs `git`
// and never commits anything (task spec step 5's own note: committing
// ~/Kahya's .gitignore change is a user/runtime action, not something
// this build task's code path performs automatically). Safe to call
// repeatedly: a line-exact match of gitignoreBackupsEntry already present
// anywhere in the file is left untouched (no duplicate line is ever
// appended).
func EnsureGitignoreEntry(kahyaDir string) error {
	path := filepath.Join(kahyaDir, ".gitignore")

	existing, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("backup: read %s: %w", path, err)
		}
		existing = nil
	}

	for _, line := range strings.Split(string(existing), "\n") {
		if strings.TrimSpace(line) == gitignoreBackupsEntry {
			return nil // already present — idempotent no-op
		}
	}

	content := string(existing)
	if len(content) > 0 && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += gitignoreBackupsEntry + "\n"

	if err := os.MkdirAll(kahyaDir, 0o700); err != nil {
		return fmt.Errorf("backup: create %s: %w", kahyaDir, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("backup: write %s: %w", path, err)
	}
	return nil
}
