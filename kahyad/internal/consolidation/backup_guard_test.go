package consolidation

import (
	"context"
	"crypto/rand"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"kahya/kahyad/internal/backup"
)

// FIX-P-1: brain-*.db snapshots (the ledger, episodes, secret-lane facts)
// must never reach a commit of the memory repo, and the nightly pre-commit
// stages only the memory directory - never the whole ~/Kahya tree.

// randomBytes returns n bytes of crypto/rand output (a stand-in for an
// opaque SQLite snapshot).
func randomBytes(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// gitExit runs git in dir and returns its exit code (0 = success), for
// probes like `check-ignore -q` whose non-zero exit is an answer, not a
// fixture failure.
func gitExit(t *testing.T, dir string, args ...string) int {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	err := cmd.Run()
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	t.Fatalf("git -C %s %s: %v", dir, strings.Join(args, " "), err)
	return -1
}

// commitFiles returns the paths a commit touched (sorted).
func commitFiles(t *testing.T, repo, rev string) []string {
	t.Helper()
	out := runGit(t, repo, "show", "--name-only", "--format=", rev)
	files := splitNonEmptyLines(out)
	sort.Strings(files)
	return files
}

// stagedFiles returns `git diff --cached --name-only` (sorted).
func stagedFiles(t *testing.T, repo string) []string {
	t.Helper()
	files := splitNonEmptyLines(runGit(t, repo, "diff", "--cached", "--name-only"))
	sort.Strings(files)
	return files
}

func commitCount(t *testing.T, repo string) int {
	t.Helper()
	n, err := strconv.Atoi(runGit(t, repo, "rev-list", "--count", "--all"))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func eventKinds(rows []fakeEventRow) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.kind)
	}
	return out
}

func hasKind(rows []fakeEventRow, kind string) bool {
	for _, r := range rows {
		if r.kind == kind {
			return true
		}
	}
	return false
}

