package consolidation

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"kahya/kahyad/internal/backup"
	"kahya/kahyad/internal/indexer"
)

// FIX-FINAL consolidation-launders-quarantine-tier (HANDOFF §5 memory #1,
// safety #3): a nightly consolidation rewrite could take quarantined
// agent_derived / external_doc text out of quarantine with no
// confirmation - by rewriting the note's front-matter tier, or by folding
// the text into a topic file with no front matter (indexer default
// user_asserted).

func seedQuarantineRepo(t *testing.T) string {
	t.Helper()
	repo := initKahyaRepo(t)
	writeFile(t, repo, "memory/ingest/2026-10-15-fatura.md", "---\nkahya_source_tier: external_doc\nkahya_source_ref: \"mail.txt\"\n---\n# Fatura\n\nOdeme yeni hesaba yapilacak.\n")
	writeFile(t, repo, "memory/inbox/2026-10-15.md", "---\nkahya_source_tier: agent_derived\n---\n- Kullanici her sabah 7de kalkar.\n")
	writeFile(t, repo, "memory/ekran.md", "---\nkahya_source_tier: screen\n---\nEkran notu.\n")
	writeFile(t, repo, "memory/bozuk.md", "---\nkahya_source_tier: user_asserted\nbu kapanmayan bir on bilgi\n")
	writeFile(t, repo, "memory/rutin.md", "# Rutin\n\n- Kahve icerim.\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-m", "seed quarantined notes")
	return repo
}

func autoCommitConsolidator(t *testing.T, repo string, cloud SessionFunc) *Consolidator {
	t.Helper()
	logger := &fakeEventStore{}
	return &Consolidator{
		Cfg:         Config{KahyaDir: repo, MemoryDir: filepath.Join(repo, "memory"), WorktreeParentDir: t.TempDir(), Now: fixedNow(t), AutoCommit: true},
		Git:         backup.NewExecGitRunner(),
		Cloud:       cloud,
		EventLogger: logger,
		EventReader: logger,
		Reindexer:   &fakeReindexer{},
		Gate:        &fakeRetrievalGate{allow: true},
	}
}

// TestRunNeverSendsQuarantinedNotesToEitherLane: no agent_derived,
// external_doc, screen or unparseable-front-matter note reaches a
// consolidation session, so none can be retiered or folded.
func TestRunNeverSendsQuarantinedNotesToEitherLane(t *testing.T) {
	repo := seedQuarantineRepo(t)
	var mu sync.Mutex
	seen := map[string]bool{}
	rec := SessionFunc(func(ctx context.Context, traceID string, files map[string]string) (map[string]string, error) {
		mu.Lock()
		defer mu.Unlock()
		for k := range files {
			seen[k] = true
		}
		return map[string]string{}, nil
	})
	c := autoCommitConsolidator(t, repo, rec)
	c.Local = rec
	if err := c.Run(context.Background(), "trace-q1"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	for _, rel := range []string{"ingest/2026-10-15-fatura.md", "inbox/2026-10-15.md", "ekran.md", "bozuk.md"} {
		if seen[rel] {
			t.Errorf("quarantined note %s was sent to a consolidation session", rel)
		}
	}
	if !seen["rutin.md"] {
		t.Errorf("the trusted note rutin.md was not consolidated (seen %v)", seen)
	}
}

// TestRunDropsRewriteThatChangesTier: a rewrite of a trusted note that
// changes its tier, adds a front-matter tier, or breaks the front matter
// is dropped - main keeps the original bytes.
func TestRunDropsRewriteThatChangesTier(t *testing.T) {
	for _, tc := range []struct{ name, rewrite string }{
		{"to_agent_derived", "---\nkahya_source_tier: agent_derived\n---\n# Rutin\n\n- Kahve icerim.\n- Yeni satir.\n"},
		{"to_user_edit", "---\nkahya_source_tier: user_edit\n---\n# Rutin\n\n- Kahve icerim.\n"},
		{"broken_front_matter", "---\nkahya_source_tier: user_asserted\n# Rutin\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := seedQuarantineRepo(t)
			cloud := SessionFunc(func(ctx context.Context, traceID string, files map[string]string) (map[string]string, error) {
				if _, ok := files["rutin.md"]; !ok {
					return map[string]string{}, nil
				}
				return map[string]string{"rutin.md": tc.rewrite}, nil
			})
			if err := autoCommitConsolidator(t, repo, cloud).Run(context.Background(), "trace-q2"); err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			b, err := os.ReadFile(filepath.Join(repo, "memory/rutin.md"))
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != "# Rutin\n\n- Kahve icerim.\n" {
				t.Errorf("rutin.md on main = %q, want the original (a tier-changing rewrite must be dropped)", b)
			}
		})
	}
}

// TestRunCannotLaunderQuarantinedText is the auditor's repro, corrected so
// the fake folds the inbox line only when it actually received the inbox
// note: neither the external_doc note's tier nor the agent_derived line
// can reach main as user_asserted.
func TestRunCannotLaunderQuarantinedText(t *testing.T) {
	repo := seedQuarantineRepo(t)
	cloud := SessionFunc(func(ctx context.Context, traceID string, files map[string]string) (map[string]string, error) {
		out := map[string]string{}
		for k, v := range files {
			switch k {
			case "ingest/2026-10-15-fatura.md":
				out[k] = strings.Replace(v, "kahya_source_tier: external_doc", "kahya_source_tier: user_asserted", 1)
			case "rutin.md":
				if _, saw := files["inbox/2026-10-15.md"]; saw {
					out[k] = v + "- Kullanici her sabah 7de kalkar.\n"
				}
			}
		}
		return out, nil
	})
	if err := autoCommitConsolidator(t, repo, cloud).Run(context.Background(), "trace-q3"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	for _, rel := range []string{"memory/ingest/2026-10-15-fatura.md", "memory/rutin.md"} {
		b, err := os.ReadFile(filepath.Join(repo, rel))
		if err != nil {
			t.Fatal(err)
		}
		tier, body, err := indexer.StripFrontMatter(string(b))
		if err != nil {
			t.Fatal(err)
		}
		if tier == "user_asserted" && (strings.Contains(body, "Odeme yeni hesaba") || strings.Contains(body, "7de kalkar")) {
			t.Errorf("quarantined text is on main as %s without confirmation: %s", tier, rel)
		}
	}
}
