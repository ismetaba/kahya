package consolidation

import (
	"context"
	"path/filepath"
	"testing"

	"kahya/kahyad/internal/backup"
)

// FIX-P-2: user-edit-wins must hold for notes whose names are not ASCII.
// git C-quotes such names in plain --name-only output (its default path
// quoting), and macOS hands back decomposed (NFD) bytes from a directory
// walk while git records the precomposed (NFC) name.

// runUserEditWinsCycle is TestRunUserEditWinsFullCycle's scenario for an
// arbitrary note path under memory/: the user edits line 2 today, the model
// proposes changing lines 2 and 4, and after Run+Approve line 2 must still
// be the user's while line 4 takes the model's change.
func runUserEditWinsCycle(t *testing.T, memRel string) {
	t.Helper()
	repo := initKahyaRepo(t)
	git := backup.NewExecGitRunner()
	notePath := "memory/" + memRel

	baseline := "unchanged intro\n" +
		"line two ORIGINAL\n" +
		"unchanged middle\n" +
		"line four original\n" +
		"unchanged outro\n"
	writeFile(t, repo, notePath, baseline)
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-m", "baseline (yesterday)")

	original := "unchanged intro\n" +
		"USER EDITED LINE TWO\n" +
		"unchanged middle\n" +
		"line four original\n" +
		"unchanged outro\n"
	writeFile(t, repo, notePath, original)
	if err := CommitAll(context.Background(), git, repo, UserCommitAuthor, UserPreCommitMessage); err != nil {
		t.Fatalf("seed user commit: %v", err)
	}

	proposedFor := 0
	cloud := SessionFunc(func(ctx context.Context, traceID string, files map[string]string) (map[string]string, error) {
		out := map[string]string{}
		for k, v := range files {
			// Keys are relative to MemoryDir and spelled the way the disk
			// walk returned them; compare normalized so the fixture works for
			// both spellings.
			if NormalizePathKey(k) == NormalizePathKey(memRel) && v == original {
				proposedFor++
				out[k] = "unchanged intro\n" +
					"MODEL OVERWRITES LINE TWO\n" +
					"unchanged middle\n" +
					"MODEL CHANGES LINE FOUR\n" +
					"unchanged outro\n"
				continue
			}
			out[k] = v
		}
		return out, nil
	})

	logger := &fakeEventStore{}
	c := &Consolidator{
		Cfg:         Config{KahyaDir: repo, MemoryDir: filepath.Join(repo, "memory"), WorktreeParentDir: t.TempDir(), Now: fixedNow(t)},
		Git:         git,
		Cloud:       cloud,
		EventLogger: logger,
		EventReader: logger,
	}
	if err := c.Run(context.Background(), "trace-1"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if proposedFor != 1 {
		t.Fatalf("the model saw %s %d times, want 1 (fixture did not reach the note)", memRel, proposedFor)
	}
	if err := c.Approve(context.Background(), "trace-2"); err != nil {
		t.Fatalf("Approve() error = %v", err)
	}

	got := readFile(t, filepath.Join(repo, notePath))
	want := "unchanged intro\n" +
		"USER EDITED LINE TWO\n" +
		"unchanged middle\n" +
		"MODEL CHANGES LINE FOUR\n" +
		"unchanged outro\n"
	if got != want {
		t.Fatalf("%s after approve =\n%q\nwant\n%q", notePath, got, want)
	}
}

func TestRunUserEditWinsTurkishFilenameFullCycle(t *testing.T) {
	runUserEditWinsCycle(t, "aile/şule.md") // NFC "aile/şule.md"
}

// TestRunUserEditWinsNFDFilenameFullCycle: the same note name written in
// decomposed form (s + U+0327 COMBINING CEDILLA). The disk walk returns
// these bytes; the user-touched map is keyed NFC; the lookup must normalize.
func TestRunUserEditWinsNFDFilenameFullCycle(t *testing.T) {
	runUserEditWinsCycle(t, "aile/şule.md")
}