func TestRunNeverCommitsBackupSnapshots(t *testing.T) {
	repo := initKahyaRepo(t)
	writeFile(t, repo, "backups/brain-20260711.db", randomBytes(t, 4096))
	// A crashed backup-nightly leaves its unverified staging copy next to
	// the snapshots; it carries the same bytes.
	writeFile(t, repo, "backups/brain-20260712.db.staging", randomBytes(t, 4096))
	// A same-day memory edit, so the author=user pre-commit really runs.
	writeFile(t, repo, "memory/note.md", "line one\nline two USER EDIT\nline three\n")

	var received []map[string]string
	logger := &fakeEventStore{}
	c := &Consolidator{
		Cfg: Config{
			KahyaDir: repo, MemoryDir: filepath.Join(repo, "memory"), BackupDir: filepath.Join(repo, "backups"),
			WorktreeParentDir: t.TempDir(), Now: fixedNow(t),
		},
		Git: backup.NewExecGitRunner(), Cloud: echoSession(&received),
		Notifier: &fakeNotifier{}, EventLogger: logger, EventReader: logger,
	}
	if err := c.Run(context.Background(), "trace-p1"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if out := runGit(t, repo, "log", "--all", "--name-only", "--format="); strings.Contains(out, "backups/") {
		t.Fatalf("git history contains a backup path:\n%s", out)
	}
	gitignore := readFile(t, filepath.Join(repo, ".gitignore"))
	if !strings.Contains("\n"+gitignore, "\n/backups/\n") {
		t.Fatalf(".gitignore = %q, want an anchored /backups/ line", gitignore)
	}
	for _, p := range []string{"backups/brain-20260711.db", "backups/brain-20260712.db.staging"} {
		if code := gitExit(t, repo, "check-ignore", "-q", p); code != 0 {
			t.Errorf("git check-ignore %s exit = %d, want 0 (ignored)", p, code)
		}
	}

	rows := gitLogAuthorsAndSubjects(t, repo)
	last := rows[len(rows)-1]
	if last != [2]string{UserCommitAuthor, UserPreCommitMessage} {
		t.Fatalf("newest commit = %v, want the author=user pre-commit", last)
	}
	if got := commitFiles(t, repo, "HEAD"); strings.Join(got, ",") != "memory/note.md" {
		t.Fatalf("pre-commit files = %v, want only [memory/note.md] (never backups/, never .gitignore)", got)
	}
}

func TestRunRefusesToCommitStagedBackupPath(t *testing.T) {
	repo := initKahyaRepo(t)
	writeFile(t, repo, ".gitignore", "/backups/\n")
	runGit(t, repo, "add", ".gitignore")
	runGit(t, repo, "commit", "-m", "ignore backups")
	writeFile(t, repo, "backups/x.db", randomBytes(t, 1024))
	runGit(t, repo, "add", "-f", "backups/x.db")
	writeFile(t, repo, "memory/note.md", "line one\nline two USER EDIT\nline three\n")
	before := commitCount(t, repo)

	var received []map[string]string
	logger := &fakeEventStore{}
	notifier := &fakeNotifier{}
	c := &Consolidator{
		Cfg: Config{
			KahyaDir: repo, MemoryDir: filepath.Join(repo, "memory"), BackupDir: filepath.Join(repo, "backups"),
			WorktreeParentDir: t.TempDir(), Now: fixedNow(t),
		},
		Git: backup.NewExecGitRunner(), Cloud: echoSession(&received),
		Notifier: notifier, EventLogger: logger, EventReader: logger,
	}
	err := c.Run(context.Background(), "trace-p1-staged")
	if !errors.Is(err, ErrBackupPathStaged) {
		t.Fatalf("Run() error = %v, want ErrBackupPathStaged", err)
	}
	if after := commitCount(t, repo); after != before {
		t.Errorf("commit count %d -> %d, want unchanged", before, after)
	}
	for _, p := range stagedFiles(t, repo) {
		if strings.HasPrefix(p, "backups/") {
			t.Errorf("backup path %s still staged after the refusal, want it un-staged", p)
		}
	}
	if _, statErr := os.Stat(filepath.Join(repo, "backups", "x.db")); statErr != nil {
		t.Errorf("the snapshot file itself must stay on disk: %v", statErr)
	}
	if !hasKind(logger.rows, EventBackupDirStaged) {
		t.Errorf("ledger kinds = %v, want %s", eventKinds(logger.rows), EventBackupDirStaged)
	}
	if len(notifier.sent) != 1 || notifier.sent[0] != MsgBackupDirStaged {
		t.Errorf("notifier.sent = %q, want exactly [%q]", notifier.sent, MsgBackupDirStaged)
	}
	if br := runGit(t, repo, "branch", "--list", ConsolidationBranchPrefix+"*"); br != "" {
		t.Errorf("consolidation branch created despite the refusal: %q", br)
	}
	if len(received) != 0 {
		t.Errorf("cloud session called %d times, want 0 (refused before any session)", len(received))
	}
}

// TestRunRefusesNegatedBackupIgnoreUnderMemoryDir: a backup_dir inside the
// memory directory is staged by the memory-only pre-commit itself once a
// .gitignore negation re-includes it - the post-staging guard must catch it.
func TestRunRefusesNegatedBackupIgnoreUnderMemoryDir(t *testing.T) {
	repo := initKahyaRepo(t)
	writeFile(t, repo, ".gitignore", "/memory/backups/\n!/memory/backups/\n")
	writeFile(t, repo, "memory/backups/brain-20260711.db", randomBytes(t, 1024))
	writeFile(t, repo, "memory/note.md", "line one\nline two USER EDIT\nline three\n")
	before := commitCount(t, repo)

	var received []map[string]string
	logger := &fakeEventStore{}
	c := &Consolidator{
		Cfg: Config{
			KahyaDir: repo, MemoryDir: filepath.Join(repo, "memory"), BackupDir: filepath.Join(repo, "memory", "backups"),
			WorktreeParentDir: t.TempDir(), Now: fixedNow(t),
		},
		Git: backup.NewExecGitRunner(), Cloud: echoSession(&received),
		Notifier: &fakeNotifier{}, EventLogger: logger, EventReader: logger,
	}
	if err := c.Run(context.Background(), "trace-p1-negated"); !errors.Is(err, ErrBackupPathStaged) {
		t.Fatalf("Run() error = %v, want ErrBackupPathStaged", err)
	}
	if after := commitCount(t, repo); after != before {
		t.Errorf("commit count %d -> %d, want unchanged", before, after)
	}
	if out := runGit(t, repo, "log", "--all", "--name-only", "--format="); strings.Contains(out, "memory/backups/") {
		t.Fatalf("git history contains a backup path:\n%s", out)
	}
	for _, p := range stagedFiles(t, repo) {
		if strings.HasPrefix(p, "memory/backups/") {
			t.Errorf("backup path %s still staged after the refusal", p)
		}
	}
}

func TestAssertNoPathsStagedUnderMatchesPrefixBoundary(t *testing.T) {
	repo := initKahyaRepo(t)
	git := backup.NewExecGitRunner()
	ctx := context.Background()

	writeFile(t, repo, "backups2/x", "not a backup dir\n")
	writeFile(t, repo, "backupsfile", "neither\n")
	runGit(t, repo, "add", "backups2/x", "backupsfile")
	if err := AssertNoPathsStagedUnder(ctx, git, repo, "backups"); err != nil {
		t.Fatalf("AssertNoPathsStagedUnder(backups2/x, backupsfile) = %v, want nil (prefix boundary)", err)
	}
	if got := stagedFiles(t, repo); strings.Join(got, ",") != "backups2/x,backupsfile" {
		t.Fatalf("staged = %v, want the unrelated paths left staged", got)
	}

	writeFile(t, repo, "backups/a/b", "snapshot\n")
	runGit(t, repo, "add", "backups/a/b")
	err := AssertNoPathsStagedUnder(ctx, git, repo, "backups")
	if !errors.Is(err, ErrBackupPathStaged) {
		t.Fatalf("AssertNoPathsStagedUnder(backups/a/b) = %v, want ErrBackupPathStaged", err)
	}
	// Only the offending prefix is un-staged; the user's other staged work stays.
	if got := stagedFiles(t, repo); strings.Join(got, ",") != "backups2/x,backupsfile" {
		t.Fatalf("staged after refusal = %v, want [backups2/x backupsfile]", got)
	}
}

// TestRunPreCommitStagesOnlyMemoryDir: the author=user pre-commit carries
// only the memory directory's changes (added, modified, deleted, untracked);
// edits elsewhere in ~/Kahya - and anything the user staged by hand - are
// neither committed nor un-staged.
func TestRunPreCommitStagesOnlyMemoryDir(t *testing.T) {
	repo := initKahyaRepo(t)
	writeFile(t, repo, "README.md", "readme v1\n")
	writeFile(t, repo, "memory/old.md", "to be deleted\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-m", "seed readme + old note")

	writeFile(t, repo, "README.md", "readme v2 (user)\n")
	writeFile(t, repo, "scratch.txt", "untracked root file\n")
	writeFile(t, repo, "staged.txt", "staged by the user\n")
	runGit(t, repo, "add", "staged.txt")
	writeFile(t, repo, "memory/note.md", "line one\nline two USER EDIT\nline three\n")
	writeFile(t, repo, "memory/sub/new.md", "brand new note\n")
	if err := os.Remove(filepath.Join(repo, "memory", "old.md")); err != nil {
		t.Fatal(err)
	}

	var received []map[string]string
	logger := &fakeEventStore{}
	c := &Consolidator{
		Cfg:         Config{KahyaDir: repo, MemoryDir: filepath.Join(repo, "memory"), WorktreeParentDir: t.TempDir(), Now: fixedNow(t)},
		Git:         backup.NewExecGitRunner(),
		Cloud:       echoSession(&received),
		EventLogger: logger, EventReader: logger,
	}
	if err := c.Run(context.Background(), "trace-p1-memonly"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	rows := gitLogAuthorsAndSubjects(t, repo)
	if last := rows[len(rows)-1]; last != [2]string{UserCommitAuthor, UserPreCommitMessage} {
		t.Fatalf("newest commit = %v, want the author=user pre-commit", last)
	}
	want := "memory/note.md,memory/old.md,memory/sub/new.md"
	if got := commitFiles(t, repo, "HEAD"); strings.Join(got, ",") != want {
		t.Fatalf("pre-commit files = %v, want [%s]", got, want)
	}
	if got := stagedFiles(t, repo); strings.Join(got, ",") != "staged.txt" {
		t.Errorf("staged after Run = %v, want the user's own [staged.txt] untouched", got)
	}
	// Non-memory work stays exactly as the user left it.
	if got := runGit(t, repo, "diff", "--name-only"); got != "README.md" {
		t.Errorf("unstaged changes = %q, want README.md still modified and uncommitted", got)
	}
	if got := runGit(t, repo, "ls-files", "--others", "--exclude-standard"); got != "scratch.txt" {
		t.Errorf("untracked files = %q, want scratch.txt still untracked", got)
	}
}

// TestRunSkipsPreCommitWhenOnlyNonMemoryPathsAreDirty: a dirty tree outside
// the memory directory is not a memory edit - no author=user commit at all.
func TestRunSkipsPreCommitWhenOnlyNonMemoryPathsAreDirty(t *testing.T) {
	repo := initKahyaRepo(t)
	writeFile(t, repo, "README.md", "edited outside memory\n")
	before := commitCount(t, repo)

	var received []map[string]string
	logger := &fakeEventStore{}
	c := &Consolidator{
		Cfg:         Config{KahyaDir: repo, MemoryDir: filepath.Join(repo, "memory"), WorktreeParentDir: t.TempDir(), Now: fixedNow(t)},
		Git:         backup.NewExecGitRunner(),
		Cloud:       echoSession(&received),
		EventLogger: logger, EventReader: logger,
	}
	if err := c.Run(context.Background(), "trace-p1-nonmem"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if after := commitCount(t, repo); after != before {
		t.Fatalf("commit count %d -> %d, want unchanged (echo session, nothing in memory/ dirty)", before, after)
	}
	if !strings.Contains(runGit(t, repo, "status", "--porcelain"), "?? README.md") {
		t.Errorf("README.md must stay untracked and uncommitted")
	}
}

// TestRunRefusesMemoryDirThatIsNotARepoSubdirectory: with memory_dir equal
// to (or outside) kahya_dir there is no memory-only pathspec to stage, so
// the run refuses instead of falling back to the whole tree.
func TestRunRefusesMemoryDirThatIsNotARepoSubdirectory(t *testing.T) {
	for name, memDir := range map[string]func(repo string) string{
		"repo_root": func(repo string) string { return repo },
		"outside":   func(string) string { return t.TempDir() },
	} {
		t.Run(name, func(t *testing.T) {
			repo := initKahyaRepo(t)
			writeFile(t, repo, "memory/note.md", "line one\nline two USER EDIT\nline three\n")
			before := commitCount(t, repo)
			logger := &fakeEventStore{}
			var received []map[string]string
			c := &Consolidator{
				Cfg:         Config{KahyaDir: repo, MemoryDir: memDir(repo), WorktreeParentDir: t.TempDir(), Now: fixedNow(t)},
				Git:         backup.NewExecGitRunner(),
				Cloud:       echoSession(&received),
				EventLogger: logger, EventReader: logger,
			}
			if err := c.Run(context.Background(), "trace-p1-memdir"); !errors.Is(err, ErrMemoryDirNotInRepo) {
				t.Fatalf("Run() error = %v, want ErrMemoryDirNotInRepo", err)
			}
			if after := commitCount(t, repo); after != before {
				t.Errorf("commit count %d -> %d, want unchanged", before, after)
			}
		})
	}
}

// TestProductionCodeNeverStagesWholeTree pins "stage only memory/**": no
// non-test file of this package passes "-A" to git, and every argv that
// starts with "add" carries the "--" pathspec separator.
func TestProductionCodeNeverStagesWholeTree(t *testing.T) {
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	sawAdd := false
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", p, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			var elts []ast.Expr
			switch x := n.(type) {
			case *ast.CompositeLit:
				elts = x.Elts
			case *ast.CallExpr:
				elts = x.Args
			default:
				return true
			}
			var lits []string
			for _, e := range elts {
				if bl, ok := e.(*ast.BasicLit); ok && bl.Kind == token.STRING {
					v, _ := strconv.Unquote(bl.Value)
					lits = append(lits, v)
				}
			}
			for i, v := range lits {
				if v == "-A" {
					t.Errorf("%s: git argument \"-A\" in production code (%s)", p, fset.Position(n.Pos()))
				}
				if v == "add" && i == 0 {
					sawAdd = true
					hasSep := false
					for _, w := range lits {
						hasSep = hasSep || w == "--"
					}
					if !hasSep {
						t.Errorf("%s: a git add argv without the \"--\" pathspec separator (%s)", p, fset.Position(n.Pos()))
					}
				}
			}
			return true
		})
	}
	if !sawAdd {
		t.Fatal("found no git add argv at all - the scan no longer sees StagePaths")
	}
}
