package backup

import (
	"errors"
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

// ErrBackupDirIsRepoRoot is returned by EnsureBackupDirIgnored when
// backup_dir resolves to kahya_dir itself: ignoring it would ignore the
// whole memory repo, and not ignoring it lets every brain-*.db snapshot
// into the next commit. Either way the configuration is unusable.
var ErrBackupDirIsRepoRoot = errors.New("backup: backup_dir must not equal kahya_dir")

// EnsureGitignoreEntry idempotently appends gitignoreBackupsEntry
// ("backups/") to <kahyaDir>/.gitignore, creating the file if it does not
// exist. It is a pure, side-effect-scoped helper — it never runs `git`
// and never commits anything (task spec step 5's own note: committing
// ~/Kahya's .gitignore change is a user/runtime action, not something
// this build task's code path performs automatically). Safe to call
// repeatedly: a line-exact match of gitignoreBackupsEntry already present
// anywhere in the file is left untouched (no duplicate line is ever
// appended). kahyad itself calls EnsureBackupDirIgnored (FIX-P-1), which
// writes the anchored form for the configured backup_dir.
func EnsureGitignoreEntry(kahyaDir string) error {
	if err := os.MkdirAll(kahyaDir, 0o700); err != nil {
		return fmt.Errorf("backup: create %s: %w", kahyaDir, err)
	}
	return EnsureGitignoreLine(kahyaDir, gitignoreBackupsEntry)
}

// EnsureBackupDirIgnored (FIX-P-1) idempotently adds an ANCHORED gitignore
// entry "/<rel>/" (rel = backupDir relative to kahyaDir, forward slashes) to
// <kahyaDir>/.gitignore, so a brain-*.db snapshot - or a crashed run's
// brain-*.db.staging file, which lives in the same directory - can never be
// staged from the memory repo. It never runs git and never commits.
//
// It returns the entry that covers backupDir. The entry is ALWAYS non-empty
// when backupDir lies inside an existing kahyaDir, also when the line was
// already present, because callers use it as the staged-path guard prefix.
// It returns ("", nil) ONLY when kahyaDir does not exist yet (the HANDOFF §7
// kickoff creates it; nothing is created here) or when backupDir lies
// outside kahyaDir (nothing in the memory repo can ever stage it). Symlinked
// spellings are resolved before that decision, so a backupDir that reaches
// kahyaDir through a symlink is still covered. An existing unanchored
// "<rel>/" line (W4-06's original spelling) already satisfies the check and
// is left as is. Anchored, because an unanchored "backups/" would also
// ignore a user note folder such as memory/backups/.
func EnsureBackupDirIgnored(kahyaDir, backupDir string) (entry string, err error) {
	if kahyaDir == "" || backupDir == "" {
		return "", nil
	}
	if _, statErr := os.Stat(kahyaDir); statErr != nil {
		if os.IsNotExist(statErr) {
			return "", nil
		}
		return "", fmt.Errorf("backup: stat %s: %w", kahyaDir, statErr)
	}
	rel, inside := relInside(kahyaDir, backupDir)
	if !inside {
		rk, rkErr := filepath.EvalSymlinks(kahyaDir)
		if rkErr != nil {
			return "", fmt.Errorf("backup: resolve %s: %w", kahyaDir, rkErr)
		}
		rel, inside = relInside(rk, resolveExistingPrefix(backupDir))
	}
	if !inside {
		return "", nil
	}
	if rel == "." {
		return "", ErrBackupDirIsRepoRoot
	}
	slash := filepath.ToSlash(rel)
	entry = "/" + slash + "/"
	return entry, EnsureGitignoreLine(kahyaDir, entry, slash+"/")
}

// relInside returns target relative to base and whether it lies inside base
// (or is base itself, rel == ".").
func relInside(base, target string) (string, bool) {
	rel, err := filepath.Rel(filepath.Clean(base), filepath.Clean(target))
	if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// resolveExistingPrefix resolves symlinks in the longest existing prefix of
// p and re-attaches the not-yet-existing tail (backup_dir is created by the
// first backup-nightly run, so at boot it usually does not exist yet).
func resolveExistingPrefix(p string) string {
	p = filepath.Clean(p)
	var tail []string
	for cur := p; ; {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return resolved
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		tail = append(tail, filepath.Base(cur))
		cur = parent
	}
}

// EnsureGitignoreLine appends entry to <kahyaDir>/.gitignore unless entry or
// any alias is already present as a whole line (TrimSpace-compared). It
// creates the file when missing but never the directory, and never runs
// git. It is the one gitignore writer in the codebase (G00 C-21: later
// packages add their own lines through it).
func EnsureGitignoreLine(kahyaDir, entry string, aliases ...string) error {
	path := filepath.Join(kahyaDir, ".gitignore")
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("backup: read %s: %w", path, err)
	}
	want := append([]string{entry}, aliases...)
	for _, line := range strings.Split(string(existing), "\n") {
		t := strings.TrimSpace(line)
		for _, w := range want {
			if t == w {
				return nil // already present - idempotent no-op
			}
		}
	}
	content := string(existing)
	if len(content) > 0 && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += entry + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("backup: write %s: %w", path, err)
	}
	return nil
}
