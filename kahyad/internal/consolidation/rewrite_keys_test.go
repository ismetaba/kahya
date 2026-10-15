package consolidation

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"kahya/kahyad/internal/backup"
	"kahya/kahyad/internal/mlx"
)

// FIX-P-7: a session may only rewrite the files it was given. A cloud
// rewrite naming a secret-lane path (or a local rewrite naming a cloud-lane
// path) rejects the whole night - nothing is written, no branch survives.

const p7SecretNote = "TR330006100519786457841326 iban bilgisi\n"

// p7Fixture is a repo with one cloud-lane note (memory/note.md, from
// initKahyaRepo) and one secret-lane note (memory/finans-2026.md).
func p7Fixture(t *testing.T, cloud, local Session) (*Consolidator, string, *fakeEventStore, *fakeNotifier) {
	t.Helper()
	repo := initKahyaRepo(t)
	writeFile(t, repo, "memory/finans-2026.md", p7SecretNote)
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-m", "seed finans note")
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
	return c, repo, logger, notifier
}

// assertRewriteRejected checks every observable of a rejected night.
func assertRewriteRejected(t *testing.T, err error, repo string, logger *fakeEventStore, notifier *fakeNotifier, lane string) {
	t.Helper()
	if !errors.Is(err, ErrRewriteUnknownPath) {
		t.Fatalf("Run() error = %v, want ErrRewriteUnknownPath", err)
	}
	if br := runGit(t, repo, "branch", "--list", ConsolidationBranchPrefix+"*"); br != "" {
		t.Errorf("consolidation branch survived the rejection: %q", br)
	}
	if wt := runGit(t, repo, "worktree", "list", "--porcelain"); strings.Count(wt, "worktree ") != 1 {
		t.Errorf("worktree list = %q, want only the main working tree", wt)
	}
	if got := readFile(t, filepath.Join(repo, "memory", "finans-2026.md")); got != p7SecretNote {
		t.Errorf("finans-2026.md = %q, want unchanged %q", got, p7SecretNote)
	}
	var rejected []fakeEventRow
	for _, r := range logger.rows {
		switch r.kind {
		case EventRewriteRejected:
			rejected = append(rejected, r)
		case EventPending:
			t.Errorf("a %s row was written for a rejected night", EventPending)
		}
	}
	if len(rejected) != 1 || rejected[0].payload["lane"] != lane {
		t.Fatalf("%s rows = %+v, want exactly one with lane=%q", EventRewriteRejected, rejected, lane)
	}
	if reason, _ := rejected[0].payload["reason"].(string); !strings.Contains(reason, "lane="+lane) {
		t.Errorf("ledger reason = %q, want the lane and the offending path (local-only detail)", reason)
	}
	if len(notifier.sent) != 1 || notifier.sent[0] != MsgRewriteRejected {
		t.Fatalf("notifier.sent = %q, want exactly [%q]", notifier.sent, MsgRewriteRejected)
	}
	for _, msg := range notifier.sent {
		if strings.Contains(msg, "finans") || strings.Contains(msg, ".md") {
			t.Errorf("notification %q names a file; the Telegram text must carry no path", msg)
		}
	}
}

func TestRunRejectsCloudRewriteNamingSecretLanePath(t *testing.T) {
	cloud := SessionFunc(func(ctx context.Context, traceID string, files map[string]string) (map[string]string, error) {
		out := map[string]string{}
		for k, v := range files {
			out[k] = v
		}
		out["finans-2026.md"] = "HALLUCINATED BY CLOUD\n" // not in this lane's input
		return out, nil
	})
	local := SessionFunc(func(ctx context.Context, traceID string, files map[string]string) (map[string]string, error) {
		return nil, mlx.ErrLocalModelUnavailable
	})
	c, repo, logger, notifier := p7Fixture(t, cloud, local)

	err := c.Run(context.Background(), "trace-p7-cloud")
	assertRewriteRejected(t, err, repo, logger, notifier, LaneCloud)
	if out := runGit(t, repo, "log", "--all", "-p", "--format="); strings.Contains(out, "HALLUCINATED BY CLOUD") {
		t.Fatal("the cloud lane's rewrite of a secret-lane file reached a commit")
	}
}

