package indexer

import (
	"context"
	"database/sql"
	"errors"
	"os/exec"
	"testing"

	"kahya/kahyad/internal/backup"
	"kahya/kahyad/internal/store/sqlcgen"
)

// scriptedGitRunner answers `git status --porcelain` and `git log -1
// --format=...` with canned output, keyed on the subcommand - a pure
// unit-level fake, no real git process at all.
type scriptedGitRunner struct {
	statusOut string
	statusErr error
	logOut    string
	logErr    error
}

func (f scriptedGitRunner) Run(_ context.Context, _ string, args ...string) (string, string, error) {
	if len(args) > 0 && args[0] == "status" {
		return f.statusOut, "", f.statusErr
	}
	return f.logOut, "", f.logErr
}

func TestResolveUserEditTierUpgradesOnCleanUserCommit(t *testing.T) {
	git := scriptedGitRunner{statusOut: "", logOut: "user <user@kahya.local>\n"}
	got := resolveUserEditTier(context.Background(), git, "/mem", "notlar/emre.md", "user_asserted")
	if got != userEditTier {
		t.Errorf("resolveUserEditTier() = %q, want %q", got, userEditTier)
	}
}

func TestResolveUserEditTierFallsBackOnDirtyWorkingTree(t *testing.T) {
	git := scriptedGitRunner{statusOut: " M notlar/emre.md\n", logOut: "user <user@kahya.local>\n"}
	got := resolveUserEditTier(context.Background(), git, "/mem", "notlar/emre.md", "user_asserted")
	if got != "user_asserted" {
		t.Errorf("resolveUserEditTier() = %q, want fallback %q (dirty tree is ambiguous)", got, "user_asserted")
	}
}

func TestResolveUserEditTierFallsBackOnNonUserAuthor(t *testing.T) {
	git := scriptedGitRunner{statusOut: "", logOut: "kahyad <kahyad@kahya.local>\n"}
	got := resolveUserEditTier(context.Background(), git, "/mem", "inbox/2026-07-12.md", "agent_derived")
	if got != "agent_derived" {
		t.Errorf("resolveUserEditTier() = %q, want fallback %q", got, "agent_derived")
	}
}

func TestResolveUserEditTierFallsBackOnNoCommitAtAll(t *testing.T) {
	git := scriptedGitRunner{statusOut: "", logOut: ""}
	got := resolveUserEditTier(context.Background(), git, "/mem", "inbox/fresh.md", "user_asserted")
	if got != "user_asserted" {
		t.Errorf("resolveUserEditTier() = %q, want fallback %q", got, "user_asserted")
	}
}

func TestResolveUserEditTierFallsBackOnGitError(t *testing.T) {
	git := scriptedGitRunner{statusErr: errors.New("boom: simulated git error")}
	got := resolveUserEditTier(context.Background(), git, "/mem", "notlar/x.md", "user_asserted")
	if got != "user_asserted" {
		t.Errorf("resolveUserEditTier() = %q, want fallback %q (git error is fail-safe)", got, "user_asserted")
	}
}

func TestResolveUserEditTierNilRunnerFallsBack(t *testing.T) {
	got := resolveUserEditTier(context.Background(), nil, "/mem", "notlar/x.md", "screen")
	if got != "screen" {
		t.Errorf("resolveUserEditTier() = %q, want fallback %q", got, "screen")
	}
}

// lookupEpisodeSourceTier is a small test-only helper mirroring
// indexer.go's own GetEpisodeBySourceAndPath call shape.
func lookupEpisodeSourceTier(t *testing.T, idx *Indexer, relPath string) string {
	t.Helper()
	row, err := idx.q.GetEpisodeBySourceAndPath(context.Background(), sqlcgen.GetEpisodeBySourceAndPathParams{
		Source:     sourceMemoryFile,
		SourcePath: sql.NullString{String: relPath, Valid: true},
	})
	if err != nil {
		t.Fatalf("GetEpisodeBySourceAndPath(%s): %v", relPath, err)
	}
	return row.SourceTier
}

// TestProcessFileAssignsUserEditTierAgainstRealGitRepo is the real,
// end-to-end proof this task's own gap-closing requirement demands: a
// genuine `git init` + `git commit --author="user <user@kahya.local>"`
// repository, indexed for real via Reindex, yields
// episodes.source_tier='user_edit' - never merely a unit-level fake.
func TestProcessFileAssignsUserEditTierAgainstRealGitRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not on PATH")
	}
	memDir := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", memDir}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	runGit("init", "-q")
	runGit("config", "user.name", "user")
	runGit("config", "user.email", "user@kahya.local")

	relPath := "notlar/gercek-kullanici-notu.md"
	writeFixture(t, memDir, relPath, "# Not\n\nBu kullanicinin kendi elle yazdigi bir not.\n")
	runGit("add", "-A")
	runGit("commit", "-q", "--author=user <user@kahya.local>", "-m", "user edit")

	st := newTestStore(t)
	idx := New(st.DB(), memDir, newTestLogger(t))
	idx.SetGitRunner(backup.NewExecGitRunner())

	if _, err := idx.Reindex(context.Background(), "trace-test", false); err != nil {
		t.Fatalf("Reindex() error = %v", err)
	}

	if got := lookupEpisodeSourceTier(t, idx, relPath); got != userEditTier {
		t.Errorf("source_tier = %q, want %q", got, userEditTier)
	}
}

// TestProcessFileFallsBackWithoutGitRepo proves the fail-safe default:
// no git repository at all leaves the existing front-matter/default tier
// untouched.
func TestProcessFileFallsBackWithoutGitRepo(t *testing.T) {
	memDir := t.TempDir()
	relPath := "notlar/no-git.md"
	writeFixture(t, memDir, relPath, "# Not\n\nHicbir git deposu yok.\n")

	st := newTestStore(t)
	idx := New(st.DB(), memDir, newTestLogger(t))
	idx.SetGitRunner(backup.NewExecGitRunner())

	if _, err := idx.Reindex(context.Background(), "trace-test", false); err != nil {
		t.Fatalf("Reindex() error = %v", err)
	}
	if got := lookupEpisodeSourceTier(t, idx, relPath); got != DefaultSourceTier {
		t.Errorf("source_tier = %q, want fallback default %q (no git repo)", got, DefaultSourceTier)
	}
}

// TestProcessFileFallsBackOnNonUserCommitAuthor proves a real repo whose
// commit author is NOT the exact user string never earns user_edit (e.g.
// kahyad's own consolidation-authored commits).
func TestProcessFileFallsBackOnNonUserCommitAuthor(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not on PATH")
	}
	memDir := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", memDir}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	runGit("init", "-q")
	runGit("config", "user.name", "kahyad")
	runGit("config", "user.email", "kahyad@kahya.local")

	relPath := "inbox/2026-07-12.md"
	writeFixture(t, memDir, relPath, "# Gunluk\n\nKahyad'in kendi yazdigi bir not.\n")
	runGit("add", "-A")
	runGit("commit", "-q", "--author=kahyad <kahyad@kahya.local>", "-m", "consolidation")

	st := newTestStore(t)
	idx := New(st.DB(), memDir, newTestLogger(t))
	idx.SetGitRunner(backup.NewExecGitRunner())

	if _, err := idx.Reindex(context.Background(), "trace-test", false); err != nil {
		t.Fatalf("Reindex() error = %v", err)
	}
	if got := lookupEpisodeSourceTier(t, idx, relPath); got != DefaultSourceTier {
		t.Errorf("source_tier = %q, want fallback default %q (non-user commit author)", got, DefaultSourceTier)
	}
}
