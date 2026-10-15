package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// confirmFixture is a real git fixture repo with one note written through
// HandleWrite (agent_derived front matter, committed by kahyad).
func confirmFixture(t *testing.T) (repoRoot, memDir string, s *Server, idx *fakeIndexer, ledger *fakeLedger) {
	t.Helper()
	repoRoot, memDir = newFixtureRepo(t)
	idx = &fakeIndexer{episodeID: 7}
	ledger = &fakeLedger{}
	s = newTestServer(memDir, &fakeSearcher{}, idx, ledger)
	if _, err := s.HandleWrite(context.Background(), "tw", MemoryWriteArgs{File: "inbox/2026-10-08.md", Content: "Emre Acme'de çalışıyor."}); err != nil {
		t.Fatal(err)
	}
	idx.calls = nil
	ledger.events = nil
	return repoRoot, memDir, s, idx, ledger
}

// TestHandleConfirmRewritesFrontMatterToUserAsserted: `kahya memory
// confirm` promotes an agent_derived note to user_asserted in the FILE (the
// source of truth survives a brain.db rebuild): front matter
// kahya_source_tier: user_asserted + kahya_confirmed_at, the body
// byte-identical, one commit by kahyad (never "user": a review is
// user_asserted, not user_edit - HANDOFF §7), the note reindexed, the
// confirmation ledgered.
func TestHandleConfirmRewritesFrontMatterToUserAsserted(t *testing.T) {
	repoRoot, memDir, s, idx, ledger := confirmFixture(t)
	abs := filepath.Join(memDir, "inbox", "2026-10-08.md")
	before, _ := os.ReadFile(abs)

	out, err := s.HandleConfirm(context.Background(), "tc", MemoryConfirmArgs{File: "inbox/2026-10-08.md"})
	if err != nil {
		t.Fatalf("HandleConfirm: %v", err)
	}
	if out.File != "inbox/2026-10-08.md" || out.EpisodeID != 7 || out.PreviousTier != "agent_derived" || out.CommitSHA == "" {
		t.Fatalf("out = %+v", out)
	}
	after, _ := os.ReadFile(abs)
	if !regexp.MustCompile(`^---\nkahya_source_tier: user_asserted\nkahya_confirmed_at: \d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ\n---\n`).Match(after) {
		t.Fatalf("front matter = %q", after)
	}
	if stripLeadingFrontMatter(string(after)) != stripLeadingFrontMatter(string(before)) {
		t.Fatalf("body changed:\nbefore %q\nafter  %q", before, after)
	}
	if got := runGitT(t, repoRoot, "log", "-1", "--format=%an <%ae>|%s"); got != "kahyad <kahyad@local>|memory_confirm: inbox/2026-10-08.md" {
		t.Fatalf("last commit = %q", got)
	}
	if got := runGitT(t, repoRoot, "show", "--name-only", "--format=", out.CommitSHA); got != "memory/inbox/2026-10-08.md" {
		t.Fatalf("commit touched %q", got)
	}
	if len(idx.calls) != 1 || idx.calls[0] != "inbox/2026-10-08.md" {
		t.Fatalf("ReindexFile calls = %v", idx.calls)
	}
	if len(ledger.events) != 1 || ledger.events[0].kind != EventMemoryConfirm ||
		ledger.events[0].payload["file"] != "inbox/2026-10-08.md" || ledger.events[0].payload["previous_tier"] != "agent_derived" ||
		ledger.events[0].payload["commit_sha"] != out.CommitSHA {
		t.Fatalf("ledger = %+v", ledger.events)
	}
}

