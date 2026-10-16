package fs

// apfs_fold_test.go (FIX-FINAL attacker2-apfs-fold-glob-bypass): macOS APFS
// opens "~/.z\u017fhrc" (U+017F LATIN SMALL LETTER LONG S) as ~/.zshrc, and
// likewise sharp s for "ss" and the ff/fi/fl/ffi/ffl/st ligatures - full
// Unicode case folding, which strings.ToLower does not do. Canonicalize
// keeps the caller's spelling, so a lower-cased deny or secret-lane glob
// missed the very file the call opened.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// requireCaseInsensitiveVolume skips when dir's volume is case-sensitive:
// there an APFS-folded spelling names a different (absent) file, so an
// end-to-end read through it has nothing to open.
func requireCaseInsensitiveVolume(t *testing.T, dir string) {
	t.Helper()
	probe := filepath.Join(dir, "case-probe")
	mustWriteFile(t, probe, "x")
	if _, err := os.Stat(filepath.Join(dir, "CASE-PROBE")); err != nil {
		t.Skip("volume is case-sensitive; APFS folding does not apply")
	}
	if err := os.Remove(probe); err != nil {
		t.Fatal(err)
	}
}

// TestHandleWriteDeniesAPFSFoldedSpellingsOfDenyGlobs: every write the
// attacker probe landed - an existing ~/.zshrc, a not-yet-existing one
// (APFS folds both ways, so the created name IS zsh's .zshrc), a new
// LaunchAgents plist, ~/.ssh/authorized_keys and kahyad's own brain.db -
// is refused before any policy decision, with an fs_deny_glob_hit row, and
// the real file keeps its bytes.
func TestHandleWriteDeniesAPFSFoldedSpellingsOfDenyGlobs(t *testing.T) {
	home := testHome(t)
	denyGlobs := []string{
		filepath.Join(home, ".zshrc"),
		filepath.Join(home, "Library", "LaunchAgents", "**"),
		filepath.Join(home, ".ssh", "**"),
		filepath.Join(home, "Library", "Application Support", "Kahya", "**"),
		filepath.Join(home, ".config", "git", "**"),
		filepath.Join(home, "code", "x", "sitecustomize.py"),
	}
	cases := []struct {
		target   string // the attacker's spelling
		existing string // the real file it opens, "" when it does not exist yet
	}{
		{"~/.z\u017fhrc", filepath.Join(home, ".zshrc")},
		{"~/Library/LaunchAgent\u017f/evil.plist", ""},
		{"~/.\u017fsh/authorized_keys", filepath.Join(home, ".ssh", "authorized_keys")},
		{"~/Library/Application \u017fupport/Kahya/brain.db", filepath.Join(home, "Library", "Application Support", "Kahya", "brain.db")},
		{"~/.con\ufb01g/git/config", ""},
		{"~/code/x/\u017fitecu\ufb06omize.py", ""},
		{"~/LIBRARY/LaunchAgents/x.plist", ""}, // plain case variant (the pre-fix guarantee)
	}
	for _, tc := range cases {
		t.Run(tc.target, func(t *testing.T) {
			if tc.existing != "" {
				mustWriteFile(t, tc.existing, "original")
			}
			pc := &fakePolicyClient{decision: allowDecision("shouldneverbeused")}
			led := &fakeLedger{}
			s := newTestServer(t, home, denyGlobs, nil, pc, led)
			_, err := s.HandleWrite(context.Background(), "trace-fold", "task-1", FsWriteArgs{Path: tc.target, ContentBase64: b64("evil")})
			if err == nil {
				t.Fatalf("HandleWrite(%q) = nil error, want a deny-glob refusal", tc.target)
			}
			if pc.checkCalls != 0 {
				t.Errorf("Policy.Check called %d times, want 0", pc.checkCalls)
			}
			if got := len(led.find("fs_deny_glob_hit")); got != 1 {
				t.Fatalf("fs_deny_glob_hit rows = %d, want 1", got)
			}
			if tc.existing != "" {
				if b, err := os.ReadFile(tc.existing); err != nil || string(b) != "original" {
					t.Errorf("%s = %q (err %v), want unchanged", tc.existing, b, err)
				}
			}
		})
	}
}

