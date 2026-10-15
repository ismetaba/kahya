package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tieredArgs(rel string, secret bool) TieredWriteArgs {
	return TieredWriteArgs{
		RelPath: rel, Tier: "external_doc", SecretLane: secret, CommitPrefix: "ingest",
		FrontMatter: []FrontMatterField{{Key: "kahya_source_ref", Value: "rapor: #1.txt"}, {Key: "kahya_lane", Value: "normal"}},
		Body:        "# Rapor\n\nÖzet.\n",
	}
}

// TestHandleWriteTieredWritesExternalDocNote (G13 MEM-6): the ingest note
// carries the CALL SITE's tier and its provenance keys (quoted, so the
// indexer's YAML parse survives a ':'/'#' in a label), is committed by
// kahyad as "ingest: <path>", reindexed and ledgered; a taken name gets
// "-2" (an existing note is never overwritten).
func TestHandleWriteTieredWritesExternalDocNote(t *testing.T) {
	repoRoot, memDir := newFixtureRepo(t)
	idx := &fakeIndexer{episodeID: 9}
	ledger := &fakeLedger{}
	s := newTestServer(memDir, &fakeSearcher{}, idx, ledger)
	out, err := s.HandleWriteTiered(context.Background(), "ti", tieredArgs("ingest/2026-10-08-rapor.md", false))
	if err != nil {
		t.Fatalf("HandleWriteTiered: %v", err)
	}
	if out.File != "ingest/2026-10-08-rapor.md" || out.EpisodeID != 9 || out.CommitSHA == "" {
		t.Fatalf("out = %+v", out)
	}
	got, _ := os.ReadFile(filepath.Join(memDir, "ingest", "2026-10-08-rapor.md"))
	want := "---\nkahya_source_tier: external_doc\nkahya_source_ref: \"rapor: #1.txt\"\nkahya_lane: \"normal\"\n---\n# Rapor\n\nÖzet.\n"
	if string(got) != want {
		t.Fatalf("note =\n%q\nwant\n%q", got, want)
	}
	if c := runGitT(t, repoRoot, "log", "-1", "--format=%an <%ae>|%s"); c != "kahyad <kahyad@local>|ingest: ingest/2026-10-08-rapor.md" {
		t.Fatalf("commit = %q", c)
	}
	if len(ledger.events) != 1 || ledger.events[0].kind != EventMemoryWriteTiered || ledger.events[0].payload["tier"] != "external_doc" {
		t.Fatalf("ledger = %+v", ledger.events)
	}
	again, err := s.HandleWriteTiered(context.Background(), "ti", tieredArgs("ingest/2026-10-08-rapor.md", false))
	if err != nil || again.File != "ingest/2026-10-08-rapor-2.md" {
		t.Fatalf("second write = %+v %v, want the -2 name", again, err)
	}
	if first, _ := os.ReadFile(filepath.Join(memDir, "ingest", "2026-10-08-rapor.md")); string(first) != want {
		t.Fatal("the first note was overwritten")
	}
}

// TestHandleWriteTieredSecretLaneNeedsCoveringGlob: a secret-lane ingest
// note lands under the secret-lane dir ONLY when a secret glob covers it;
// otherwise nothing is written (fail-closed - an uncovered secret note
// would look normal to every path-based lane rule).
func TestHandleWriteTieredSecretLaneNeedsCoveringGlob(t *testing.T) {
	_, memDir := newFixtureRepo(t)
	s := newTestServer(memDir, &fakeSearcher{}, &fakeIndexer{episodeID: 4}, &fakeLedger{})
	s.SetSecretLaneSeam(SecretLaneSeam{Dir: "gizli", Matcher: prefixGlobMatcher{}})
	if _, err := s.HandleWriteTiered(context.Background(), "ti", tieredArgs("ingest/x.md", true)); !errors.Is(err, ErrTieredSecretGlobMissing) {
		t.Fatalf("no glob: err = %v, want ErrTieredSecretGlobMissing", err)
	}
	s.SetSecretLaneSeam(SecretLaneSeam{Dir: "gizli", Globs: []string{filepath.Join(memDir, "gizli", "**")}})
	if _, err := s.HandleWriteTiered(context.Background(), "ti", tieredArgs("ingest/x.md", true)); !errors.Is(err, ErrTieredSecretGlobMissing) {
		t.Fatalf("no matcher: err = %v, want ErrTieredSecretGlobMissing", err)
	}
	s.SetSecretLaneSeam(SecretLaneSeam{Dir: "gizli", Globs: []string{filepath.Join(memDir, "gizli", "**")}, Matcher: erroringGlobMatcher{}})
	if _, err := s.HandleWriteTiered(context.Background(), "ti", tieredArgs("ingest/x.md", true)); !errors.Is(err, ErrTieredSecretGlobMissing) {
		t.Fatalf("glob error: err = %v, want ErrTieredSecretGlobMissing (a glob error is no coverage)", err)
	}
	assertNotExist(t, filepath.Join(memDir, "gizli"))

	s.SetSecretLaneSeam(secretSeam(memDir, nil, nil, nil))
	out, err := s.HandleWriteTiered(context.Background(), "ti", tieredArgs("ingest/x.md", true))
	if err != nil || out.File != "gizli/ingest/x.md" {
		t.Fatalf("covered = %+v %v, want gizli/ingest/x.md", out, err)
	}
}

