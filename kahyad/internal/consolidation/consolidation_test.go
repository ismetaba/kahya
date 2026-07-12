package consolidation

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kahya/kahyad/internal/backup"
	"kahya/kahyad/internal/indexer"
	"kahya/kahyad/internal/mlx"
)

// --- shared test fakes ---

type fakeNotifier struct {
	sent []string
}

func (f *fakeNotifier) SendNotification(ctx context.Context, traceID, text string) bool {
	f.sent = append(f.sent, text)
	return true
}

type fakeReindexer struct {
	calls int
}

func (f *fakeReindexer) Reindex(ctx context.Context, traceID string, full, reEmbed bool) (indexer.Result, error) {
	f.calls++
	return indexer.Result{}, nil
}

type fakePusher struct {
	calls int
}

func (f *fakePusher) Run(ctx context.Context, traceID string) error {
	f.calls++
	return nil
}

// echoSession returns a SessionFunc that records every call's file map
// (into *received) and echoes every file back UNCHANGED - the default
// "consolidation found nothing to change" stand-in.
func echoSession(received *[]map[string]string) SessionFunc {
	return func(ctx context.Context, traceID string, files map[string]string) (map[string]string, error) {
		cp := make(map[string]string, len(files))
		for k, v := range files {
			cp[k] = v
		}
		*received = append(*received, cp)
		out := make(map[string]string, len(files))
		for k, v := range files {
			out[k] = v
		}
		return out, nil
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// gitLogAuthorsAndSubjects returns kahyaDir's `git log --format=%an <%ae>|%s`
// output, oldest-first, as (author, subject) pairs.
func gitLogAuthorsAndSubjects(t *testing.T, kahyaDir string) [][2]string {
	t.Helper()
	out := runGit(t, kahyaDir, "log", "--reverse", "--format=%an <%ae>|%s")
	var rows [][2]string
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 2)
		if len(parts) != 2 {
			continue
		}
		rows = append(rows, [2]string{parts[0], parts[1]})
	}
	return rows
}

func fixedNow(t *testing.T) func() time.Time {
	return func() time.Time { return time.Date(2026, 7, 12, 3, 0, 0, 0, time.UTC) }
}

// --- (a) commit discipline + pending diff + approve ---