// TestHandleDeleteDeniesAPFSFoldedSpellingOfDenyGlob: fs_delete through the
// long-s spelling of ~/Library/LaunchAgents is refused; the plist stays.
func TestHandleDeleteDeniesAPFSFoldedSpellingOfDenyGlob(t *testing.T) {
	home := testHome(t)
	target := filepath.Join(home, "Library", "LaunchAgents", "com.user.plist")
	mustWriteFile(t, target, "payload")
	pc := &fakePolicyClient{decision: allowDecision("tok")}
	led := &fakeLedger{}
	s := newTestServer(t, home, []string{filepath.Join(home, "Library", "LaunchAgents", "**")}, nil, pc, led)
	if _, err := s.HandleDelete(context.Background(), "trace-fold-del", "task-1", FsDeleteArgs{Path: "~/Library/LaunchAgent\u017f/com.user.plist"}); err == nil {
		t.Fatal("HandleDelete through a long-s spelling = nil error, want a deny-glob refusal")
	}
	if pc.checkCalls != 0 || len(led.find("fs_deny_glob_hit")) != 1 {
		t.Fatalf("checkCalls=%d fs_deny_glob_hit=%d, want 0 and 1", pc.checkCalls, len(led.find("fs_deny_glob_hit")))
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("the plist was moved despite the deny glob: %v", err)
	}
}

// TestHandleReadAPFSFoldedSpellingOfSecretGlobEscalates: fs_read of a
// secret-glob file through its long-s / ligature spelling returns
// secret_lane=true, marks the session and escalates the task exactly like
// the plain spelling.
func TestHandleReadAPFSFoldedSpellingOfSecretGlobEscalates(t *testing.T) {
	home := testHome(t)
	requireCaseInsensitiveVolume(t, home)
	mustWriteFile(t, filepath.Join(home, "Documents", "saglik", "tahlil.md"), "Kontrol 14 Kasim.\n")
	mustWriteFile(t, filepath.Join(home, "Documents", "kimlik", "ofis.md"), "Ofis notu.\n")
	globs := []string{filepath.Join(home, "Documents", "saglik", "**"), filepath.Join(home, "Documents", "kimlik", "**")}
	for _, spelling := range []string{
		"~/Documents/saglik/tahlil.md",
		"~/Documents/\u017faglik/tahlil.md",
		"~/Document\u017f/saglik/tahlil.md",
		"~/Documents/kimlik/o\ufb01s.md",
		"~/Documents/\u212aimlik/ofis.md",
	} {
		t.Run(spelling, func(t *testing.T) {
			marker := &fakeSensitiveMarker{}
			esc := &fakeLaneEscalator{}
			s := newTestServer(t, home, nil, globs, &fakePolicyClient{decision: PolicyDecision{Result: PolicyResultAllow, Class: "R"}}, &fakeLedger{})
			s.SensitiveMarker, s.LaneEscalator = marker, esc
			out, err := s.HandleReadForTask(context.Background(), "trace-fold-read", "task-1", FsReadArgs{Path: spelling})
			if err != nil {
				t.Fatalf("HandleReadForTask(%q): %v", spelling, err)
			}
			if !out.SecretLane || marker.callCount() != 1 || len(esc.snapshot()) != 1 {
				t.Fatalf("%q: secret_lane=%v marks=%d escalations=%d, want true/1/1", spelling, out.SecretLane, marker.callCount(), len(esc.snapshot()))
			}
		})
	}
}

// TestHandleReadSecretGlobErrorFailsClosed: a secret-lane glob that cannot
// be evaluated makes the read secret-lane (it used to fall through to the
// content check alone).
func TestHandleReadSecretGlobErrorFailsClosed(t *testing.T) {
	home := testHome(t)
	mustWriteFile(t, filepath.Join(home, "notes", "a.md"), "temiz\n")
	esc := &fakeLaneEscalator{}
	s := newTestServer(t, home, nil, []string{filepath.Join(home, "notes", "[")}, &fakePolicyClient{decision: PolicyDecision{Result: PolicyResultAllow, Class: "R"}}, &fakeLedger{})
	s.LaneEscalator = esc
	out, err := s.HandleReadForTask(context.Background(), "trace-glob-err", "task-1", FsReadArgs{Path: "~/notes/a.md"})
	if err != nil {
		t.Fatalf("HandleReadForTask: %v", err)
	}
	if !out.SecretLane || len(esc.snapshot()) != 1 {
		t.Fatalf("secret_lane=%v escalations=%d, want true/1 on a glob error", out.SecretLane, len(esc.snapshot()))
	}
}

// TestPathCoversSecretGlobFoldsAncestorSpellings: a repo or workdir spelled
// with an APFS-folded name still holds the secret dir below it.
func TestPathCoversSecretGlobFoldsAncestorSpellings(t *testing.T) {
	home := testHome(t)
	globs := []string{filepath.Join(home, "Documents", "saglik", "**")}
	for _, p := range []string{
		filepath.Join(home, "Document\u017f"),
		filepath.Join(home, "Documents", "\u017faglik"),
		filepath.Join(home, "Documents", "\u017faglik", "x.md"),
	} {
		if !PathCoversSecretGlob(home, p, globs) {
			t.Errorf("PathCoversSecretGlob(%q) = false, want true", strings.TrimPrefix(p, home))
		}
	}
	if PathCoversSecretGlob(home, filepath.Join(home, "Downloads"), globs) {
		t.Error("an unrelated directory is covered")
	}
}
