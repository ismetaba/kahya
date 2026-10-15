package backup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureGitignoreEntryCreatesFileWhenMissing(t *testing.T) {
	kahyaDir := t.TempDir()

	if err := EnsureGitignoreEntry(kahyaDir); err != nil {
		t.Fatalf("EnsureGitignoreEntry: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(kahyaDir, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if strings.TrimSpace(string(got)) != "backups/" {
		t.Errorf(".gitignore content = %q, want exactly \"backups/\\n\"", got)
	}
}

func TestEnsureGitignoreEntryAppendsWithoutDuplicating(t *testing.T) {
	kahyaDir := t.TempDir()
	path := filepath.Join(kahyaDir, ".gitignore")
	if err := os.WriteFile(path, []byte("*.tmp\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := EnsureGitignoreEntry(kahyaDir); err != nil {
		t.Fatalf("EnsureGitignoreEntry (1st): %v", err)
	}
	if err := EnsureGitignoreEntry(kahyaDir); err != nil {
		t.Fatalf("EnsureGitignoreEntry (2nd, idempotent): %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(got)
	if strings.Count(content, "backups/") != 1 {
		t.Errorf(".gitignore content = %q, want exactly one \"backups/\" line", content)
	}
	if !strings.Contains(content, "*.tmp") {
		t.Errorf(".gitignore content = %q, want the pre-existing \"*.tmp\" line preserved", content)
	}
}

func TestEnsureGitignoreEntryNoOpWhenAlreadyPresent(t *testing.T) {
	kahyaDir := t.TempDir()
	path := filepath.Join(kahyaDir, ".gitignore")
	original := "node_modules/\nbackups/\n*.log\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := EnsureGitignoreEntry(kahyaDir); err != nil {
		t.Fatalf("EnsureGitignoreEntry: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Errorf(".gitignore content = %q, want unchanged %q", got, original)
	}
}

// --- FIX-P-1: the configured backup dir is always git-ignored ---

func TestEnsureBackupDirIgnoredWritesAnchoredEntry(t *testing.T) {
	kahyaDir := t.TempDir()
	entry, err := EnsureBackupDirIgnored(kahyaDir, filepath.Join(kahyaDir, "backups"))
	if err != nil {
		t.Fatalf("EnsureBackupDirIgnored: %v", err)
	}
	if entry != "/backups/" {
		t.Errorf("entry = %q, want %q", entry, "/backups/")
	}
	got, err := os.ReadFile(filepath.Join(kahyaDir, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if string(got) != "/backups/\n" {
		t.Errorf(".gitignore = %q, want exactly %q", got, "/backups/\n")
	}

	// Idempotent: a second call returns the same entry and appends nothing.
	entry2, err := EnsureBackupDirIgnored(kahyaDir, filepath.Join(kahyaDir, "backups"))
	if err != nil || entry2 != "/backups/" {
		t.Fatalf("second call = (%q, %v), want (%q, nil)", entry2, err, "/backups/")
	}
	if again, _ := os.ReadFile(filepath.Join(kahyaDir, ".gitignore")); string(again) != "/backups/\n" {
		t.Errorf(".gitignore after second call = %q, want unchanged", again)
	}
}

func TestEnsureBackupDirIgnoredCustomNestedDir(t *testing.T) {
	kahyaDir := t.TempDir()
	entry, err := EnsureBackupDirIgnored(kahyaDir, filepath.Join(kahyaDir, "snapshots", "db"))
	if err != nil {
		t.Fatalf("EnsureBackupDirIgnored: %v", err)
	}
	if entry != "/snapshots/db/" {
		t.Errorf("entry = %q, want %q", entry, "/snapshots/db/")
	}
	got, _ := os.ReadFile(filepath.Join(kahyaDir, ".gitignore"))
	if string(got) != "/snapshots/db/\n" {
		t.Errorf(".gitignore = %q, want %q", got, "/snapshots/db/\n")
	}
}

func TestEnsureBackupDirIgnoredOutsideRepoIsNoOp(t *testing.T) {
	kahyaDir := t.TempDir()
	entry, err := EnsureBackupDirIgnored(kahyaDir, t.TempDir())
	if err != nil || entry != "" {
		t.Fatalf("EnsureBackupDirIgnored(outside) = (%q, %v), want (\"\", nil)", entry, err)
	}
	if _, err := os.Stat(filepath.Join(kahyaDir, ".gitignore")); !os.IsNotExist(err) {
		t.Errorf(".gitignore stat err = %v, want not-exist (nothing written)", err)
	}
	// A sibling whose name merely starts with the repo's name is outside too.
	sibling := kahyaDir + "-backups"
	if entry, err := EnsureBackupDirIgnored(kahyaDir, sibling); err != nil || entry != "" {
		t.Fatalf("EnsureBackupDirIgnored(%s) = (%q, %v), want (\"\", nil)", sibling, entry, err)
	}
}

func TestEnsureBackupDirIgnoredAcceptsExistingUnanchoredLine(t *testing.T) {
	kahyaDir := t.TempDir()
	path := filepath.Join(kahyaDir, ".gitignore")
	if err := os.WriteFile(path, []byte("backups/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	entry, err := EnsureBackupDirIgnored(kahyaDir, filepath.Join(kahyaDir, "backups"))
	if err != nil {
		t.Fatalf("EnsureBackupDirIgnored: %v", err)
	}
	if entry != "/backups/" {
		t.Errorf("entry = %q, want %q (callers use it as the guard prefix)", entry, "/backups/")
	}
	if got, _ := os.ReadFile(path); string(got) != "backups/\n" {
		t.Errorf(".gitignore = %q, want unchanged %q", got, "backups/\n")
	}
}

func TestEnsureBackupDirIgnoredRefusesRepoRoot(t *testing.T) {
	kahyaDir := t.TempDir()
	entry, err := EnsureBackupDirIgnored(kahyaDir, kahyaDir)
	if !errors.Is(err, ErrBackupDirIsRepoRoot) {
		t.Fatalf("EnsureBackupDirIgnored(root) err = %v, want ErrBackupDirIsRepoRoot", err)
	}
	if entry != "" {
		t.Errorf("entry = %q, want empty", entry)
	}
	if _, err := os.Stat(filepath.Join(kahyaDir, ".gitignore")); !os.IsNotExist(err) {
		t.Errorf(".gitignore stat err = %v, want not-exist", err)
	}
}

func TestEnsureBackupDirIgnoredSkipsMissingKahyaDir(t *testing.T) {
	kahyaDir := filepath.Join(t.TempDir(), "Kahya")
	entry, err := EnsureBackupDirIgnored(kahyaDir, filepath.Join(kahyaDir, "backups"))
	if err != nil || entry != "" {
		t.Fatalf("EnsureBackupDirIgnored(missing) = (%q, %v), want (\"\", nil)", entry, err)
	}
	if _, err := os.Stat(kahyaDir); !os.IsNotExist(err) {
		t.Errorf("kahyaDir stat err = %v, want not-exist (the helper must never create it)", err)
	}
}

// TestEnsureBackupDirIgnoredResolvesSymlinkedSpelling: a backup_dir that
// reaches kahya_dir through a symlink (here: the repo seen via a link) is
// still inside the repo and must still be ignored.
func TestEnsureBackupDirIgnoredResolvesSymlinkedSpelling(t *testing.T) {
	root := t.TempDir()
	kahyaDir := filepath.Join(root, "Kahya")
	if err := os.Mkdir(kahyaDir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(kahyaDir, link); err != nil {
		t.Fatal(err)
	}
	entry, err := EnsureBackupDirIgnored(kahyaDir, filepath.Join(link, "backups"))
	if err != nil {
		t.Fatalf("EnsureBackupDirIgnored: %v", err)
	}
	if entry != "/backups/" {
		t.Errorf("entry = %q, want %q", entry, "/backups/")
	}
	if got, _ := os.ReadFile(filepath.Join(kahyaDir, ".gitignore")); string(got) != "/backups/\n" {
		t.Errorf(".gitignore = %q, want %q", got, "/backups/\n")
	}
}
