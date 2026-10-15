package consolidation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"kahya/kahyad/internal/backup"
	"kahya/kahyad/internal/mlx"
)

// FIX-B-5 phase 1: each lane is consolidated in batches under a token
// budget instead of one whole-lane JSON call.

// recordingTruncatingSession is a cloud/local stand-in whose transport cuts
// any response above maxOut bytes - the shape of a real max_tokens cut. It
// appends marker to every file it is given (so the night has a diff),
// records every call's input, and parses its own (possibly cut) response
// with the production parser.
type recordingTruncatingSession struct {
	mu     sync.Mutex
	maxOut int
	marker string
	lane   string
	calls  []map[string]string
}

func (s *recordingTruncatingSession) Consolidate(ctx context.Context, traceID string, files map[string]string) (map[string]string, error) {
	s.mu.Lock()
	cp := make(map[string]string, len(files))
	for k, v := range files {
		cp[k] = v
	}
	s.calls = append(s.calls, cp)
	s.mu.Unlock()
	out := make(map[string]string, len(files))
	for k, v := range files {
		out[k] = v + s.marker
	}
	raw, _ := json.Marshal(map[string]any{"files": out})
	if s.maxOut > 0 && len(raw) > s.maxOut {
		raw = raw[:s.maxOut]
	}
	return parseRewriteResponse(string(raw), s.lane, files)
}

// seedCorpus writes n notes of size*"satır içeriği\n" under memory/topicNN/
// and commits them.
func seedCorpus(t *testing.T, repo string, n, lines int) map[string]string {
	t.Helper()
	files := map[string]string{}
	for i := 0; i < n; i++ {
		rel := fmt.Sprintf("topic%02d/note.md", i)
		content := strings.Repeat("satır içeriği\n", lines)
		writeFile(t, repo, "memory/"+rel, content)
		files[rel] = content
	}
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-m", "seed corpus")
	return files
}

func newBatchConsolidator(t *testing.T, repo string, cloud, local Session, cfg func(*Config)) (*Consolidator, *fakeEventStore, *fakeNotifier) {
	t.Helper()
	logger := &fakeEventStore{}
	notifier := &fakeNotifier{}
	c := &Consolidator{
		Cfg: Config{KahyaDir: repo, MemoryDir: filepath.Join(repo, "memory"), WorktreeParentDir: t.TempDir(), Now: fixedNow(t)},
		Git: backup.NewExecGitRunner(), Cloud: cloud, Local: local,
		Notifier: notifier, EventLogger: logger, EventReader: logger,
	}
	if cfg != nil {
		cfg(&c.Cfg)
	}
	return c, logger, notifier
}

// branchFile reads memory/<rel> on tonight's consolidation branch.
func branchFile(t *testing.T, repo, rel string) string {
	t.Helper()
	return runGit(t, repo, "show", tonightsBranch+":memory/"+rel)
}

func payloadsOfKind(rows []fakeEventRow, kind string) []map[string]any {
	var out []map[string]any
	for _, r := range rows {
		if r.kind == kind {
			out = append(out, r.payload)
		}
	}
	return out
}

