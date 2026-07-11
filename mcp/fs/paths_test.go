package fs

import (
	"os"
	"path/filepath"
	"testing"
)

// testHome returns t.TempDir(), resolved through filepath.EvalSymlinks —
// on macOS, t.TempDir()'s own path lives under /var/folders, which is
// ITSELF a symlink to /private/var/folders (the same gotcha
// mcp/memory/server_test.go documents and works around). Canonicalize's
// own resolveDeepestExisting resolves through that symlink too, so a
// test comparing an expected Op/Match string against a RAW,
// un-resolved t.TempDir() value would spuriously fail on this platform
// even though the code under test is correct.
func testHome(t *testing.T) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(t.TempDir()): %v", err)
	}
	return resolved
}

func TestCanonicalizeRejectsEmpty(t *testing.T) {
	if _, err := Canonicalize(testHome(t), "   "); err == nil {
		t.Fatal("Canonicalize(empty) = nil error, want error")
	}
}

func TestCanonicalizeExpandsHome(t *testing.T) {
	home := testHome(t)
	cp, err := Canonicalize(home, "~/notes/todo.md")
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	want := filepath.Join(home, "notes", "todo.md")
	if cp.Op != want {
		t.Errorf("Op = %q, want %q", cp.Op, want)
	}
	if cp.Match != want {
		t.Errorf("Match = %q, want %q", cp.Match, want)
	}
}

func TestCanonicalizeBareTilde(t *testing.T) {
	home := testHome(t)
	cp, err := Canonicalize(home, "~")
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	if cp.Op != home {
		t.Errorf("Op = %q, want %q", cp.Op, home)
	}
}

// TestCanonicalizeCollapsesDotDotBypass is the task's own "dotdot" bypass
// fixture: ~/Library/LaunchAgents/../LaunchAgents/evil.plist must
// canonicalize to the EXACT same result as ~/Library/LaunchAgents/
// evil.plist, so a deny-glob match against the canonical form can never
// be dodged by a textual ".." detour.
func TestCanonicalizeCollapsesDotDotBypass(t *testing.T) {
	home := testHome(t)
	mustMkdirAll(t, filepath.Join(home, "Library", "LaunchAgents"))

	dotdot, err := Canonicalize(home, "~/Library/LaunchAgents/../LaunchAgents/evil.plist")
	if err != nil {
		t.Fatalf("Canonicalize(dotdot): %v", err)
	}
	direct, err := Canonicalize(home, "~/Library/LaunchAgents/evil.plist")
	if err != nil {
		t.Fatalf("Canonicalize(direct): %v", err)
	}
	if dotdot.Match != direct.Match {
		t.Errorf("dotdot canonical = %q, direct canonical = %q, want equal", dotdot.Match, direct.Match)
	}

	deny := []string{filepath.Join(home, "Library", "LaunchAgents", "**")}
	hit, err := MatchesAnyGlobCI(dotdot.Match, deny)
	if err != nil {
		t.Fatalf("MatchesAnyGlobCI: %v", err)
	}
	if !hit {
		t.Error("dotdot-canonicalized path did not match the LaunchAgents deny glob, want match (bypass would succeed)")
	}
}

// TestCanonicalizeResolvesSymlinkBypass is the task's own symlink bypass
// fixture: a symlink ~/tmp/lnk -> ~/.zshrc must canonicalize to the SAME
// result as ~/.zshrc directly, so writing through the symlink can never
// dodge the ~/.zshrc deny glob.
func TestCanonicalizeResolvesSymlinkBypass(t *testing.T) {
	home := testHome(t)
	zshrc := filepath.Join(home, ".zshrc")
	mustWriteFile(t, zshrc, "original zshrc contents\n")
	mustMkdirAll(t, filepath.Join(home, "tmp"))
	link := filepath.Join(home, "tmp", "lnk")
	if err := os.Symlink(zshrc, link); err != nil {
		t.Fatalf("os.Symlink: %v", err)
	}

	viaLink, err := Canonicalize(home, "~/tmp/lnk")
	if err != nil {
		t.Fatalf("Canonicalize(via symlink): %v", err)
	}
	direct, err := Canonicalize(home, "~/.zshrc")
	if err != nil {
		t.Fatalf("Canonicalize(direct): %v", err)
	}
	if viaLink.Match != direct.Match {
		t.Errorf("symlink canonical = %q, direct canonical = %q, want equal", viaLink.Match, direct.Match)
	}

	deny := []string{filepath.Join(home, ".zshrc")}
	hit, err := MatchesAnyGlobCI(viaLink.Match, deny)
	if err != nil {
		t.Fatalf("MatchesAnyGlobCI: %v", err)
	}
	if !hit {
		t.Error("symlink-canonicalized path did not match the .zshrc deny glob, want match (bypass would succeed)")
	}
}