func TestRunProducesPendingDiffAndApproveShowsCommitDiscipline(t *testing.T) {
	repo := initKahyaRepo(t)
	// A same-day dirty edit BEFORE the run (Run's own step 1 must commit
	// this as author=user first).
	writeFile(t, repo, "memory/note.md", "line one\nline two DIRTY EDIT\nline three\n")

	var cloudReceived []map[string]string
	cloud := SessionFunc(func(ctx context.Context, traceID string, files map[string]string) (map[string]string, error) {
		cloudReceived = append(cloudReceived, files)
		out := map[string]string{}
		for k, v := range files {
			out[k] = v + "\nCONSOLIDATED MARKER\n"
		}
		return out, nil
	})

	logger := &fakeEventStore{}
	notifier := &fakeNotifier{}
	c := &Consolidator{
		Cfg:         Config{KahyaDir: repo, MemoryDir: filepath.Join(repo, "memory"), WorktreeParentDir: t.TempDir(), Now: fixedNow(t)},
		Git:         backup.NewExecGitRunner(),
		Cloud:       cloud,
		Notifier:    notifier,
		EventLogger: logger,
		EventReader: logger,
	}

	if err := c.Run(context.Background(), "trace-run-1"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(cloudReceived) != 1 {
		t.Fatalf("cloud session called %d times, want 1", len(cloudReceived))
	}

	diff, found, err := c.Show(context.Background())
	if err != nil {
		t.Fatalf("Show() error = %v", err)
	}
	if !found {
		t.Fatal("Show() found = false, want a pending suggestion")
	}
	if !strings.Contains(diff, "CONSOLIDATED MARKER") {
		t.Fatalf("Show() diff missing expected content:\n%s", diff)
	}
	if len(notifier.sent) != 1 || notifier.sent[0] != MsgSuggestionReady {
		t.Fatalf("notifier.sent = %+v, want exactly [%q]", notifier.sent, MsgSuggestionReady)
	}

	if err := c.Approve(context.Background(), "trace-approve-1"); err != nil {
		t.Fatalf("Approve() error = %v", err)
	}

	rows := gitLogAuthorsAndSubjects(t, repo)
	if len(rows) < 3 {
		t.Fatalf("git log = %+v, want >= 3 commits (seed, user pre-commit, kahyad commit)", rows)
	}
	last := rows[len(rows)-1]
	prev := rows[len(rows)-2]
	if last[0] != KahyaCommitAuthor {
		t.Errorf("last commit author = %q, want %q (subject=%q)", last[0], KahyaCommitAuthor, last[1])
	}
	if last[1] != "nightly consolidation" {
		t.Errorf("last commit subject = %q, want %q", last[1], "nightly consolidation")
	}
	if prev[0] != UserCommitAuthor {
		t.Errorf("preceding commit author = %q, want %q (the dirty-tree pre-commit)", prev[0], UserCommitAuthor)
	}
	if prev[1] != UserPreCommitMessage {
		t.Errorf("preceding commit subject = %q, want %q", prev[1], UserPreCommitMessage)
	}

	if got := readFile(t, filepath.Join(repo, "memory", "note.md")); !strings.Contains(got, "CONSOLIDATED MARKER") {
		t.Fatalf("main's note.md after approve = %q, want the merged content", got)
	}

	// FindPending must be empty again after approval.
	if p, err := FindPending(context.Background(), logger); err != nil || p != nil {
		t.Fatalf("FindPending() after approve = (%+v, %v), want (nil, nil)", p, err)
	}
}

// --- (b) USER-EDIT-WINS across a full Run+Approve cycle ---

func TestRunUserEditWinsFullCycle(t *testing.T) {
	repo := initKahyaRepo(t)
	git := backup.NewExecGitRunner()

	// Anchor ("unchanged middle") lines separate the user-touched hunk
	// from the independently-changed one, exactly like diff_test.go's own
	// TestApplyUserEditWinsProtectsTouchedLine - a hunk with no equal
	// line anywhere between two edited regions is, by definition, ONE
	// hunk, and user_edit winning it would revert BOTH regions (see that
	// test's sibling, TestApplyUserEditWinsWholeFileHunkAllReverted).
	//
	// The BASELINE commit (below) already has every line except line 2 in
	// its final shape - the user's OWN edit (the second commit) then
	// changes ONLY line 2, so ComputeUserTouchedLines' git diff correctly
	// marks JUST that one line touched, rather than the whole file (which
	// is what would happen if the user's commit replaced content
	// unrelated to what came before it - a full-file replacement IS a
	// full-file diff from git's point of view, not a "one line edited"
	// diff).
	baseline := "unchanged intro\n" +
		"line two ORIGINAL\n" +
		"unchanged middle\n" +
		"line four original\n" +
		"unchanged outro\n"
	writeFile(t, repo, "memory/note.md", baseline)
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-m", "baseline (yesterday)")

	original := "unchanged intro\n" +
		"USER EDITED LINE TWO\n" +
		"unchanged middle\n" +
		"line four original\n" +
		"unchanged outro\n"
	writeFile(t, repo, "memory/note.md", original)
	// A same-day user edit, committed BEFORE Run (simulating an earlier
	// memory_write today, not the dirty-tree case).
	if err := CommitAll(context.Background(), git, repo, UserCommitAuthor, UserPreCommitMessage); err != nil {
		t.Fatalf("seed user commit: %v", err)
	}

	cloud := SessionFunc(func(ctx context.Context, traceID string, files map[string]string) (map[string]string, error) {
		out := map[string]string{}
		for k, v := range files {
			// Propose changing EVERY edited line, including the
			// user-touched one - user_edit must still win on line 2 while
			// line 4's change (untouched by the user) is accepted.
			if k == "note.md" && v == original {
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
	if err := c.Approve(context.Background(), "trace-2"); err != nil {
		t.Fatalf("Approve() error = %v", err)
	}

	got := readFile(t, filepath.Join(repo, "memory", "note.md"))
	want := "unchanged intro\n" +
		"USER EDITED LINE TWO\n" +
		"unchanged middle\n" +
		"MODEL CHANGES LINE FOUR\n" +
		"unchanged outro\n"
	if got != want {
		t.Fatalf("note.md after approve =\n%q\nwant\n%q", got, want)
	}
}

// --- (c) secret-lane ordering invariant ---

func TestRunSecretLaneFileNeverInCloudEnvelope(t *testing.T) {
	repo := initKahyaRepo(t)
	writeFile(t, repo, "memory/finans-2026.md", "TR330006100519786457841326 iban bilgisi")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-m", "seed finans note")

	var cloudReceived []map[string]string
	var localReceived []map[string]string
	cloud := echoSession(&cloudReceived)
	local := SessionFunc(func(ctx context.Context, traceID string, files map[string]string) (map[string]string, error) {
		cp := make(map[string]string, len(files))
		for k, v := range files {
			cp[k] = v
		}
		localReceived = append(localReceived, cp)
		out := make(map[string]string, len(files))
		for k, v := range files {
			out[k] = v + "\nlocal lane touched this\n"
		}
		return out, nil
	})

	logger := &fakeEventStore{}
	c := &Consolidator{
		Cfg: Config{
			KahyaDir: repo, MemoryDir: filepath.Join(repo, "memory"),
			SecretLaneGlobs:   []string{filepath.Join(repo, "memory", "finans*.md")},
			WorktreeParentDir: t.TempDir(), Now: fixedNow(t),
		},
		Git: backup.NewExecGitRunner(), Cloud: cloud, Local: local,
		EventLogger: logger, EventReader: logger,
	}
	if err := c.Run(context.Background(), "trace-1"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	for _, call := range cloudReceived {
		if _, ok := call["finans-2026.md"]; ok {
			t.Fatalf("secret-lane file finans-2026.md appeared in a CLOUD-lane call: %+v", call)
		}
	}
	sawSecretLocally := false
	for _, call := range localReceived {
		if _, ok := call["finans-2026.md"]; ok {
			sawSecretLocally = true
		}
	}
	if !sawSecretLocally {
		t.Fatal("secret-lane file finans-2026.md never reached the LOCAL lane at all")
	}
}

// --- (d) local-model-unavailable fail-closed skip, never a cloud fallback ---

func TestRunLocalUnavailableSkipsFailClosedNeverCloudFallback(t *testing.T) {
	repo := initKahyaRepo(t)
	secretContent := "TR330006100519786457841326 iban bilgisi"
	writeFile(t, repo, "memory/finans-2026.md", secretContent)
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-m", "seed finans note")

	var cloudReceived []map[string]string
	cloud := SessionFunc(func(ctx context.Context, traceID string, files map[string]string) (map[string]string, error) {
		cp := make(map[string]string, len(files))
		for k, v := range files {
			cp[k] = v
		}
		cloudReceived = append(cloudReceived, cp)
		out := make(map[string]string, len(files))
		for k, v := range files {
			// Actually change the non-secret file so a diff exists even
			// though the secret-lane file below is skipped entirely.
			out[k] = v + "\ncloud lane changed this\n"
		}
		return out, nil
	})
	local := SessionFunc(func(ctx context.Context, traceID string, files map[string]string) (map[string]string, error) {
		return nil, mlx.ErrLocalModelUnavailable
	})

	logger := &fakeEventStore{}
	notifier := &fakeNotifier{}
	c := &Consolidator{
		Cfg: Config{
			KahyaDir: repo, MemoryDir: filepath.Join(repo, "memory"),
			SecretLaneGlobs:   []string{filepath.Join(repo, "memory", "finans*.md")},
			WorktreeParentDir: t.TempDir(), Now: fixedNow(t),
		},
		Git: backup.NewExecGitRunner(), Cloud: cloud, Local: local,
		Notifier: notifier, EventLogger: logger, EventReader: logger,
	}
	if err := c.Run(context.Background(), "trace-1"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	for _, call := range cloudReceived {
		if _, ok := call["finans-2026.md"]; ok {
			t.Fatalf("secret-lane file leaked into the cloud lane after a local-unavailable failure: %+v", call)
		}
	}

	foundNotice := false
	for _, msg := range notifier.sent {
		if msg == MsgLocalSkipped {
			foundNotice = true
		}
	}
	if !foundNotice {
		t.Fatalf("notifier.sent = %+v, want the byte-exact %q notice", notifier.sent, MsgLocalSkipped)
	}

	// The secret file itself must be untouched (skipped, not merged with
	// garbage) - approve and check main's content is unchanged.
	if err := c.Approve(context.Background(), "trace-2"); err != nil {
		t.Fatalf("Approve() error = %v", err)
	}
	if got := readFile(t, filepath.Join(repo, "memory", "finans-2026.md")); got != secretContent {
		t.Fatalf("finans-2026.md after approve = %q, want unchanged %q", got, secretContent)
	}
}

// --- (e) write boundary: zero writes outside the worktree; reindex only after approve ---

func TestRunWriteBoundaryAndReindexOnlyAfterApprove(t *testing.T) {
	repo := initKahyaRepo(t)
	before := readFile(t, filepath.Join(repo, "memory", "note.md"))

	cloud := SessionFunc(func(ctx context.Context, traceID string, files map[string]string) (map[string]string, error) {
		out := map[string]string{}
		for k, v := range files {
			out[k] = v + "\nchanged by cloud lane\n"
		}
		return out, nil
	})

	logger := &fakeEventStore{}
	reindexer := &fakeReindexer{}
	c := &Consolidator{
		// HotWindow is deliberately left nil: the markdown/git pipeline
		// this test exercises has NO brain.db-touching collaborator at
		// all (Cloud/Local/Git/Notifier/EventLogger/EventReader/Reindexer/
		// Pusher - none of these can open episodes/chunks/facts; only
		// EventLogger/EventReader touch brain.db's events table, the one
		// HANDOFF §5 carve-out).
		Cfg:         Config{KahyaDir: repo, MemoryDir: filepath.Join(repo, "memory"), WorktreeParentDir: t.TempDir(), Now: fixedNow(t)},
		Git:         backup.NewExecGitRunner(),
		Cloud:       cloud,
		EventLogger: logger,
		EventReader: logger,
		Reindexer:   reindexer,
	}

	if err := c.Run(context.Background(), "trace-1"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	// main's own working tree must be untouched by Run() - the rewrite
	// only ever lands on the (now-removed) worktree's branch.
	if got := readFile(t, filepath.Join(repo, "memory", "note.md")); got != before {
		t.Fatalf("main's note.md changed during Run() (write-boundary violation): got %q, want unchanged %q", got, before)
	}
	if reindexer.calls != 0 {
		t.Fatalf("Reindexer was called %d times during Run(), want 0 (reindex must only happen after approve)", reindexer.calls)
	}

	if err := c.Approve(context.Background(), "trace-2"); err != nil {
		t.Fatalf("Approve() error = %v", err)
	}
	if reindexer.calls != 1 {
		t.Fatalf("Reindexer was called %d times after approve, want exactly 1", reindexer.calls)
	}
	if got := readFile(t, filepath.Join(repo, "memory", "note.md")); got == before {
		t.Fatal("main's note.md unchanged after approve - the merge did not actually land")
	}
}

// --- (f) supersede: a second run while a suggestion is pending ---

func TestRunSupersedesStalePendingAndRegeneratesAgainstCurrentMain(t *testing.T) {
	repo := initKahyaRepo(t)
	git := backup.NewExecGitRunner()

	cloud1 := SessionFunc(func(ctx context.Context, traceID string, files map[string]string) (map[string]string, error) {
		out := map[string]string{}
		for k, v := range files {
			out[k] = v + "\nFIRST RUN MARKER\n"
		}
		return out, nil
	})
	logger := &fakeEventStore{}
	c := &Consolidator{
		Cfg:         Config{KahyaDir: repo, MemoryDir: filepath.Join(repo, "memory"), WorktreeParentDir: t.TempDir(), Now: fixedNow(t)},
		Git:         git,
		Cloud:       cloud1,
		EventLogger: logger,
		EventReader: logger,
	}
	if err := c.Run(context.Background(), "trace-first"); err != nil {
		t.Fatalf("first Run() error = %v", err)
	}
	firstPending, err := FindPending(context.Background(), logger)
	if err != nil || firstPending == nil {
		t.Fatalf("FindPending() after first run = (%+v, %v), want a pending suggestion", firstPending, err)
	}
	firstBranch := firstPending.Branch

	// Advance main directly (simulating the user editing memory during the
	// day, between the two nightly runs) so the SECOND run's diff must
	// reflect current main, not the first run's stale base.
	writeFile(t, repo, "memory/second-note.md", "brand new note added after the first run\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-m", "user edits before consolidation", "--author", UserCommitAuthor)

	cloud2 := SessionFunc(func(ctx context.Context, traceID string, files map[string]string) (map[string]string, error) {
		out := map[string]string{}
		for k, v := range files {
			out[k] = v + "\nSECOND RUN MARKER\n"
		}
		return out, nil
	})
	c.Cloud = cloud2

	if err := c.Run(context.Background(), "trace-second"); err != nil {
		t.Fatalf("second Run() error = %v", err)
	}

	branchList := runGit(t, repo, "branch", "--list", ConsolidationBranchPrefix+"*")
	if strings.Count(branchList, ConsolidationBranchPrefix) != 1 {
		t.Fatalf("branch --list = %q, want exactly ONE consolidation branch (the stale one must be deleted)", branchList)
	}
	if strings.Contains(branchList, firstBranch) && firstBranch != c.Cfg.Now().Format("kahya/consolidation-20060102") {
		// firstBranch and the second run's branch share the same name
		// (same fixed Now()), so this check only matters if a test ever
		// varies Now() between runs - kept here defensively.
	}

	secondPending, err := FindPending(context.Background(), logger)
	if err != nil || secondPending == nil {
		t.Fatalf("FindPending() after second run = (%+v, %v), want a fresh pending suggestion", secondPending, err)
	}
	if secondPending.TraceID != "trace-second" {
		t.Fatalf("pending.TraceID = %q, want trace-second", secondPending.TraceID)
	}

	// The ledger must record consolidation.superseded carrying BOTH
	// trace_ids.
	supersededRows, err := logger.ListEventsByKind(context.Background(), EventSuperseded)
	if err != nil || len(supersededRows) != 1 {
		t.Fatalf("ListEventsByKind(superseded) = (%+v, %v), want exactly one row", supersededRows, err)
	}
	if !strings.Contains(supersededRows[0].Payload, "trace-first") || !strings.Contains(supersededRows[0].Payload, "trace-second") {
		t.Fatalf("superseded payload = %q, want BOTH trace-first and trace-second", supersededRows[0].Payload)
	}

	// The fresh diff must reflect the SECOND run's changes AND the file
	// added to main in between - the stale first diff is never merged.
	diff, found, err := c.Show(context.Background())
	if err != nil || !found {
		t.Fatalf("Show() = (%q, %v, %v)", diff, found, err)
	}
	if strings.Contains(diff, "FIRST RUN MARKER") {
		t.Fatalf("Show() diff still contains the STALE first run's marker:\n%s", diff)
	}
	if !strings.Contains(diff, "SECOND RUN MARKER") {
		t.Fatalf("Show() diff missing the second run's marker:\n%s", diff)
	}
}

// --- (g) auto-commit guard ---

func TestRunAutoCommitRefusedWithoutEvalMiniPass(t *testing.T) {
	repo := initKahyaRepo(t)
	cloud := SessionFunc(func(ctx context.Context, traceID string, files map[string]string) (map[string]string, error) {
		out := map[string]string{}
		for k, v := range files {
			out[k] = v + "\nchanged\n"
		}
		return out, nil
	})
	logger := &fakeEventStore{}
	c := &Consolidator{
		Cfg:         Config{KahyaDir: repo, MemoryDir: filepath.Join(repo, "memory"), WorktreeParentDir: t.TempDir(), Now: fixedNow(t), AutoCommit: true},
		Git:         backup.NewExecGitRunner(),
		Cloud:       cloud,
		EventLogger: logger,
		EventReader: logger,
	}
	if err := c.Run(context.Background(), "trace-1"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	// Without an eval.mini.pass event, auto_commit:true must NOT merge -
	// the run stays in suggestion mode.
	pending, err := FindPending(context.Background(), logger)
	if err != nil || pending == nil {
		t.Fatalf("FindPending() = (%+v, %v), want a pending suggestion (auto-commit must have been refused)", pending, err)
	}

	refusedRows, err := logger.ListEventsByKind(context.Background(), EventAutoCommitRefused)
	if err != nil || len(refusedRows) == 0 {
		t.Fatalf("ListEventsByKind(auto_commit_refused) = (%+v, %v), want at least one row", refusedRows, err)
	}
}

func TestRunAutoCommitProceedsWithRecentEvalMiniPass(t *testing.T) {
	repo := initKahyaRepo(t)
	cloud := SessionFunc(func(ctx context.Context, traceID string, files map[string]string) (map[string]string, error) {
		out := map[string]string{}
		for k, v := range files {
			out[k] = v + "\nchanged\n"
		}
		return out, nil
	})
	logger := &fakeEventStore{}
	if err := logger.LogEvent(context.Background(), "eval-trace", EventEvalMiniPass, map[string]any{"ok": true}); err != nil {
		t.Fatal(err)
	}
	reindexer := &fakeReindexer{}
	c := &Consolidator{
		Cfg:         Config{KahyaDir: repo, MemoryDir: filepath.Join(repo, "memory"), WorktreeParentDir: t.TempDir(), Now: fixedNow(t), AutoCommit: true},
		Git:         backup.NewExecGitRunner(),
		Cloud:       cloud,
		EventLogger: logger,
		EventReader: logger,
		Reindexer:   reindexer,
	}
	if err := c.Run(context.Background(), "trace-1"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	pending, err := FindPending(context.Background(), logger)
	if err != nil {
		t.Fatalf("FindPending() error = %v", err)
	}
	if pending != nil {
		t.Fatalf("FindPending() = %+v, want nil (auto-commit should have merged directly)", pending)
	}
	if reindexer.calls != 1 {
		t.Fatalf("Reindexer.calls = %d, want 1 (auto-mode merge also triggers reindex)", reindexer.calls)
	}
}

// --- (h) push after approve ---

func TestApprovePushesToRemote(t *testing.T) {
	repo := initKahyaRepo(t)
	git := backup.NewExecGitRunner()
	remoteDir := t.TempDir()
	runGit(t, remoteDir, "init", "--bare")
	runGit(t, repo, "remote", "add", "origin", "file://"+remoteDir)
	runGit(t, repo, "push", "-u", "origin", "main")

	cloud := SessionFunc(func(ctx context.Context, traceID string, files map[string]string) (map[string]string, error) {
		out := map[string]string{}
		for k, v := range files {
			out[k] = v + "\nchanged\n"
		}
		return out, nil
	})
	logger := &fakeEventStore{}
	pusher := backup.NewPusher(backup.NewExecGitRunner(), nil, repo)
	c := &Consolidator{
		Cfg:         Config{KahyaDir: repo, MemoryDir: filepath.Join(repo, "memory"), WorktreeParentDir: t.TempDir(), Now: fixedNow(t)},
		Git:         git,
		Cloud:       cloud,
		EventLogger: logger,
		EventReader: logger,
		Pusher:      pusher,
	}
	if err := c.Run(context.Background(), "trace-1"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if err := c.Approve(context.Background(), "trace-2"); err != nil {
		t.Fatalf("Approve() error = %v", err)
	}

	// Acceptance criterion, verbatim: `git -C ~/Kahya log origin/main..main`
	// is empty (the nightly push ran).
	out := runGit(t, repo, "log", "origin/main..main", "--format=%H")
	if strings.TrimSpace(out) != "" {
		t.Fatalf("git log origin/main..main = %q, want empty (push after approve did not run)", out)
	}
}

// --- (i) rejection ---

func TestRejectDeletesBranchAndLedgersRejection(t *testing.T) {
	repo := initKahyaRepo(t)
	before := readFile(t, filepath.Join(repo, "memory", "note.md"))
	cloud := SessionFunc(func(ctx context.Context, traceID string, files map[string]string) (map[string]string, error) {
		out := map[string]string{}
		for k, v := range files {
			out[k] = v + "\nrejected content\n"
		}
		return out, nil
	})
	logger := &fakeEventStore{}
	c := &Consolidator{
		Cfg:         Config{KahyaDir: repo, MemoryDir: filepath.Join(repo, "memory"), WorktreeParentDir: t.TempDir(), Now: fixedNow(t)},
		Git:         backup.NewExecGitRunner(),
		Cloud:       cloud,
		EventLogger: logger,
		EventReader: logger,
	}
	if err := c.Run(context.Background(), "trace-1"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	pending, err := FindPending(context.Background(), logger)
	if err != nil || pending == nil {
		t.Fatalf("FindPending() = (%+v, %v), want a pending suggestion", pending, err)
	}

	if err := c.Reject(context.Background(), "trace-2"); err != nil {
		t.Fatalf("Reject() error = %v", err)
	}

	branchList := runGit(t, repo, "branch", "--list", ConsolidationBranchPrefix+"*")
	if strings.TrimSpace(branchList) != "" {
		t.Fatalf("branch --list after reject = %q, want empty (branch must be deleted)", branchList)
	}
	if got := readFile(t, filepath.Join(repo, "memory", "note.md")); got != before {
		t.Fatalf("note.md changed after reject: got %q, want unchanged %q", got, before)
	}
	rejectedRows, err := logger.ListEventsByKind(context.Background(), EventRejected)
	if err != nil || len(rejectedRows) != 1 {
		t.Fatalf("ListEventsByKind(rejected) = (%+v, %v), want exactly one row", rejectedRows, err)
	}

	if _, _, err := c.Show(context.Background()); err != nil {
		t.Fatalf("Show() error after reject = %v", err)
	}
	if found2, err := func() (bool, error) { _, f, e := c.Show(context.Background()); return f, e }(); err != nil || found2 {
		t.Fatalf("Show() found = %v after reject, want false", found2)
	}
}

func TestApproveWithNoPendingReturnsErrNoPending(t *testing.T) {
	repo := initKahyaRepo(t)
	logger := &fakeEventStore{}
	c := &Consolidator{
		Cfg:         Config{KahyaDir: repo, MemoryDir: filepath.Join(repo, "memory")},
		Git:         backup.NewExecGitRunner(),
		EventLogger: logger, EventReader: logger,
	}
	if err := c.Approve(context.Background(), "trace-1"); err != ErrNoPending {
		t.Fatalf("Approve() error = %v, want ErrNoPending", err)
	}
	if err := c.Reject(context.Background(), "trace-1"); err != ErrNoPending {
		t.Fatalf("Reject() error = %v, want ErrNoPending", err)
	}
}