func TestOrderCandidatesChangedFirstThenNeighboursThenMtime(t *testing.T) {
	files := map[string]string{
		"b/changed.md":    "x",
		"a/changed.md":    "x",
		"a/neighbour.md":  "x",
		"b/neighbour.md":  "x",
		"c/old.md":        "x",
		"c/new.md":        "x",
		"d/newest.md":     "x",
		"d/tie-b.md":      "x",
		"d/tie-a.md":      "x",
		"e/huge.md":       strings.Repeat("x", 100),
		"a/huge-chg.md":   strings.Repeat("x", 100),
		"kök.md":          "x",
		"f/değişti.md":    "x",
		"f/komşu-not.md":  "x",
		"g/unrelated.md":  "x",
		"g/sub/nested.md": "x",
	}
	changed := map[string]bool{
		NormalizePathKey("a/changed.md"):    true,
		NormalizePathKey("b/changed.md"):    true,
		NormalizePathKey("a/huge-chg.md"):   true,
		NormalizePathKey("f/değişti.md"):    true,
		NormalizePathKey("g/sub/nested.md"): true,
	}
	mtimes := map[string]int64{
		"c/old.md": 100, "c/new.md": 300, "d/newest.md": 500, "d/tie-b.md": 200, "d/tie-a.md": 200,
		"kök.md": 50, "g/unrelated.md": 400,
	}
	ordered, skipped := orderCandidates(files, nil, changed, func(rel string) int64 { return mtimes[rel] }, 50)
	want := []string{
		// changed, by (dir, name)
		"a/changed.md", "b/changed.md", "f/değişti.md", "g/sub/nested.md",
		// same-directory neighbours of changed files, by (dir, name)
		"a/neighbour.md", "b/neighbour.md", "f/komşu-not.md",
		// the rest: commit time desc, ties by path ("g/unrelated.md" is in
		// g/, not in g/sub/, so it is not a neighbour)
		"d/newest.md", "g/unrelated.md", "c/new.md", "d/tie-a.md", "d/tie-b.md", "c/old.md", "kök.md",
	}
	if !reflect.DeepEqual(ordered, want) {
		t.Fatalf("ordered =\n%q\nwant\n%q", ordered, want)
	}
	if wantSkipped := []string{"a/huge-chg.md", "e/huge.md"}; !reflect.DeepEqual(skipped, wantSkipped) {
		t.Fatalf("skipped = %q, want %q (above max_file_bytes, changed or not)", skipped, wantSkipped)
	}
	// No changed set: everything by commit time.
	ordered, _ = orderCandidates(map[string]string{"x/a.md": "", "y/b.md": "", "z/c.md": ""}, nil, nil,
		func(rel string) int64 { return map[string]int64{"x/a.md": 1, "y/b.md": 3, "z/c.md": 2}[rel] }, 0)
	if want := []string{"y/b.md", "z/c.md", "x/a.md"}; !reflect.DeepEqual(ordered, want) {
		t.Fatalf("ordered without a changed set = %q, want %q", ordered, want)
	}
}

func TestBuildBatchesRespectsBudgetNeverSplitsFile(t *testing.T) {
	files := map[string]string{}
	var ordered []string
	for i := 0; i < 30; i++ {
		rel := fmt.Sprintf("n%02d.md", i)
		files[rel] = strings.Repeat("ğ", 50*(i%7+1))
		ordered = append(ordered, rel)
	}
	files["big.md"] = strings.Repeat("ş", 5000)
	ordered = append(ordered[:10], append([]string{"big.md"}, ordered[10:]...)...)
	const budget = 1200
	batches, skipped := buildBatches(ordered, files, budget)
	if !reflect.DeepEqual(skipped, []string{"big.md"}) {
		t.Fatalf("skipped = %q, want [big.md] (one file above the budget)", skipped)
	}
	if len(batches) < 2 {
		t.Fatalf("batches = %d, want the files split into several", len(batches))
	}
	var seen []string
	for i, b := range batches {
		if b.EstTokens > budget {
			t.Errorf("batch %d EstTokens = %d > budget %d", i, b.EstTokens, budget)
		}
		if got := estimateFilesTokens(b.Files); got != b.EstTokens {
			t.Errorf("batch %d EstTokens = %d, estimateFilesTokens = %d", i, b.EstTokens, got)
		}
		for _, p := range b.paths() {
			if b.Files[p] != files[p] {
				t.Errorf("batch %d file %s content changed (a file is never split)", i, p)
			}
		}
		seen = append(seen, b.paths()...)
		if i+1 < len(batches) {
			// greedy: the next batch's first file did not fit into this one
			next := batches[i+1]
			first := ""
			for _, p := range ordered {
				if _, ok := next.Files[p]; ok {
					first = p
					break
				}
			}
			if b.EstTokens+estimateFileTokens(first, files[first]) <= budget {
				t.Errorf("batch %d closed although %s still fit", i, first)
			}
		}
	}
	sort.Strings(seen)
	var want []string
	for _, p := range ordered {
		if p != "big.md" {
			want = append(want, p)
		}
	}
	sort.Strings(want)
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("batched files = %q, want every file but big.md exactly once", seen)
	}
	// Order is kept: concatenating the batches gives ordered minus skipped.
	var concat []string
	for _, b := range batches {
		var inBatch []string
		for _, p := range ordered {
			if _, ok := b.Files[p]; ok {
				inBatch = append(inBatch, p)
			}
		}
		concat = append(concat, inBatch...)
	}
	var orderedNoBig []string
	for _, p := range ordered {
		if p != "big.md" {
			orderedNoBig = append(orderedNoBig, p)
		}
	}
	if !reflect.DeepEqual(concat, orderedNoBig) {
		t.Fatalf("batches do not follow the candidate order:\n%q\nwant\n%q", concat, orderedNoBig)
	}
}

