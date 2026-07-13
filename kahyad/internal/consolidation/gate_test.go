// gate_test.go is W5-05's own phase-gate test (HANDOFF §6 W5 acceptance,
// verbatim): "gece külliyat konsolide olup diff commit ediliyor". It
// exercises the REAL Consolidator.Run/Show/Approve pipeline against a real
// git-worktree fixture corpus (initKahyaRepo, the SAME helper this
// package's own consolidation_test.go already uses) end to end, in one
// place, as the phase gate's own canonical record - Run produces a
// pending diff, and Approve lands an author=kahyad commit AND triggers
// exactly one reindex. The individual invariants each have their own
// dedicated, more granular tests elsewhere in this package
// (TestRunProducesPendingDiffAndApproveShowsCommitDiscipline,
// TestRunWriteBoundaryAndReindexOnlyAfterApprove); this file's job is only
// to compose them into the single assertion the W5 acceptance gate names.
package consolidation

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"kahya/kahyad/internal/backup"
)

func TestW5GateConsolidationProducesDiffThenApproveCommitsAsKahyaAndReindexes(t *testing.T) {
	repo := initKahyaRepo(t)
	before := readFile(t, filepath.Join(repo, "memory", "note.md"))

	cloud := SessionFunc(func(ctx context.Context, traceID string, files map[string]string) (map[string]string, error) {
		out := map[string]string{}
		for k, v := range files {
			out[k] = v + "\nW5 GATE CONSOLIDATION MARKER\n"
		}
		return out, nil
	})

	logger := &fakeEventStore{}
	notifier := &fakeNotifier{}
	reindexer := &fakeReindexer{}
	c := &Consolidator{
		Cfg:         Config{KahyaDir: repo, MemoryDir: filepath.Join(repo, "memory"), WorktreeParentDir: t.TempDir(), Now: fixedNow(t)},
		Git:         backup.NewExecGitRunner(),
		Cloud:       cloud,
		Notifier:    notifier,
		EventLogger: logger,
		EventReader: logger,
		Reindexer:   reindexer,
	}

	// --- "gece külliyat konsolide olup" - a nightly Run produces a diff. ---
	if err := c.Run(context.Background(), "trace-w5-gate-run"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	diff, found, err := c.Show(context.Background())
	if err != nil {
		t.Fatalf("Show() error = %v", err)
	}
	if !found {
		t.Fatal("Show() found = false, want a pending suggestion (a diff) after Run")
	}
	if !strings.Contains(diff, "W5 GATE CONSOLIDATION MARKER") {
		t.Fatalf("Show() diff missing the expected content:\n%s", diff)
	}
	// Suggestion mode (task spec default): nothing merged to main yet, and
	// no reindex has happened yet - both are gated on approval below.
	if got := readFile(t, filepath.Join(repo, "memory", "note.md")); got != before {
		t.Fatalf("main's note.md changed before approval - write-boundary violated: got %q, want unchanged %q", got, before)
	}
	if reindexer.calls != 0 {
		t.Fatalf("Reindexer called %d times before approval, want 0", reindexer.calls)
	}

	// --- "diff commit ediliyor" - Approve lands author=kahyad + reindex. ---
	if err := c.Approve(context.Background(), "trace-w5-gate-approve"); err != nil {
		t.Fatalf("Approve() error = %v", err)
	}

	rows := gitLogAuthorsAndSubjects(t, repo)
	if len(rows) == 0 {
		t.Fatal("git log is empty after approve")
	}
	last := rows[len(rows)-1]
	if last[0] != KahyaCommitAuthor {
		t.Fatalf("last commit author = %q, want %q (author=kahyad commit)", last[0], KahyaCommitAuthor)
	}
	if last[1] != "nightly consolidation" {
		t.Fatalf("last commit subject = %q, want %q", last[1], "nightly consolidation")
	}
	if got := readFile(t, filepath.Join(repo, "memory", "note.md")); !strings.Contains(got, "W5 GATE CONSOLIDATION MARKER") {
		t.Fatalf("main's note.md after approve = %q, want the merged content", got)
	}
	if reindexer.calls != 1 {
		t.Fatalf("Reindexer called %d times after approve, want exactly 1 (a reindex event/trigger)", reindexer.calls)
	}

	// The pending suggestion is resolved; nothing left outstanding.
	if p, err := FindPending(context.Background(), logger); err != nil || p != nil {
		t.Fatalf("FindPending() after approve = (%+v, %v), want (nil, nil)", p, err)
	}
}
