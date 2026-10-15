package memory

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// FIX-U-3 review: on a case-/normalisation-insensitive volume (macOS APFS,
// the production volume) an existing "Gizli/" swallows a write to
// "gizli/notes/...", and every downstream walker then reports the stored
// "Gizli/..." name - one no byte-exact secret_lane_globs entry matches.
// memory_write checks glob coverage on the STORED name, before and after
// the write. On a case-sensitive volume the two names are different
// directories and the note lands byte-exact under gizli/; each test asserts
// whichever behaviour the test volume implies.

// caseInsensitiveVolume reports whether the volume holding t.TempDir()
// (where newFixtureRepo puts the memory dir) folds name case.
func caseInsensitiveVolume(t *testing.T) bool {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "case-probe"), nil, 0o600); err != nil {
		t.Fatalf("case probe: %v", err)
	}
	_, err := os.Stat(filepath.Join(dir, "CASE-PROBE"))
	return err == nil
}

// regularFiles lists every regular file under memDir (forward-slash,
// relative), skipping dot-directories as the walkers do.
func regularFiles(t *testing.T, memDir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(memDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && p != memDir && strings.HasPrefix(d.Name(), ".") {
			return fs.SkipDir
		}
		if d.Type().IsRegular() {
			rel, _ := filepath.Rel(memDir, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", memDir, err)
	}
	return out
}

// TestHandleWriteSecretRefusesCaseVariantSecretDir is the review's repro: a
// "Gizli/" directory exists (left by an agent's write+forget, or made by
// the user), and a secret memory_write redirected to gizli/notes/... would
// be stored as Gizli/notes/... - uncovered by the gizli/** glob. The write
// is refused (nothing on disk, nothing committed, nothing reindexed); the
// task is still escalated (the agent holds the bytes).
func TestHandleWriteSecretRefusesCaseVariantSecretDir(t *testing.T) {
	repoRoot, memDir := newFixtureRepo(t)
	seedCommit(t, repoRoot)
	commitsBefore := runGitT(t, repoRoot, "rev-list", "--count", "HEAD")
	if err := os.Mkdir(filepath.Join(memDir, "Gizli"), 0o700); err != nil {
		t.Fatalf("mkdir Gizli: %v", err)
	}
	led := &fakeLedger{}
	esc := &fakeLaneEscalator{}
	idx := &fakeIndexer{episodeID: 3}
	s := newTestServer(memDir, &fakeSearcher{}, idx, led)
	s.SetSecretLaneSeam(secretSeam(memDir, &fakeContentClassifier{secret: true, category: "finans"}, nil, esc))

	out, err := s.HandleWriteForTask(context.Background(), "trace-ci", "task-ci", MemoryWriteArgs{Content: u3IBAN, File: "inbox/c.md"})
	if !caseInsensitiveVolume(t) {
		if err != nil || out.File != "gizli/notes/inbox/c.md" {
			t.Fatalf("case-sensitive volume: out = %+v, err = %v; want the note byte-exact under gizli/notes/inbox/c.md", out, err)
		}
		if _, err := os.Stat(filepath.Join(memDir, "gizli", "notes", "inbox", "c.md")); err != nil {
			t.Fatalf("case-sensitive volume: redirected note: %v", err)
		}
		return
	}
	if err == nil || err.Error() != MsgMemoryWriteSecretLaneRefused {
		t.Fatalf("out = %+v, err = %v; want the write refused with %q", out, err, MsgMemoryWriteSecretLaneRefused)
	}
	if !reflect.DeepEqual(esc.calls, []laneCall{{"task-ci", "trace-ci", "finans"}}) {
		t.Errorf("escalations = %v, want the task escalated once anyway", esc.calls)
	}
	if files := regularFiles(t, memDir); len(files) != 0 {
		t.Errorf("files under the memory dir = %v, want none", files)
	}
	if got := runGitT(t, repoRoot, "rev-list", "--count", "HEAD"); got != commitsBefore {
		t.Errorf("commit count = %s, want %s (nothing committed)", got, commitsBefore)
	}
	if len(idx.calls) != 0 {
		t.Errorf("reindex calls = %v, want none", idx.calls)
	}
	if got := eventKinds(led); !reflect.DeepEqual(got, []string{EventMemoryWriteSecretLaneRefused}) {
		t.Fatalf("ledger kinds = %v, want one %s", got, EventMemoryWriteSecretLaneRefused)
	}
	p := led.events[0].payload
	if p["reason"] != "secret_dir_case_mismatch" || p["file"] != "gizli/notes/inbox/c.md" || p["on_disk_file"] != "Gizli/notes/inbox/c.md" {
		t.Errorf("refusal payload = %v, want reason secret_dir_case_mismatch, file gizli/notes/inbox/c.md, on_disk_file Gizli/notes/inbox/c.md", p)
	}
}

// TestHandleWriteSecretAdoptsStoredNameWhenStillCovered: a stored name that
// differs only BELOW the covered gizli/ directory ("Inbox" for "inbox") is
// still secret for every rule; the note's path becomes the stored name, so
// git, the index and the ledger agree with what the walkers will report.
func TestHandleWriteSecretAdoptsStoredNameWhenStillCovered(t *testing.T) {
	repoRoot, memDir := newFixtureRepo(t)
	if err := os.MkdirAll(filepath.Join(memDir, "gizli", "notes", "Inbox"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	led := &fakeLedger{}
	idx := &fakeIndexer{episodeID: 4}
	s := newTestServer(memDir, &fakeSearcher{}, idx, led)
	s.SetSecretLaneSeam(secretSeam(memDir, &fakeContentClassifier{secret: true, category: "finans"}, nil, &fakeLaneEscalator{}))

	out, err := s.HandleWriteForTask(context.Background(), "trace-ad", "task-ad", MemoryWriteArgs{Content: u3IBAN, File: "inbox/a.md"})
	if err != nil {
		t.Fatalf("HandleWriteForTask: %v", err)
	}
	want := "gizli/notes/inbox/a.md"
	if caseInsensitiveVolume(t) {
		want = "gizli/notes/Inbox/a.md"
	}
	if out.File != want || !out.SecretLane {
		t.Fatalf("out = %+v, want File %s, SecretLane true", out, want)
	}
	if files := regularFiles(t, memDir); !reflect.DeepEqual(files, []string{want}) {
		t.Fatalf("files under the memory dir = %v, want exactly [%s] (the stored name)", files, want)
	}
	if !reflect.DeepEqual(idx.calls, []string{want}) {
		t.Errorf("reindex calls = %v, want [%s]", idx.calls, want)
	}
	if msg := runGitT(t, repoRoot, "log", "-1", "--format=%s"); msg != "memory_write: "+want {
		t.Errorf("commit subject = %q, want memory_write: %s", msg, want)
	}
	if tracked := runGitT(t, repoRoot, "ls-files"); tracked != "memory/"+want {
		t.Errorf("git ls-files = %q, want memory/%s", tracked, want)
	}
	r := led.events[len(led.events)-1].payload
	if r["requested_file"] != "inbox/a.md" || r["file"] != want {
		t.Errorf("redirect payload = %v, want requested inbox/a.md -> %s", r, want)
	}
}

// raceMatcher simulates a directory created concurrently between
// memory_write's pre-write name check and the write: its first Match call
// (the coverage check, which runs after the pre-write name check) creates
// mkdir, then it matches like prefixGlobMatcher.
type raceMatcher struct {
	mkdir string
	done  *bool
}

func (m raceMatcher) Match(pattern, p string) (bool, error) {
	if !*m.done {
		*m.done = true
		if err := os.Mkdir(m.mkdir, 0o700); err != nil {
			return false, err
		}
	}
	return prefixGlobMatcher{}.Match(pattern, p)
}

// TestHandleWriteSecretPostWriteNameCheckUndoesRacedWrite: the post-write
// check catches a case-variant directory that appeared after the pre-write
// check - the bytes are removed again and the write is refused.
func TestHandleWriteSecretPostWriteNameCheckUndoesRacedWrite(t *testing.T) {
	repoRoot, memDir := newFixtureRepo(t)
	seedCommit(t, repoRoot)
	commitsBefore := runGitT(t, repoRoot, "rev-list", "--count", "HEAD")
	led := &fakeLedger{}
	idx := &fakeIndexer{}
	s := newTestServer(memDir, &fakeSearcher{}, idx, led)
	seam := secretSeam(memDir, &fakeContentClassifier{secret: true, category: "kimlik"}, nil, &fakeLaneEscalator{})
	seam.Matcher = raceMatcher{mkdir: filepath.Join(memDir, "Gizli"), done: new(bool)}
	s.SetSecretLaneSeam(seam)

	out, err := s.HandleWriteForTask(context.Background(), "trace-r", "task-r", MemoryWriteArgs{Content: "TC kimlik numaram 10000000146", File: "inbox/r.md"})
	if !caseInsensitiveVolume(t) {
		if err != nil || out.File != "gizli/notes/inbox/r.md" {
			t.Fatalf("case-sensitive volume: out = %+v, err = %v; want the note byte-exact under gizli/", out, err)
		}
		return
	}
	if err == nil || err.Error() != MsgMemoryWriteSecretLaneRefused {
		t.Fatalf("out = %+v, err = %v; want the raced write refused with %q", out, err, MsgMemoryWriteSecretLaneRefused)
	}
	if files := regularFiles(t, memDir); len(files) != 0 {
		t.Errorf("files under the memory dir = %v, want none (the write is undone)", files)
	}
	if got := runGitT(t, repoRoot, "rev-list", "--count", "HEAD"); got != commitsBefore {
		t.Errorf("commit count = %s, want %s (nothing committed)", got, commitsBefore)
	}
	if len(idx.calls) != 0 {
		t.Errorf("reindex calls = %v, want none", idx.calls)
	}
	if got := eventKinds(led); !reflect.DeepEqual(got, []string{EventMemoryWriteSecretLaneRefused}) {
		t.Fatalf("ledger kinds = %v, want one %s", got, EventMemoryWriteSecretLaneRefused)
	}
	if p := led.events[0].payload; p["reason"] != "secret_dir_case_mismatch" || p["on_disk_file"] != "Gizli/notes/inbox/r.md" {
		t.Errorf("refusal payload = %v, want reason secret_dir_case_mismatch, on_disk_file Gizli/notes/inbox/r.md", p)
	}
}

// TestOnDiskRelPathReportsStoredNames pins the helper: existing components
// come back under their stored names (case, and Unicode normalisation where
// the volume folds it), missing ones as given, and a component that cannot
// be traversed is an error (the caller refuses).
func TestOnDiskRelPathReportsStoredNames(t *testing.T) {
	memDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(memDir, "Gizli", "notes"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	check := func(rel, want string) {
		t.Helper()
		got, err := onDiskRelPath(memDir, rel)
		if err != nil || got != want {
			t.Errorf("onDiskRelPath(%q) = %q, %v; want %q", rel, got, err, want)
		}
	}
	check("Gizli/notes/x.md", "Gizli/notes/x.md")
	check("inbox/z.md", "inbox/z.md")
	if caseInsensitiveVolume(t) {
		check("gizli/notes/new/y.md", "Gizli/notes/new/y.md")
		check("GIZLI/NOTES/y.md", "Gizli/notes/y.md")
	} else {
		check("gizli/notes/new/y.md", "gizli/notes/new/y.md")
	}

	// Unicode normalisation: "saglik" with g-breve, stored decomposed (NFD).
	nfd, nfc := "sağlık", "sağlık"
	if err := os.Mkdir(filepath.Join(memDir, nfd), 0o700); err != nil {
		t.Fatalf("mkdir NFD: %v", err)
	}
	if _, err := os.Stat(filepath.Join(memDir, nfc)); err == nil {
		check(nfc+"/a.md", nfd+"/a.md")
	} else {
		check(nfc+"/a.md", nfc+"/a.md")
	}

	if err := os.WriteFile(filepath.Join(memDir, "blk"), nil, 0o600); err != nil {
		t.Fatalf("write blk: %v", err)
	}
	if got, err := onDiskRelPath(memDir, "blk/x.md"); err == nil {
		t.Errorf("onDiskRelPath(blk/x.md) = %q, nil; want an error (blk is a file)", got)
	}
}