func TestChangedSinceLastApprovedUsesMergeCommit(t *testing.T) {
	repo := initKahyaRepo(t)
	writeFile(t, repo, "memory/günlük.md", "a\n")
	writeFile(t, repo, "memory/konu/b.md", "b\n")
	writeFile(t, repo, "outside.txt", "o\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-m", "seed")
	git := backup.NewExecGitRunner()
	ctx := context.Background()

	store := &fakeEventStore{}
	if _, ok, err := changedSinceLastApproved(ctx, git, store, repo, "memory"); err != nil || ok {
		t.Fatalf("no approved row: ok=%v err=%v, want ok=false (everything changed)", ok, err)
	}

	merge := runGit(t, repo, "rev-parse", "HEAD")
	if err := LedgerApproved(ctx, store, "trace-approve", "trace-run", merge); err != nil {
		t.Fatal(err)
	}
	writeFile(t, repo, "memory/günlük.md", "a\nUSER\n")
	writeFile(t, repo, "outside.txt", "changed outside memory\n")
	runGit(t, repo, "commit", "-am", "user edit")

	changed, ok, err := changedSinceLastApproved(ctx, git, store, repo, "memory")
	if err != nil || !ok {
		t.Fatalf("changedSinceLastApproved = (%v, %v, %v), want ok", changed, ok, err)
	}
	if want := map[string]bool{NormalizePathKey("günlük.md"): true}; !reflect.DeepEqual(changed, want) {
		t.Fatalf("changed = %v, want only günlük.md (memory-relative, NFC key)", changed)
	}

	// The latest approved row wins; a commit that no longer exists (history
	// rewritten) or a malformed merge_commit means "everything changed".
	if err := LedgerApproved(ctx, store, "trace-approve-2", "trace-run-2", strings.Repeat("ab", 20)); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := changedSinceLastApproved(ctx, git, store, repo, "memory"); err != nil || ok {
		t.Fatalf("gone merge commit: ok=%v err=%v, want ok=false", ok, err)
	}
	if err := store.LogEvent(ctx, "trace-approve-3", EventApproved, map[string]any{"trace_id": "x", "merge_commit": "--output=/tmp/x"}); err != nil {
		t.Fatal(err)
	}
	// The malformed row is skipped and the previous (gone) one is used.
	if _, ok, err := changedSinceLastApproved(ctx, git, store, repo, "memory"); err != nil || ok {
		t.Fatalf("malformed merge_commit: ok=%v err=%v, want ok=false", ok, err)
	}
}

// countingGit counts git invocations by subcommand.
type countingGit struct {
	backup.GitRunner
	mu    sync.Mutex
	calls map[string]int
}

