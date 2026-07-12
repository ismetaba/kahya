package backup

import (
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