// TestHandleConfirmKeepsOtherFrontMatterKeys: an ingested external_doc note
// (MEM-6) is confirmable too, and its provenance keys survive the rewrite.
func TestHandleConfirmKeepsOtherFrontMatterKeys(t *testing.T) {
	repoRoot, memDir := newFixtureRepo(t)
	note := "---\nkahya_source_tier: external_doc\nkahya_source_ref: \"rapor.txt\"\nkahya_confirmed_at: eski\n---\n# Rapor\n\nÖzet.\n"
	if err := os.MkdirAll(filepath.Join(memDir, "ingest"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memDir, "ingest", "r.md"), []byte(note), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitT(t, repoRoot, "add", "--", "memory/ingest/r.md")
	runGitT(t, repoRoot, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "seed")
	s := newTestServer(memDir, &fakeSearcher{}, &fakeIndexer{episodeID: 3}, &fakeLedger{})
	out, err := s.HandleConfirm(context.Background(), "tc", MemoryConfirmArgs{File: "ingest/r.md"})
	if err != nil || out.PreviousTier != "external_doc" {
		t.Fatalf("HandleConfirm = %+v %v", out, err)
	}
	after, _ := os.ReadFile(filepath.Join(memDir, "ingest", "r.md"))
	got := string(after)
	if !strings.HasPrefix(got, "---\nkahya_source_tier: user_asserted\nkahya_confirmed_at: ") ||
		!strings.Contains(got, "\nkahya_source_ref: \"rapor.txt\"\n---\n# Rapor\n\nÖzet.\n") || strings.Contains(got, "eski") {
		t.Fatalf("rewritten note = %q", got)
	}
}

// TestHandleConfirmAlreadyUserTierErrors: a note already at a user tier (an
// explicit user_asserted/user_edit key, or no front matter at all - the
// indexer's default user_asserted) is refused with the Turkish error and
// left untouched; an unknown tier value is refused too.
func TestHandleConfirmAlreadyUserTierErrors(t *testing.T) {
	_, memDir := newFixtureRepo(t)
	s := newTestServer(memDir, &fakeSearcher{}, &fakeIndexer{episodeID: 1}, &fakeLedger{})
	for name, content := range map[string]string{
		"plain.md":    "kullanıcının kendi notu\n",
		"asserted.md": "---\nkahya_source_tier: user_asserted\n---\nnot\n",
		"edit.md":     "---\nkahya_source_tier: user_edit\n---\nnot\n",
	} {
		abs := filepath.Join(memDir, name)
		if err := os.WriteFile(abs, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := s.HandleConfirm(context.Background(), "tc", MemoryConfirmArgs{File: name})
		if !errors.Is(err, ErrNoteAlreadyConfirmed) || !strings.HasPrefix(err.Error(), "memory_confirm: not zaten onaylı (") {
			t.Fatalf("%s: err = %v, want ErrNoteAlreadyConfirmed", name, err)
		}
		if got, _ := os.ReadFile(abs); string(got) != content {
			t.Fatalf("%s rewritten: %q", name, got)
		}
	}
	if err := os.WriteFile(filepath.Join(memDir, "odd.md"), []byte("---\nkahya_source_tier: sahte\n---\nnot\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.HandleConfirm(context.Background(), "tc", MemoryConfirmArgs{File: "odd.md"}); err == nil || errors.Is(err, ErrNoteAlreadyConfirmed) {
		t.Fatalf("unknown tier: err = %v, want a refusal", err)
	}
	if _, err := s.HandleConfirm(context.Background(), "tc", MemoryConfirmArgs{File: "yok.md"}); !errors.Is(err, ErrNoteNotFound) {
		t.Fatalf("missing note: err = %v, want ErrNoteNotFound", err)
	}
}

// TestHandleConfirmRejectsTraversal: the path is confined to MemoryDir
// exactly like memory_write's (no absolute path, no "..", no symlink).
func TestHandleConfirmRejectsTraversal(t *testing.T) {
	_, memDir := newFixtureRepo(t)
	outside := filepath.Join(filepath.Dir(memDir), "outside.md")
	if err := os.WriteFile(outside, []byte("---\nkahya_source_tier: agent_derived\n---\nx\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(memDir, &fakeSearcher{}, &fakeIndexer{episodeID: 1}, &fakeLedger{})
	for _, p := range []string{"../outside.md", outside, ""} {
		if _, err := s.HandleConfirm(context.Background(), "tc", MemoryConfirmArgs{File: p}); !errors.Is(err, ErrNotePath) {
			t.Fatalf("HandleConfirm(%q) = %v, want ErrNotePath", p, err)
		}
	}
	if got, _ := os.ReadFile(outside); !strings.Contains(string(got), "agent_derived") {
		t.Fatal("a file outside the memory dir was rewritten")
	}
}

// TestHandleConfirmGitFailureRestoresFile: a failing commit restores the
// note's prior bytes - no promoted tier on disk without a commit, a
// reindex and a ledger row.
func TestHandleConfirmGitFailureRestoresFile(t *testing.T) {
	repoRoot, memDir, s, idx, ledger := confirmFixture(t)
	abs := filepath.Join(memDir, "inbox", "2026-10-08.md")
	before, _ := os.ReadFile(abs)
	installFailingPreCommitHook(t, repoRoot)
	if _, err := s.HandleConfirm(context.Background(), "tc", MemoryConfirmArgs{File: "inbox/2026-10-08.md"}); err == nil {
		t.Fatal("HandleConfirm with a failing commit = nil error")
	}
	if after, _ := os.ReadFile(abs); string(after) != string(before) {
		t.Fatalf("note not restored:\n%q", after)
	}
	if len(idx.calls) != 0 || len(ledger.events) != 0 {
		t.Fatalf("reindex %v / ledger %v after a failed commit", idx.calls, ledger.events)
	}
	if st := runGitT(t, repoRoot, "status", "--porcelain"); st != "" {
		t.Fatalf("git status = %q, want clean", st)
	}
}