func (g *countingGit) Run(ctx context.Context, dir string, args ...string) (string, string, error) {
	g.mu.Lock()
	if g.calls == nil {
		g.calls = map[string]int{}
	}
	if len(args) > 0 {
		g.calls[args[0]]++
	}
	g.mu.Unlock()
	return g.GitRunner.Run(ctx, dir, args...)
}

func TestLastCommitTimesSinglePass(t *testing.T) {
	repo := initKahyaRepo(t)
	commitAt := func(date, msg string) {
		t.Helper()
		runGit(t, repo, "add", "-A")
		cmd := exec.Command("git", "-C", repo, "commit", "-q", "-m", msg)
		cmd.Env = append(os.Environ(), "GIT_COMMITTER_DATE="+date, "GIT_AUTHOR_DATE="+date)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git commit: %v\n%s", err, out)
		}
	}
	writeFile(t, repo, "memory/a.md", "a\n")
	writeFile(t, repo, "memory/alt/günlük.md", "g\n")
	writeFile(t, repo, "outside.md", "o\n")
	commitAt("2026-01-01T00:00:00Z", "one")
	writeFile(t, repo, "memory/a.md", "a2\n")
	writeFile(t, repo, "outside.md", "o2\n")
	commitAt("2026-02-01T00:00:00Z", "two")

	git := &countingGit{GitRunner: backup.NewExecGitRunner()}
	times, err := lastCommitTimes(context.Background(), git, repo, "memory")
	if err != nil {
		t.Fatalf("lastCommitTimes: %v", err)
	}
	if git.calls["log"] != 1 || len(git.calls) != 1 {
		t.Fatalf("git calls = %v, want exactly one git log", git.calls)
	}
	if got, want := times["a.md"], int64(1769904000); got != want {
		t.Errorf("a.md = %d, want %d (its newest commit)", got, want)
	}
	if got, want := times[NormalizePathKey("alt/günlük.md")], int64(1767225600); got != want {
		t.Errorf("alt/günlük.md = %d, want %d", got, want)
	}
	if _, ok := times["outside.md"]; ok {
		t.Errorf("times has a file outside memory/: %v", times)
	}
	if _, ok := times["note.md"]; !ok {
		t.Errorf("times misses the seed note.md: %v", times)
	}
}

// TestRunBatchesCloudLaneWithinBudget is the B-5 repro: a lane whose single
// JSON response would exceed the transport's output cap failed every night;
// now it is split into batches that each fit.
func TestRunBatchesCloudLaneWithinBudget(t *testing.T) {
	repo := initKahyaRepo(t)
	corpus := seedCorpus(t, repo, 40, 200) // 40 notes of ~3.4 KB
	corpus["note.md"] = "line one\nline two\nline three\n"
	cloud := &recordingTruncatingSession{maxOut: 20_000, marker: "\nB5 BATCHED\n", lane: LaneCloud}
	const budget = 4000
	c, logger, _ := newBatchConsolidator(t, repo, cloud, nil, func(cfg *Config) { cfg.CloudBatchTokenBudget = budget })

	if err := c.Run(context.Background(), "trace-b5-batches"); err != nil {
		t.Fatalf("Run() error = %v (one whole-lane call is cut at 20 KB and fails to decode)", err)
	}
	if len(cloud.calls) < 2 {
		t.Fatalf("cloud calls = %d, want the lane split into several batches", len(cloud.calls))
	}
	seen := map[string]int{}
	for i, call := range cloud.calls {
		if est := estimateFilesTokens(call); est > budget {
			t.Errorf("call %d estimate %d > budget %d", i, est, budget)
		}
		for k, v := range call {
			seen[k]++
			if v != corpus[k] {
				t.Errorf("call %d got %s with altered content (batches carry the raw file)", i, k)
			}
		}
	}
	if len(seen) != len(corpus) {
		t.Fatalf("files sent = %d, want all %d", len(seen), len(corpus))
	}
	for k, n := range seen {
		if n != 1 {
			t.Errorf("%s sent %d times, want exactly once", k, n)
		}
	}
	if rows := payloadsOfKind(logger.rows, EventBatchesDeferred); len(rows) != 0 {
		t.Errorf("batches_deferred rows = %v, want none (20 batches fit the default cap)", rows)
	}
	// One branch, one kahyad commit, holding every batch's rewrite.
	for rel, orig := range corpus {
		if got := branchFile(t, repo, rel); got != strings.TrimRight(orig+"\nB5 BATCHED\n", "\n") {
			t.Fatalf("branch %s = %q, want the batch rewrite", rel, got)
		}
	}
	if n := strings.Count(runGit(t, repo, "log", "--format=%an", MainBranch+".."+tonightsBranch), "kahyad"); n != 1 {
		t.Fatalf("kahyad commits on the branch = %d, want exactly 1 (batching stays inside one night)", n)
	}
	if len(payloadsOfKind(logger.rows, EventPending)) != 1 {
		t.Fatal("want exactly one pending row for the night")
	}
}