func TestRunRejectsLocalRewriteNamingCloudLanePath(t *testing.T) {
	var cloudReceived []map[string]string
	cloud := echoSession(&cloudReceived)
	local := SessionFunc(func(ctx context.Context, traceID string, files map[string]string) (map[string]string, error) {
		out := map[string]string{}
		for k, v := range files {
			out[k] = v
		}
		out["note.md"] = "LOCAL LANE WROTE A CLOUD FILE\n" // cloud lane's file
		return out, nil
	})
	c, repo, logger, notifier := p7Fixture(t, cloud, local)

	err := c.Run(context.Background(), "trace-p7-local")
	assertRewriteRejected(t, err, repo, logger, notifier, LaneLocal)
	if out := runGit(t, repo, "log", "--all", "-p", "--format="); strings.Contains(out, "LOCAL LANE WROTE A CLOUD FILE") {
		t.Fatal("the local lane's rewrite of a cloud-lane file reached a commit")
	}
}

func TestParseRewriteResponseRejectsUnknownPath(t *testing.T) {
	_, err := parseRewriteResponse(`{"files":{"a.md":"x","b.md":"y"}}`, LaneCloud, map[string]string{"a.md": "old"})
	if !errors.Is(err, ErrRewriteUnknownPath) {
		t.Fatalf("parseRewriteResponse(extra key) error = %v, want ErrRewriteUnknownPath", err)
	}
	if !strings.Contains(err.Error(), `first="b.md"`) || !strings.Contains(err.Error(), "lane=cloud") {
		t.Errorf("error = %q, want the lane and the first offending path", err)
	}
}

func TestParseRewriteResponseAcceptsSubsetAsUnchanged(t *testing.T) {
	got, err := parseRewriteResponse(`{"files":{"a.md":"new a"}}`, LaneCloud, map[string]string{"a.md": "old a", "b.md": "old b"})
	if err != nil {
		t.Fatalf("parseRewriteResponse(subset) error = %v, want nil", err)
	}
	if got["a.md"] != "new a" {
		t.Errorf("a.md = %q, want %q", got["a.md"], "new a")
	}
	if _, ok := got["b.md"]; ok {
		t.Errorf("b.md present in %v, want absent (= unchanged)", got)
	}
}

func TestCloudSessionConsolidateRejectsUnknownPathFromSpawner(t *testing.T) {
	spawner := &fakeWorkerSpawner{response: `{"files": {"a.md": "new a", "finans.md": "smuggled"}}`}
	session := CloudSession{Spawner: spawner}
	_, err := session.Consolidate(context.Background(), "trace-p7", map[string]string{"a.md": "old a"})
	if !errors.Is(err, ErrRewriteUnknownPath) {
		t.Fatalf("Consolidate() error = %v, want ErrRewriteUnknownPath", err)
	}
	if len(spawner.envelopes) != 1 {
		t.Fatalf("spawner called %d times, want 1", len(spawner.envelopes))
	}
}

func TestLocalSessionConsolidateRejectsUnknownPath(t *testing.T) {
	srv := fakeQwenChatServer(t, `{"files":{"finans.md":"x","extra.md":"y"}}`)
	defer srv.Close()
	runner := &fakeLocalRunner{baseURL: srv.URL}
	session := LocalSession{Sup: runner, Model: "qwen3-30b-a3b"}
	_, err := session.Consolidate(context.Background(), "trace-p7", map[string]string{"finans.md": "old"})
	if !errors.Is(err, ErrRewriteUnknownPath) {
		t.Fatalf("Consolidate() error = %v, want ErrRewriteUnknownPath", err)
	}
	if !runner.called {
		t.Fatal("LocalRunner.Do was never called")
	}
}

func TestValidateRewriteKeysMergedSet(t *testing.T) {
	files := map[string]string{"a.md": "", "b.md": ""}
	if err := validateRewriteKeys("merged", files, map[string]string{"a.md": "x", "b.md": "y"}); err != nil {
		t.Fatalf("validateRewriteKeys(known keys) = %v, want nil", err)
	}
	err := validateRewriteKeys("merged", files, map[string]string{"a.md": "x", "z.md": "y", "c.md": "w"})
	if !errors.Is(err, ErrRewriteUnknownPath) {
		t.Fatalf("validateRewriteKeys(unknown) = %v, want ErrRewriteUnknownPath", err)
	}
	if !strings.Contains(err.Error(), "count=2") || !strings.Contains(err.Error(), `first="c.md"`) {
		t.Errorf("error = %q, want count=2 and the sorted-first offender c.md", err)
	}
}
