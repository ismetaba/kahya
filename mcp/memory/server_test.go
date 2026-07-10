package memory

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ---- fakes ----

type fakeSearcher struct {
	hits      []Hit
	err       error
	lastQuery string
	lastK     int
	lastTID   string
}

func (f *fakeSearcher) Search(_ context.Context, traceID, q string, k int) ([]Hit, error) {
	f.lastTID, f.lastQuery, f.lastK = traceID, q, k
	if f.err != nil {
		return nil, f.err
	}
	return f.hits, nil
}

type fakeIndexer struct {
	episodeID int64
	err       error
	calls     []string
}

func (f *fakeIndexer) ReindexFile(_ context.Context, _ string, relPath string) (int64, error) {
	f.calls = append(f.calls, relPath)
	if f.err != nil {
		return 0, f.err
	}
	return f.episodeID, nil
}

type ledgerEvent struct {
	traceID string
	kind    string
	payload map[string]any
}

type fakeLedger struct {
	events []ledgerEvent
	err    error
}

func (f *fakeLedger) LogEvent(_ context.Context, traceID, kind string, payload map[string]any) error {
	f.events = append(f.events, ledgerEvent{traceID, kind, payload})
	return f.err
}

// ---- fixture git repo helper: memoryDir is a SUBDIRECTORY of the repo
// root, mirroring production (~/Kahya is the repo, ~/Kahya/memory is
// cfg.memory_dir) - so gitRepoRoot's "git -C memoryDir rev-parse
// --show-toplevel" must walk up one level, exactly like prod.

func newFixtureRepo(t *testing.T) (repoRoot, memoryDir string) {
	t.Helper()
	repoRoot = t.TempDir()
	memoryDir = filepath.Join(repoRoot, "memory")
	if err := os.MkdirAll(memoryDir, 0o700); err != nil {
		t.Fatalf("mkdir memory dir: %v", err)
	}
	runGitT(t, repoRoot, "init", "-q")
	return repoRoot, memoryDir
}

func runGitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newTestServer(memoryDir string, search Searcher, idx Indexer, ledger Ledger) *Server {
	return New(memoryDir, search, idx, ledger, nil)
}

// ---- resolveMemoryPath / traversal rejection (W12-05 step 9) ----

func TestResolveMemoryPathRejectsAbsolutePath(t *testing.T) {
	memDir := t.TempDir()
	if _, err := resolveMemoryPath(memDir, "/etc/passwd"); err == nil {
		t.Fatal("resolveMemoryPath(absolute) = nil error, want error")
	}
}

func TestResolveMemoryPathRejectsTraversal(t *testing.T) {
	memDir := t.TempDir()
	if _, err := resolveMemoryPath(memDir, "../../etc/x"); err == nil {
		t.Fatal("resolveMemoryPath(../../etc/x) = nil error, want error")
	}
}

func TestResolveMemoryPathRejectsEmpty(t *testing.T) {
	memDir := t.TempDir()
	if _, err := resolveMemoryPath(memDir, ""); err == nil {
		t.Fatal("resolveMemoryPath(\"\") = nil error, want error")
	}
}