// TestRunBatchKeyValidationPerBatch: a batch may only rewrite its own files.
// Returning another batch's file (one that IS in the lane) rejects the night
// exactly like a path outside the lane (FIX-P-7, per batch).
func TestRunBatchKeyValidationPerBatch(t *testing.T) {
	repo := initKahyaRepo(t)
	seedCorpus(t, repo, 3, 100)
	var first map[string]string
	calls := 0
	cloud := SessionFunc(func(ctx context.Context, traceID string, files map[string]string) (map[string]string, error) {
		calls++
		out := map[string]string{}
		for k, v := range files {
			out[k] = v + "\nchanged\n"
		}
		if calls == 1 {
			first = files
			return out, nil
		}
		for k := range first {
			out[k] = "SMUGGLED BY BATCH 2\n" // in the lane, but not in this batch
		}
		return out, nil
	})
	c, logger, notifier := newBatchConsolidator(t, repo, cloud, nil, func(cfg *Config) { cfg.CloudBatchTokenBudget = 1000 })
	err := c.Run(context.Background(), "trace-b5-perbatch")
	if !errors.Is(err, ErrRewriteUnknownPath) {
		t.Fatalf("Run() error = %v, want ErrRewriteUnknownPath", err)
	}
	if calls < 2 {
		t.Fatalf("cloud calls = %d, want >= 2", calls)
	}
	if br := runGit(t, repo, "branch", "--list", ConsolidationBranchPrefix+"*"); br != "" {
		t.Errorf("consolidation branch survived: %q", br)
	}
	if out := runGit(t, repo, "log", "--all", "-p", "--format="); strings.Contains(out, "SMUGGLED BY BATCH 2") {
		t.Fatal("a batch's rewrite of another batch's file reached a commit")
	}
	rejected := payloadsOfKind(logger.rows, EventRewriteRejected)
	if len(rejected) != 1 || rejected[0]["lane"] != LaneCloud {
		t.Fatalf("rewrite_rejected rows = %v, want one for lane cloud", rejected)
	}
	if len(payloadsOfKind(logger.rows, EventPending)) != 0 {
		t.Fatal("a pending row was written for a rejected night")
	}
	if len(notifier.sent) != 1 || notifier.sent[0] != MsgRewriteRejected {
		t.Fatalf("notifier.sent = %q, want exactly [%q]", notifier.sent, MsgRewriteRejected)
	}
}