// TestHandleWriteTieredRefusesTrustedTierAndBadFields: the writer accepts
// only the untrusted-ingested tiers and well-formed fields - it can never
// mint a user tier or smuggle a second tier / confirmation key.
func TestHandleWriteTieredRefusesTrustedTierAndBadFields(t *testing.T) {
	_, memDir := newFixtureRepo(t)
	s := newTestServer(memDir, &fakeSearcher{}, &fakeIndexer{episodeID: 1}, &fakeLedger{})
	for name, mutate := range map[string]func(*TieredWriteArgs){
		"user tier":  func(a *TieredWriteArgs) { a.Tier = "user_asserted" },
		"agent tier": func(a *TieredWriteArgs) { a.Tier = "agent_derived" },
		"second tier key": func(a *TieredWriteArgs) {
			a.FrontMatter = []FrontMatterField{{Key: "kahya_source_tier", Value: "user_edit"}}
		},
		"confirmed key": func(a *TieredWriteArgs) { a.FrontMatter = []FrontMatterField{{Key: "kahya_confirmed_at", Value: "x"}} },
		"bad key":       func(a *TieredWriteArgs) { a.FrontMatter = []FrontMatterField{{Key: "Kaynak Adı", Value: "x"}} },
		"multi-line value": func(a *TieredWriteArgs) {
			a.FrontMatter = []FrontMatterField{{Key: "kahya_source_ref", Value: "a\nkahya_source_tier: user_edit"}}
		},
		"not markdown": func(a *TieredWriteArgs) { a.RelPath = "ingest/x.txt" },
		"escape":       func(a *TieredWriteArgs) { a.RelPath = "../x.md" },
		"absolute":     func(a *TieredWriteArgs) { a.RelPath = "/tmp/x.md" },
	} {
		a := tieredArgs("ingest/x.md", false)
		mutate(&a)
		if _, err := s.HandleWriteTiered(context.Background(), "ti", a); !errors.Is(err, ErrTieredInvalid) {
			t.Errorf("%s: err = %v, want ErrTieredInvalid", name, err)
		}
	}
	assertNotExist(t, filepath.Join(memDir, "ingest"))
}

// TestHandleWriteTieredGitFailureRemovesFile: a failing commit leaves no
// orphan note (no bytes on disk without commit + reindex + ledger).
func TestHandleWriteTieredGitFailureRemovesFile(t *testing.T) {
	repoRoot, memDir := newFixtureRepo(t)
	installFailingPreCommitHook(t, repoRoot)
	idx := &fakeIndexer{episodeID: 1}
	s := newTestServer(memDir, &fakeSearcher{}, idx, &fakeLedger{})
	if _, err := s.HandleWriteTiered(context.Background(), "ti", tieredArgs("ingest/x.md", false)); err == nil {
		t.Fatal("HandleWriteTiered with a failing commit = nil error")
	}
	assertNotExist(t, filepath.Join(memDir, "ingest", "x.md"))
	if st := runGitT(t, repoRoot, "status", "--porcelain", "--untracked-files=all"); len(idx.calls) != 0 || strings.Contains(st, "x.md") {
		t.Fatalf("reindex calls %v / git status %q after a failed commit", idx.calls, st)
	}
}
