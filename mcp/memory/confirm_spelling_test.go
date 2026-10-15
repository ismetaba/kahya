package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/text/unicode/norm"
)

// TestHandleConfirmUsesStoredSpelling (FIX-FINAL memory-confirm-nfd-
// duplicate-episode): on APFS an NFD/NFC or case variant of a note's path
// opens the same file, but the indexer keys episodes on the exact
// source_path bytes. HandleConfirm used the user's spelling verbatim, so
// the NFD variant reindexed a SECOND episode next to the stored one and
// the case variant failed with git's raw pathspec error. It now resolves
// the name the volume stores and uses only that.
func TestHandleConfirmUsesStoredSpelling(t *testing.T) {
	for _, tc := range []struct{ name, write, confirm string }{
		{"nfd", "notlar/Ayşe İş.md", norm.NFD.String("notlar/Ayşe İş.md")},
		{"case", "notlar/zeynep.md", "Notlar/Zeynep.md"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repoRoot, memDir := newFixtureRepo(t)
			idx := &fakeIndexer{episodeID: 7}
			ledger := &fakeLedger{}
			s := newTestServer(memDir, &fakeSearcher{}, idx, ledger)
			if _, err := s.HandleWrite(context.Background(), "tw", MemoryWriteArgs{File: tc.write, Content: "Zeynep pazartesi Ankara ofisine taşınıyor."}); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(filepath.Join(memDir, filepath.FromSlash(tc.confirm))); err != nil {
				t.Skipf("volume is case/normalisation-sensitive (%v): the variant is a different file here", err)
			}
			idx.calls, ledger.events = nil, nil

			out, err := s.HandleConfirm(context.Background(), "tc", MemoryConfirmArgs{File: tc.confirm})
			if err != nil {
				t.Fatalf("HandleConfirm(%q): %v", tc.confirm, err)
			}
			if out.File != tc.write {
				t.Errorf("out.File = %q, want the stored spelling %q", out.File, tc.write)
			}
			if len(idx.calls) != 1 || idx.calls[0] != tc.write {
				t.Errorf("ReindexFile calls = %q, want exactly the stored spelling %q", idx.calls, tc.write)
			}
			if len(ledger.events) != 1 || ledger.events[0].payload["file"] != tc.write {
				t.Errorf("ledger = %+v, want file %q", ledger.events, tc.write)
			}
			if got := runGitT(t, repoRoot, "log", "-1", "--format=%s"); got != "memory_confirm: "+tc.write {
				t.Errorf("last commit subject = %q, want the stored spelling", got)
			}
			after, err := os.ReadFile(filepath.Join(memDir, filepath.FromSlash(tc.write)))
			if err != nil || !strings.Contains(string(after), "kahya_source_tier: user_asserted") {
				t.Errorf("stored note not promoted: %q (%v)", after, err)
			}
		})
	}
}