// TestRunLocalUnavailableMidLaneKeepsEarlierBatches: the local model going
// away after batch 1 keeps batch 1's validated rewrite, leaves the rest of
// the secret lane untouched, sends the Turkish notice - and nothing of the
// secret lane ever reaches the cloud lane.
func TestRunLocalUnavailableMidLaneKeepsEarlierBatches(t *testing.T) {
	repo := initKahyaRepo(t)
	secret := map[string]string{
		"finans-a.md": "TR330006100519786457841326 hesap a\n" + strings.Repeat("gelir gider\n", 40),
		"finans-b.md": "TR330006100519786457841326 hesap b\n" + strings.Repeat("gelir gider\n", 40),
		"finans-c.md": "TR330006100519786457841326 hesap c\n" + strings.Repeat("gelir gider\n", 40),
	}
	for rel, content := range secret {
		writeFile(t, repo, "memory/"+rel, content)
	}
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-m", "seed finans notes")

	var cloudReceived []map[string]string
	cloud := SessionFunc(func(ctx context.Context, traceID string, files map[string]string) (map[string]string, error) {
		cloudReceived = append(cloudReceived, files)
		out := map[string]string{}
		for k, v := range files {
			out[k] = v + "\ncloud lane changed this\n"
		}
		return out, nil
	})
	var localCalls []map[string]string
	local := SessionFunc(func(ctx context.Context, traceID string, files map[string]string) (map[string]string, error) {
		localCalls = append(localCalls, files)
		if len(localCalls) > 1 {
			return nil, fmt.Errorf("local lane: %w", mlx.ErrLocalModelUnavailable)
		}
		out := map[string]string{}
		for k, v := range files {
			out[k] = v + "LOCAL BATCH 1\n"
		}
		return out, nil
	})
	budget := estimateFileTokens("finans-a.md", secret["finans-a.md"]) // one file per batch
	c, logger, notifier := newBatchConsolidator(t, repo, cloud, local, func(cfg *Config) {
		cfg.SecretLaneGlobs = []string{filepath.Join(repo, "memory", "finans*.md")}
		cfg.LocalBatchTokenBudget = budget
	})
	if err := c.Run(context.Background(), "trace-b5-local-mid"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	for _, call := range cloudReceived {
		for k := range call {
			if strings.HasPrefix(k, "finans") {
				t.Fatalf("secret-lane file %s reached the cloud lane", k)
			}
		}
	}
	if len(localCalls) != 2 {
		t.Fatalf("local calls = %d, want 2 (batch 1 ok, batch 2 unavailable, no batch 3)", len(localCalls))
	}
	if _, ok := localCalls[0]["finans-a.md"]; !ok || len(localCalls[0]) != 1 {
		t.Fatalf("local batch 1 = %v, want only finans-a.md", sortedKeys(localCalls[0]))
	}
	if !strings.Contains(branchFile(t, repo, "finans-a.md"), "LOCAL BATCH 1") {
		t.Error("batch 1's validated rewrite was not kept")
	}
	for _, rel := range []string{"finans-b.md", "finans-c.md"} {
		if got := branchFile(t, repo, rel); got != strings.TrimRight(secret[rel], "\n") {
			t.Errorf("%s on the branch = %q, want unchanged", rel, got)
		}
	}
	if !strings.Contains(branchFile(t, repo, "note.md"), "cloud lane changed this") {
		t.Error("the cloud lane's rewrite was lost")
	}
	sent := false
	for _, m := range notifier.sent {
		if m == MsgLocalSkipped {
			sent = true
		}
	}
	if !sent {
		t.Fatalf("notifier.sent = %q, want the byte-exact %q", notifier.sent, MsgLocalSkipped)
	}
	rows := payloadsOfKind(logger.rows, "consolidation.local_unavailable")
	if len(rows) != 1 {
		t.Fatalf("local_unavailable rows = %d, want 1", len(rows))
	}
	if rows[0]["completed_batches"] != 1 {
		t.Errorf("completed_batches = %v, want 1", rows[0]["completed_batches"])
	}
	if got := rows[0]["remaining_files"]; !reflect.DeepEqual(got, []string{"finans-b.md", "finans-c.md"}) {
		t.Errorf("remaining_files = %v, want [finans-b.md finans-c.md]", got)
	}
	p := payloadsOfKind(logger.rows, EventPending)
	if len(p) != 1 || p[0]["skipped_secret_lane"] != true {
		t.Fatalf("pending rows = %v, want one with skipped_secret_lane=true", p)
	}
}

func TestRunSkipsOversizedFileAndLedgers(t *testing.T) {
	repo := initKahyaRepo(t)
	writeFile(t, repo, "memory/dev-not.md", strings.Repeat("büyük satır\n", 900)) // ~11 KB
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-m", "big note")
	var received []map[string]string
	c, logger, _ := newBatchConsolidator(t, repo, changingRecorder(&received), nil, func(cfg *Config) { cfg.MaxFileBytes = 4096 })
	if err := c.Run(context.Background(), "trace-b5-big"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	for _, call := range received {
		if _, ok := call["dev-not.md"]; ok {
			t.Fatal("a file above max_file_bytes reached a session")
		}
	}
	if len(received) != 1 {
		t.Fatalf("cloud calls = %d, want 1 (note.md)", len(received))
	}
	rows := payloadsOfKind(logger.rows, EventFileSkippedTooLarge)
	if len(rows) != 1 || rows[0]["lane"] != LaneCloud || !reflect.DeepEqual(rows[0]["paths"], []string{"dev-not.md"}) {
		t.Fatalf("file_skipped_too_large rows = %v, want one {lane: cloud, paths: [dev-not.md]}", rows)
	}
	if got := branchFile(t, repo, "dev-not.md"); got != strings.TrimRight(strings.Repeat("büyük satır\n", 900), "\n") {
		t.Error("the skipped file changed on the branch")
	}
}

// changingRecorder records each call and appends a line to every file.
func changingRecorder(received *[]map[string]string) SessionFunc {
	return func(ctx context.Context, traceID string, files map[string]string) (map[string]string, error) {
		*received = append(*received, files)
		out := map[string]string{}
		for k, v := range files {
			out[k] = v + "\nchanged\n"
		}
		return out, nil
	}
}

func TestRunDefersBatchesBeyondMaxPerLane(t *testing.T) {
	repo := initKahyaRepo(t)
	corpus := seedCorpus(t, repo, 4, 60)
	var received []map[string]string
	c, logger, _ := newBatchConsolidator(t, repo, changingRecorder(&received), nil, func(cfg *Config) {
		cfg.CloudBatchTokenBudget = estimateFileTokens("topic00/note.md", corpus["topic00/note.md"]) // one file per batch
		cfg.MaxBatchesPerLane = 1
	})
	if err := c.Run(context.Background(), "trace-b5-defer"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(received) != 1 {
		t.Fatalf("cloud calls = %d, want exactly max_batches_per_lane = 1", len(received))
	}
	sentRel := sortedKeys(received[0])[0]
	rows := payloadsOfKind(logger.rows, EventBatchesDeferred)
	if len(rows) != 1 || rows[0]["lane"] != LaneCloud {
		t.Fatalf("batches_deferred rows = %v, want one for lane cloud", rows)
	}
	deferred, _ := rows[0]["files"].([]string)
	if len(deferred) != 4 || rows[0]["batches"] != 4 {
		t.Fatalf("deferred = %v (batches %v), want the other 4 files in 4 batches", deferred, rows[0]["batches"])
	}
	all := append([]string{sentRel}, deferred...)
	sort.Strings(all)
	want := append(sortedKeys(corpus), "note.md")
	sort.Strings(want)
	if !reflect.DeepEqual(all, want) {
		t.Fatalf("sent %s + deferred %v != every cloud file %v", sentRel, deferred, want)
	}
	for _, rel := range deferred {
		orig := corpus[rel]
		if rel == "note.md" {
			orig = "line one\nline two\nline three\n"
		}
		if got := branchFile(t, repo, rel); got != strings.TrimRight(orig, "\n") {
			t.Errorf("deferred %s changed on the branch", rel)
		}
	}
}

// TestRunDropsMalformedBatchKeepsOthers (decision G06-7): a batch whose
// response is cut (invalid JSON) is dropped and ledgered; the other batches'
// rewrites still land, and the night produces its suggestion.
func TestRunDropsMalformedBatchKeepsOthers(t *testing.T) {
	repo := initKahyaRepo(t)
	corpus := seedCorpus(t, repo, 3, 60)
	calls := 0
	var cutBatch []string
	cloud := SessionFunc(func(ctx context.Context, traceID string, files map[string]string) (map[string]string, error) {
		calls++
		out := map[string]string{}
		for k, v := range files {
			out[k] = v + "\nchanged\n"
		}
		raw, _ := json.Marshal(map[string]any{"files": out})
		if calls == 2 {
			cutBatch = sortedKeys(files)
			raw = raw[:len(raw)/2] // a max_tokens cut
		}
		return parseRewriteResponse(string(raw), LaneCloud, files)
	})
	c, logger, notifier := newBatchConsolidator(t, repo, cloud, nil, func(cfg *Config) {
		cfg.CloudBatchTokenBudget = estimateFileTokens("topic00/note.md", corpus["topic00/note.md"])
	})
	if err := c.Run(context.Background(), "trace-b5-drop"); err != nil {
		t.Fatalf("Run() error = %v, want the night to continue past a malformed batch", err)
	}
	if calls != 4 {
		t.Fatalf("cloud calls = %d, want 4 (every batch tried)", calls)
	}
	rows := payloadsOfKind(logger.rows, EventBatchDropped)
	if len(rows) != 1 || rows[0]["lane"] != LaneCloud || !reflect.DeepEqual(rows[0]["files"], cutBatch) || rows[0]["batch"] != 2 {
		t.Fatalf("batch_dropped rows = %v, want one {lane cloud, batch 2, files %v}", rows, cutBatch)
	}
	if reason, _ := rows[0]["reason"].(string); !strings.Contains(reason, "decode") {
		t.Errorf("batch_dropped reason = %q, want the decode error", reason)
	}
	all := append(sortedKeys(corpus), "note.md")
	for _, rel := range all {
		got := branchFile(t, repo, rel)
		if rel == cutBatch[0] {
			if strings.Contains(got, "changed") {
				t.Errorf("the dropped batch's file %s changed", rel)
			}
			continue
		}
		if !strings.Contains(got, "changed") {
			t.Errorf("%s: the other batches' rewrites were lost", rel)
		}
	}
	if len(notifier.sent) != 1 || notifier.sent[0] != MsgSuggestionReady {
		t.Fatalf("notifier.sent = %q, want the suggestion notice only", notifier.sent)
	}
}

// TestRunMalformedBatchNeverMasksUnknownPath: a response that both names a
// foreign path and is otherwise usable is a security rejection, never a
// dropped batch (G06-3 still rejects the night).
func TestRunMalformedBatchNeverMasksUnknownPath(t *testing.T) {
	repo := initKahyaRepo(t)
	cloud := SessionFunc(func(ctx context.Context, traceID string, files map[string]string) (map[string]string, error) {
		return nil, fmt.Errorf("%w; %w", ErrRewriteMalformed, ErrRewriteUnknownPath)
	})
	c, logger, _ := newBatchConsolidator(t, repo, cloud, nil, nil)
	if err := c.Run(context.Background(), "trace-b5-both"); !errors.Is(err, ErrRewriteUnknownPath) {
		t.Fatalf("Run() error = %v, want ErrRewriteUnknownPath", err)
	}
	if len(payloadsOfKind(logger.rows, EventBatchDropped)) != 0 || len(payloadsOfKind(logger.rows, EventRewriteRejected)) != 1 {
		t.Fatalf("want a rejection, not a dropped batch: %v", eventKinds(logger.rows))
	}
}
