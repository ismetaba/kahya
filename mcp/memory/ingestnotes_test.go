package memory

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func digestArgs(rel, digest, ingestedAt, lane string, secret bool) TieredWriteArgs {
	return TieredWriteArgs{
		RelPath: rel, Tier: "external_doc", SecretLane: secret, CommitPrefix: "ingest", Body: "# Belge\n",
		FrontMatter: []FrontMatterField{
			{Key: "kahya_source_sha256", Value: digest}, {Key: "kahya_ingested_at", Value: ingestedAt},
			{Key: "kahya_lane_reason", Value: "forced"}, {Key: "kahya_lane", Value: lane},
		},
	}
}

// TestFindIngestNotesByTheirOwnFrontMatter (FIX-FINAL ingest-kill-between-
// note-commit-and-note-written-duplicates): the notes an ingest wrote for
// a document are found by the kahya_source_sha256 key of their LEADING
// front matter - in the ingest dir and in the secret-lane one, newest
// first, with their lane, lane reason and last commit - and nothing else
// is: another document's note, the same text in a note body, a symlink, a
// note outside the ingest dirs.
func TestFindIngestNotesByTheirOwnFrontMatter(t *testing.T) {
	_, memDir := newFixtureRepo(t)
	s := newTestServer(memDir, &fakeSearcher{}, &fakeIndexer{episodeID: 1}, &fakeLedger{})
	s.SetSecretLaneSeam(secretSeam(memDir, nil, nil, nil))
	ctx := context.Background()
	const digest = "d09fe115c7bf83433d13159b9dea06c86f07f94a45e6858b53bc6eddf901e2f9"
	older, err := s.HandleWriteTiered(ctx, "tr", digestArgs("ingest/a.md", digest, "2026-10-08T10:00:00Z", "normal", false))
	if err != nil {
		t.Fatal(err)
	}
	newer, err := s.HandleWriteTiered(ctx, "tr", digestArgs("ingest/a.md", digest, "2026-10-09T10:00:00Z", "secret", true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.HandleWriteTiered(ctx, "tr", digestArgs("ingest/b.md", "other", "2026-10-10T10:00:00Z", "normal", false)); err != nil {
		t.Fatal(err)
	}
	// the digest in a note BODY (memory_write keeps its own agent_derived
	// leading block), a symlink to a matching note, a matching note
	// outside the ingest dirs
	if _, err := s.HandleWrite(ctx, "tr", MemoryWriteArgs{File: "ingest/c.md", Content: "---\nkahya_source_sha256: \"" + digest + "\"\n---\n"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(memDir, "ingest", "a.md"), filepath.Join(memDir, "ingest", "link.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.HandleWriteTiered(ctx, "tr", digestArgs("notes/a.md", digest, "2026-10-11T10:00:00Z", "normal", false)); err != nil {
		t.Fatal(err)
	}

	refs, err := s.FindIngestNotes(ctx, "tr", "ingest", digest)
	if err != nil {
		t.Fatalf("FindIngestNotes: %v", err)
	}
	if len(refs) != 2 || refs[0].File != newer.File || refs[1].File != older.File {
		t.Fatalf("refs = %+v, want [%s %s]", refs, newer.File, older.File)
	}
	if refs[0].Lane != "secret" || refs[0].LaneReason != "forced" || refs[0].CommitSHA != newer.CommitSHA || refs[1].CommitSHA != older.CommitSHA {
		t.Fatalf("refs = %+v", refs)
	}
}

// TestFindIngestNotesRecoversAJournalledNoteFirst: a note whose write a
// crash left uncommitted (its intent journalled) is committed as kahyad
// by the lookup's own recovery and reported with that commit.
func TestFindIngestNotesRecoversAJournalledNoteFirst(t *testing.T) {
	repoRoot, memDir := newFixtureRepo(t)
	s := newTestServer(memDir, &fakeSearcher{}, &fakeIndexer{episodeID: 1}, &fakeLedger{})
	const digest = "abc123"
	content := []byte("---\nkahya_source_tier: external_doc\nkahya_source_sha256: \"" + digest + "\"\n---\n# Belge\n")
	crashAfterWrite(t, s, repoRoot, "ingest/x.md", content)

	refs, err := s.FindIngestNotes(context.Background(), "tr", "ingest", digest)
	if err != nil {
		t.Fatalf("FindIngestNotes: %v", err)
	}
	if len(refs) != 1 || refs[0].File != "ingest/x.md" || refs[0].CommitSHA == "" {
		t.Fatalf("refs = %+v, want the recovered note with its commit", refs)
	}
	if author := runGitT(t, repoRoot, "log", "-1", "--format=%an", "--", "memory/ingest/x.md"); author != "kahyad" {
		t.Fatalf("note's author = %q, want kahyad", author)
	}
}
