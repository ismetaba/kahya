package server

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/text/unicode/norm"
)

// TestMemoryConfirmVariantSpellingKeepsOneEpisode (FIX-FINAL
// memory-confirm-nfd-duplicate-episode): through the control route with a
// real indexer, confirming a note by an NFD or case variant of its path
// promotes the existing episode instead of inserting a second active one
// next to it, and a case variant no longer fails with git's raw pathspec
// error.
func TestMemoryConfirmVariantSpellingKeepsOneEpisode(t *testing.T) {
	for _, tc := range []struct{ name, write, confirm string }{
		{"nfd", "notlar/Ayşe İş.md", norm.NFD.String("notlar/Ayşe İş.md")},
		{"case", "notlar/zeynep.md", "Notlar/Zeynep.md"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMCPTestFixture(t)
			writeAgentNote(t, f, tc.write, "Zeynep pazartesi Ankara ofisine taşınıyor.")
			if _, err := os.Lstat(filepath.Join(f.srv.mcpMemoryDir, filepath.FromSlash(tc.confirm))); err != nil {
				t.Skipf("volume is case/normalisation-sensitive: %v", err)
			}
			status, res := postMemoryConfirm(t, f.client, `{"file":"`+tc.confirm+`"}`)
			if status != http.StatusOK {
				t.Fatalf("confirm(%q) = %d %v, want 200", tc.confirm, status, res)
			}
			if res["file"] != tc.write {
				t.Errorf("confirmed file = %v, want the stored spelling %q", res["file"], tc.write)
			}
			var active int
			if err := f.store.DB().QueryRow(`SELECT count(*) FROM episodes WHERE status = 'active'`).Scan(&active); err != nil {
				t.Fatal(err)
			}
			if active != 1 {
				t.Errorf("active episodes for ONE note = %d, want 1", active)
			}
			var tier string
			if err := f.store.DB().QueryRow(`SELECT source_tier FROM episodes WHERE status = 'active'`).Scan(&tier); err != nil {
				t.Fatal(err)
			}
			if tier != "user_asserted" {
				t.Errorf("active episode tier = %q, want user_asserted", tier)
			}
		})
	}
}

// TestMemoryExtractFileVariantSpellingResolvesStoredName: `kahya memory
// extract --file` with a case variant of an existing note's path reaches
// ExtractFile with the stored spelling (it looks the episode up by exact
// source_path bytes and used to answer 404); a path that does not exist
// keeps its spelling.
func TestMemoryExtractFileVariantSpellingResolvesStoredName(t *testing.T) {
	memDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(memDir, "notlar"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memDir, "notlar", "zeynep.md"), []byte("not\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(memDir, "Notlar", "Zeynep.md")); err != nil {
		t.Skipf("volume is case-sensitive: %v", err)
	}
	e := &fakeMemoryExtractor{}
	client := newMemoryExtractTestServer(t, memDir, e)
	if status, res := postMemoryExtract(t, client, `{"file":"Notlar/Zeynep.md"}`); status != http.StatusOK {
		t.Fatalf("extract = %d %v, want 200", status, res)
	}
	if status, _ := postMemoryExtract(t, client, `{"file":"notlar/yok.md"}`); status != http.StatusOK {
		t.Fatalf("extract of a missing note = %d, want the fake's 200", status)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.calls) != 2 || e.calls[0] != "file:notlar/zeynep.md" || e.calls[1] != "file:notlar/yok.md" {
		t.Errorf("ExtractFile calls = %q, want the stored spelling, then the missing path unchanged", e.calls)
	}
}