func TestResolveMemoryPathRejectsSymlinkEscape(t *testing.T) {
	memDir := t.TempDir()
	outside := t.TempDir()
	// memDir/evil -> outside (a symlinked directory escaping memDir).
	if err := os.Symlink(outside, filepath.Join(memDir, "evil")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := resolveMemoryPath(memDir, "evil/x.md"); err == nil {
		t.Fatal("resolveMemoryPath(through a symlinked dir) = nil error, want error")
	}
}

func TestResolveMemoryPathRejectsSymlinkLeaf(t *testing.T) {
	memDir := t.TempDir()
	outsideFile := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outsideFile, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(memDir, "link.md")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := resolveMemoryPath(memDir, "link.md"); err == nil {
		t.Fatal("resolveMemoryPath(symlink leaf) = nil error, want error")
	}
}

func TestResolveMemoryPathAllowsOrdinaryNestedPath(t *testing.T) {
	memDir := t.TempDir()
	abs, err := resolveMemoryPath(memDir, "inbox/2026-07-10.md")
	if err != nil {
		t.Fatalf("resolveMemoryPath: %v", err)
	}
	want := filepath.Join(memDir, "inbox", "2026-07-10.md")
	// memDir may itself resolve through symlinks on macOS (e.g. /var ->
	// /private/var); compare the EvalSymlinks'd want, matching what
	// resolveMemoryPath itself does internally.
	wantResolved, err := filepath.EvalSymlinks(filepath.Dir(filepath.Dir(want)))
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if abs != filepath.Join(wantResolved, "inbox", "2026-07-10.md") {
		t.Fatalf("resolveMemoryPath = %q, want %q", abs, filepath.Join(wantResolved, "inbox", "2026-07-10.md"))
	}
}

// ---- memory_search ----

func TestHandleSearchEmptyQueryErrors(t *testing.T) {
	s := newTestServer(t.TempDir(), &fakeSearcher{}, &fakeIndexer{}, &fakeLedger{})
	if _, err := s.HandleSearch(context.Background(), "tid", MemorySearchArgs{Query: "   "}); err == nil {
		t.Fatal("HandleSearch(empty query) = nil error, want error")
	}
}

func TestHandleSearchReturnsMappedResults(t *testing.T) {
	fs := &fakeSearcher{hits: []Hit{
		{Path: "note.md", Seq: 1, Text: "ev bakiyoruz", Score: 0.7, SourceTier: "user_asserted"},
	}}
	s := newTestServer(t.TempDir(), fs, &fakeIndexer{}, &fakeLedger{})
	out, err := s.HandleSearch(context.Background(), "tid-1", MemorySearchArgs{Query: "ev", K: 3})
	if err != nil {
		t.Fatalf("HandleSearch: %v", err)
	}
	if fs.lastTID != "tid-1" || fs.lastQuery != "ev" || fs.lastK != 3 {
		t.Errorf("Search called with (%q,%q,%d), want (tid-1,ev,3)", fs.lastTID, fs.lastQuery, fs.lastK)
	}
	if len(out.Results) != 1 || out.Results[0].Path != "note.md" || out.Results[0].Seq != 1 ||
		out.Results[0].Text != "ev bakiyoruz" || out.Results[0].SourceTier != "user_asserted" {
		t.Errorf("HandleSearch results = %+v, want the single mapped hit", out.Results)
	}
}

// ---- memory_write ----

func TestHandleWriteCreatesFileWithFrontMatterAndCommits(t *testing.T) {
	repoRoot, memDir := newFixtureRepo(t)
	idx := &fakeIndexer{episodeID: 42}
	led := &fakeLedger{}
	s := newTestServer(memDir, &fakeSearcher{}, idx, led)

	out, err := s.HandleWrite(context.Background(), "tid-w1", MemoryWriteArgs{
		Content: "Kadıköy'de iki daire gezdik", File: "notes/kadikoy.md",
	})
	if err != nil {
		t.Fatalf("HandleWrite: %v", err)
	}
	if out.File != "notes/kadikoy.md" {
		t.Errorf("out.File = %q, want notes/kadikoy.md", out.File)
	}
	if out.EpisodeID != 42 {
		t.Errorf("out.EpisodeID = %d, want 42", out.EpisodeID)
	}
	if out.CommitSHA == "" {
		t.Error("out.CommitSHA is empty")
	}

	raw, err := os.ReadFile(filepath.Join(memDir, "notes", "kadikoy.md"))
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	content := string(raw)
	if !strings.HasPrefix(content, "---\nkahya_source_tier: agent_derived\n---\n") {
		t.Errorf("written file does not start with the agent_derived front matter, got: %q", content)
	}
	if !strings.Contains(content, "Kadıköy'de iki daire gezdik") {
		t.Errorf("written file missing the Turkish content, got: %q", content)
	}

	author := runGitT(t, repoRoot, "log", "-1", "--format=%an")
	if author != "kahyad" {
		t.Errorf("git log author = %q, want kahyad", author)
	}
	headSHA := runGitT(t, repoRoot, "rev-parse", "HEAD")
	if headSHA != out.CommitSHA {
		t.Errorf("out.CommitSHA = %q, want HEAD %q", out.CommitSHA, headSHA)
	}

	if len(idx.calls) != 1 || idx.calls[0] != "notes/kadikoy.md" {
		t.Errorf("ReindexFile calls = %v, want [notes/kadikoy.md]", idx.calls)
	}
	if len(led.events) != 1 || led.events[0].kind != "memory_write" {
		t.Fatalf("ledger events = %+v, want one memory_write event", led.events)
	}
	if led.events[0].payload["file"] != "notes/kadikoy.md" || led.events[0].payload["commit_sha"] != out.CommitSHA {
		t.Errorf("ledger payload = %+v, want file/commit_sha to match", led.events[0].payload)
	}
	if led.events[0].traceID != "tid-w1" {
		t.Errorf("ledger traceID = %q, want tid-w1", led.events[0].traceID)
	}
}

func TestHandleWriteDefaultFileUsesInboxDateFormat(t *testing.T) {
	_, memDir := newFixtureRepo(t)
	s := newTestServer(memDir, &fakeSearcher{}, &fakeIndexer{}, &fakeLedger{})

	out, err := s.HandleWrite(context.Background(), "tid", MemoryWriteArgs{Content: "not"})
	if err != nil {
		t.Fatalf("HandleWrite: %v", err)
	}
	if !regexp.MustCompile(`^inbox/\d{4}-\d{2}-\d{2}\.md$`).MatchString(out.File) {
		t.Errorf("default File = %q, want inbox/YYYY-MM-DD.md shape", out.File)
	}
	if _, err := os.Stat(filepath.Join(memDir, filepath.FromSlash(out.File))); err != nil {
		t.Errorf("default inbox file not created on disk: %v", err)
	}
}

func TestHandleWriteAppendsToExistingFileWithSeparator(t *testing.T) {
	_, memDir := newFixtureRepo(t)
	s := newTestServer(memDir, &fakeSearcher{}, &fakeIndexer{}, &fakeLedger{})

	if _, err := s.HandleWrite(context.Background(), "t1", MemoryWriteArgs{Content: "birinci not", File: "inbox/x.md"}); err != nil {
		t.Fatalf("first HandleWrite: %v", err)
	}
	if _, err := s.HandleWrite(context.Background(), "t2", MemoryWriteArgs{Content: "ikinci not", File: "inbox/x.md"}); err != nil {
		t.Fatalf("second HandleWrite: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(memDir, "inbox", "x.md"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	content := string(raw)
	want := "---\nkahya_source_tier: agent_derived\n---\nbirinci not\n\n---\n\nikinci not"
	if content != want {
		t.Fatalf("appended file content = %q, want %q", content, want)
	}
}

func TestHandleWriteRejectsTraversal(t *testing.T) {
	_, memDir := newFixtureRepo(t)
	s := newTestServer(memDir, &fakeSearcher{}, &fakeIndexer{}, &fakeLedger{})
	if _, err := s.HandleWrite(context.Background(), "t", MemoryWriteArgs{Content: "x", File: "../../etc/passwd"}); err == nil {
		t.Fatal("HandleWrite(traversal) = nil error, want error")
	}
}

// ---- memory_forget ----

func TestHandleForgetHeadingRemovesOnlyThatSection(t *testing.T) {
	repoRoot, memDir := newFixtureRepo(t)
	original := "# Notlar\n\n## Birinci\nBirinci icerik.\n\n## Ikinci\nIkinci icerik.\n\n## Ucuncu\nUcuncu icerik.\n"
	if err := os.WriteFile(filepath.Join(memDir, "notes.md"), []byte(original), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	runGitT(t, repoRoot, "add", "-A")
	runGitT(t, repoRoot, "-c", "user.email=seed@example.com", "-c", "user.name=Seed", "commit", "-q", "-m", "seed")

	idx := &fakeIndexer{episodeID: 7}
	led := &fakeLedger{}
	s := newTestServer(memDir, &fakeSearcher{}, idx, led)

	out, err := s.HandleForget(context.Background(), "tid-f1", MemoryForgetArgs{File: "notes.md", Heading: "Ikinci"})
	if err != nil {
		t.Fatalf("HandleForget: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(memDir, "notes.md"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := string(raw)
	if strings.Contains(got, "Ikinci") {
		t.Errorf("removed section still present: %q", got)
	}
	if !strings.Contains(got, "Birinci icerik.") || !strings.Contains(got, "Ucuncu icerik.") {
		t.Errorf("unrelated sections were affected: %q", got)
	}

	author := runGitT(t, repoRoot, "log", "-1", "--format=%an")
	if author != "kahyad" {
		t.Errorf("git log author = %q, want kahyad", author)
	}
	if out.CommitSHA == "" {
		t.Error("CommitSHA is empty")
	}
	if len(idx.calls) != 1 || idx.calls[0] != "notes.md" {
		t.Errorf("ReindexFile calls = %v, want [notes.md]", idx.calls)
	}
	if len(led.events) != 1 || led.events[0].kind != "memory_forget" || led.events[0].payload["heading"] != "Ikinci" {
		t.Errorf("ledger events = %+v, want one memory_forget event with heading=Ikinci", led.events)
	}
}

func TestHandleForgetHeadingNotFoundErrors(t *testing.T) {
	_, memDir := newFixtureRepo(t)
	if err := os.WriteFile(filepath.Join(memDir, "notes.md"), []byte("# A\ntext\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s := newTestServer(memDir, &fakeSearcher{}, &fakeIndexer{}, &fakeLedger{})
	if _, err := s.HandleForget(context.Background(), "t", MemoryForgetArgs{File: "notes.md", Heading: "Yok Boyle Bir Sey"}); err == nil {
		t.Fatal("HandleForget(missing heading) = nil error, want error")
	}
}

func TestHandleForgetWholeFileMovesToTrashAndCommits(t *testing.T) {
	repoRoot, memDir := newFixtureRepo(t)
	if err := os.WriteFile(filepath.Join(memDir, "notes.md"), []byte("iceri"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	runGitT(t, repoRoot, "add", "-A")
	runGitT(t, repoRoot, "-c", "user.email=seed@example.com", "-c", "user.name=Seed", "commit", "-q", "-m", "seed")

	idx := &fakeIndexer{episodeID: 9}
	led := &fakeLedger{}
	s := newTestServer(memDir, &fakeSearcher{}, idx, led)

	out, err := s.HandleForget(context.Background(), "tid-f2", MemoryForgetArgs{File: "notes.md"})
	if err != nil {
		t.Fatalf("HandleForget: %v", err)
	}

	if _, err := os.Stat(filepath.Join(memDir, "notes.md")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("original file still present (err=%v), want it gone", err)
	}

	entries, err := os.ReadDir(filepath.Join(memDir, ".trash"))
	if err != nil {
		t.Fatalf("read .trash: %v", err)
	}
	if len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), "-notes.md") {
		t.Fatalf(".trash entries = %v, want exactly one *-notes.md", entries)
	}
	trashed, err := os.ReadFile(filepath.Join(memDir, ".trash", entries[0].Name()))
	if err != nil || string(trashed) != "iceri" {
		t.Errorf(".trash file content = %q (err=%v), want %q", trashed, err, "iceri")
	}

	// Present in git history: the seed commit's blob is still reachable.
	logOut := runGitT(t, repoRoot, "log", "--all", "--oneline")
	if !strings.Contains(logOut, "seed") {
		t.Errorf("git history missing the seed commit: %s", logOut)
	}
	author := runGitT(t, repoRoot, "log", "-1", "--format=%an")
	if author != "kahyad" {
		t.Errorf("git log author = %q, want kahyad", author)
	}
	if out.CommitSHA == "" {
		t.Error("CommitSHA is empty")
	}
	if len(idx.calls) != 1 || idx.calls[0] != "notes.md" {
		t.Errorf("ReindexFile calls = %v, want [notes.md] (original path, so the indexer detects it's gone)", idx.calls)
	}
	if len(led.events) != 1 || led.events[0].kind != "memory_forget" {
		t.Errorf("ledger events = %+v, want one memory_forget event", led.events)
	}
}

func TestHandleForgetMissingFileErrors(t *testing.T) {
	_, memDir := newFixtureRepo(t)
	s := newTestServer(memDir, &fakeSearcher{}, &fakeIndexer{}, &fakeLedger{})
	if _, err := s.HandleForget(context.Background(), "t", MemoryForgetArgs{File: "does-not-exist.md"}); err == nil {
		t.Fatal("HandleForget(missing file) = nil error, want error")
	}
}

func TestHandleForgetRejectsTraversal(t *testing.T) {
	_, memDir := newFixtureRepo(t)
	s := newTestServer(memDir, &fakeSearcher{}, &fakeIndexer{}, &fakeLedger{})
	if _, err := s.HandleForget(context.Background(), "t", MemoryForgetArgs{File: "../../etc/passwd"}); err == nil {
		t.Fatal("HandleForget(traversal) = nil error, want error")
	}
}

// ---- small pure-function unit tests ----

func TestRemoveSectionNestedLevels(t *testing.T) {
	content := "# Top\n\n## A\ntextA\n\n### A-sub\nsubtext\n\n## B\ntextB\n"
	got, found := removeSection(content, "A")
	if !found {
		t.Fatal("removeSection: heading A not found")
	}
	if strings.Contains(got, "textA") || strings.Contains(got, "subtext") {
		t.Errorf("removeSection did not remove the nested sub-heading: %q", got)
	}
	if !strings.Contains(got, "textB") {
		t.Errorf("removeSection removed too much: %q", got)
	}
}

func TestRemoveSectionHeadingPrefixTolerant(t *testing.T) {
	content := "## Kadıköy\nnot\n\n## Diger\nx\n"
	got, found := removeSection(content, "## Kadıköy")
	if !found {
		t.Fatal("removeSection with a '#'-prefixed heading argument: not found")
	}
	if strings.Contains(got, "Kadıköy") {
		t.Errorf("section not removed: %q", got)
	}
}

func TestStripLeadingFrontMatterRemovesBlock(t *testing.T) {
	in := "---\nkahya_source_tier: user_asserted\n---\nBody text\n"
	got := stripLeadingFrontMatter(in)
	if got != "Body text\n" {
		t.Errorf("stripLeadingFrontMatter = %q, want %q", got, "Body text\n")
	}
}

func TestStripLeadingFrontMatterLeavesUnterminatedAlone(t *testing.T) {
	in := "---\nno closing delimiter\nBody\n"
	if got := stripLeadingFrontMatter(in); got != in {
		t.Errorf("stripLeadingFrontMatter(unterminated) = %q, want unchanged %q", got, in)
	}
}

func TestStripLeadingFrontMatterNoFrontMatter(t *testing.T) {
	in := "Just a normal note.\n"
	if got := stripLeadingFrontMatter(in); got != in {
		t.Errorf("stripLeadingFrontMatter(no front matter) = %q, want unchanged %q", got, in)
	}
}
