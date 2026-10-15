package consolidation

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kahya/kahyad/internal/backup"
)

// TestApproveLedgerFailureKeepsBranchForRetry (FIX-FINAL consolidation-
// approved-ledger-loss-strands-pending): finalize deleted the branch and
// only then wrote consolidation.approved, logging a failure, so Approve
// returned nil while FindPending still reported the merged suggestion and
// show/approve/reject failed with raw git errors. The approved row is now
// written first and its failure returned: the branch survives, show still
// answers, and a retried approve ledgers the merge and removes the branch.
func TestApproveLedgerFailureKeepsBranchForRetry(t *testing.T) {
	repo := initKahyaRepo(t)
	writeFile(t, repo, "memory/note.md", "line one\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-m", "seed")
	cloud := SessionFunc(func(ctx context.Context, traceID string, files map[string]string) (map[string]string, error) {
		out := map[string]string{}
		for k, v := range files {
			out[k] = v + "CONSOLIDATED\n"
		}
		return out, nil
	})
	logger := &fakeEventStore{}
	c := &Consolidator{
		Cfg:         Config{KahyaDir: repo, MemoryDir: filepath.Join(repo, "memory"), WorktreeParentDir: t.TempDir(), Now: fixedNow(t)},
		Git:         backup.NewExecGitRunner(),
		Cloud:       cloud,
		Notifier:    &fakeNotifier{},
		EventLogger: logger,
		EventReader: logger,
		Reindexer:   &fakeReindexer{},
	}
	if err := c.Run(context.Background(), "trace-night1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	logger.failKind = EventApproved
	if err := c.Approve(context.Background(), "trace-approve1"); err == nil {
		t.Fatal("approve with a failed approved ledger write returned nil")
	}
	logger.failKind = ""
	if _, found, err := c.Show(context.Background()); err != nil || !found {
		t.Fatalf("show after the failed ledger write: found=%v err=%v, want the pending suggestion", found, err)
	}
	if err := c.Approve(context.Background(), "trace-approve2"); err != nil {
		t.Fatalf("retried approve: %v", err)
	}
	if p, err := FindPending(context.Background(), logger); err != nil || p != nil {
		t.Fatalf("pending after the retry = %+v, %v; want none", p, err)
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "memory", "note.md")); !strings.Contains(string(b), "CONSOLIDATED") {
		t.Fatalf("main lacks the merge: %q", b)
	}
	if out := runGit(t, repo, "branch", "--list", "kahya/consolidation-*"); strings.TrimSpace(out) != "" {
		t.Fatalf("consolidation branch survived the retried approve: %q", out)
	}
}