// TestCanonicalizeSymlinkOnNonExistentLeaf covers a fs_write TARGET that
// does not exist yet, one directory below a symlinked directory — proving
// resolveDeepestExisting resolves the deepest EXISTING ancestor (the
// symlinked directory) and appends the not-yet-existing leaf verbatim,
// rather than failing outright the way a bare filepath.EvalSymlinks(full
// path) would on a nonexistent path.
func TestCanonicalizeSymlinkOnNonExistentLeaf(t *testing.T) {
	home := testHome(t)
	real := filepath.Join(home, "real-dir")
	mustMkdirAll(t, real)
	link := filepath.Join(home, "linked-dir")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("os.Symlink: %v", err)
	}

	cp, err := Canonicalize(home, "~/linked-dir/brand-new-file.txt")
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	want := filepath.Join(real, "brand-new-file.txt")
	if cp.Op != want {
		t.Errorf("Op = %q, want %q", cp.Op, want)
	}
}

// TestNFDEncodedSegmentMatchesNFCGlob is the task's own NFD-encoded-
// variant bypass fixture, generalized: policy.yaml deny globs are stored
// (and, per HANDOFF §7, typically authored) in NFC form, but APFS can
// store — and a caller can supply — the SAME visible path with a
// combining-character (NFD) spelling of an accented segment. Canonicalize
// must fold both spellings onto the identical Match string so deny-glob
// matching (which runs ONLY on that canonical result) cannot be dodged by
// choosing the decomposed encoding. "Ö" (U+00D6) is used here as the
// concrete accented character (it has a real, distinct NFD decomposition
// O + COMBINING DIAERESIS, unlike some Turkish letters unique to that
// alphabet which have no canonical decomposition at all) under a path
// shaped like the task's own "~/Library/Application Support/Kahya/..."
// fixture description.
func TestNFDEncodedSegmentMatchesNFCGlob(t *testing.T) {
	home := testHome(t)
	const nfcSegment = "Ö"  // U+00D6, single precomposed code point
	const nfdSegment = "Ö" // "O" + U+0308 COMBINING DIAERESIS

	nfcRaw := "~/Library/Application Support/Kahya/" + nfcSegment + "/secret.txt"
	nfdRaw := "~/Library/Application Support/Kahya/" + nfdSegment + "/secret.txt"

	nfc, err := Canonicalize(home, nfcRaw)
	if err != nil {
		t.Fatalf("Canonicalize(nfc): %v", err)
	}
	nfd, err := Canonicalize(home, nfdRaw)
	if err != nil {
		t.Fatalf("Canonicalize(nfd): %v", err)
	}
	if nfc.Match != nfd.Match {
		t.Fatalf("NFC canonical = %q (% x), NFD canonical = %q (% x), want equal after normalization",
			nfc.Match, []byte(nfc.Match), nfd.Match, []byte(nfd.Match))
	}

	deny := []string{filepath.Join(home, "Library", "Application Support", "Kahya", "**")}
	hit, err := MatchesAnyGlobCI(nfd.Match, deny)
	if err != nil {
		t.Fatalf("MatchesAnyGlobCI: %v", err)
	}
	if !hit {
		t.Error("NFD-encoded path did not match the App Support/Kahya deny glob, want match (bypass would succeed)")
	}
}

func TestCanonicalizeRejectsZeroWidthRune(t *testing.T) {
	home := testHome(t)
	if _, err := Canonicalize(home, "~/notes/todo​.md"); err == nil {
		t.Fatal("Canonicalize with a zero-width space = nil error, want error")
	}
}

func TestCanonicalizeRejectsBidiOverrideRune(t *testing.T) {
	home := testHome(t)
	if _, err := Canonicalize(home, "~/notes/todo‮txt.md"); err == nil {
		t.Fatal("Canonicalize with a RIGHT-TO-LEFT OVERRIDE rune = nil error, want error")
	}
}

// TestMatchesAnyGlobCICaseInsensitive is the task's own case-insensitivity
// fixture: ~/LIBRARY/LaunchAgents/x.plist must deny exactly like its
// lowercase form (APFS is case-insensitive but case-preserving).
func TestMatchesAnyGlobCICaseInsensitive(t *testing.T) {
	home := testHome(t)
	cp, err := Canonicalize(home, "~/LIBRARY/LaunchAgents/x.plist")
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	deny := []string{filepath.Join(home, "Library", "LaunchAgents", "**")}
	hit, err := MatchesAnyGlobCI(cp.Match, deny)
	if err != nil {
		t.Fatalf("MatchesAnyGlobCI: %v", err)
	}
	if !hit {
		t.Error("uppercase-cased path did not match the (mixed-case) deny glob, want match")
	}
}

func TestMatchesAnyGlobCINoMatch(t *testing.T) {
	home := testHome(t)
	cp, err := Canonicalize(home, "~/Documents/report.txt")
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	deny := []string{filepath.Join(home, "Library", "LaunchAgents", "**")}
	hit, err := MatchesAnyGlobCI(cp.Match, deny)
	if err != nil {
		t.Fatalf("MatchesAnyGlobCI: %v", err)
	}
	if hit {
		t.Error("unrelated path matched the LaunchAgents deny glob, want no match")
	}
}

func mustMkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll(%s): %v", dir, err)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	mustMkdirAll(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}
