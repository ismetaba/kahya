package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FIX-FINAL intent-journal-torn-write-recovered-as-user-edit: kahyad's
// journalled note writes (memory_write, the ingest note writer, memory
// confirm, memory_forget-by-heading) land atomically and durably - the
// bytes go to a temp file under <git-dir>/kahya-pending/, are fsynced, and
// only then replace the note (rename, or link for a new ingest note) and
// the directory is fsynced. After a kill or a power loss at any point the
// note holds its prior bytes or exactly the journalled ones, never a torn
// mix the recovery would leave to the nightly author=user pre-commit.

// killPoint simulates kahyad dying (SIGKILL / power loss) at a point
// inside a note write: the panic unwinds the handler, so none of its undo
// code runs - only what was already on disk remains.
type killPoint struct{}

// runUntilKilled runs op and swallows the simulated death.
func runUntilKilled(t *testing.T, op func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(killPoint); !ok {
				panic(r)
			}
			return
		}
		t.Fatal("the write was never killed: the injected kill point was not reached")
	}()
	op()
}

// killMidWrite makes the next note write die after half of its bytes.
func killMidWrite(t *testing.T) {
	t.Helper()
	prev := noteBytesWriter
	noteBytesWriter = func(f *os.File, b []byte) (int, error) {
		n, err := f.Write(b[:len(b)/2])
		if err != nil {
			return n, err
		}
		panic(killPoint{})
	}
	t.Cleanup(func() { noteBytesWriter = prev })
}

// killBeforePlace makes the next note write die after its bytes were
// written and synced, before they replace the note.
func killBeforePlace(t *testing.T) {
	t.Helper()
	prev := beforeNotePlace
	beforeNotePlace = func(tmp, dst string) error { panic(killPoint{}) }
	t.Cleanup(func() { beforeNotePlace = prev })
}

func pendingEntries(t *testing.T, repoRoot string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(repoRoot, ".git", pendingDirName))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// assertRecoveredClean runs the boot recovery and checks that it leaves
// nothing behind: no journal entry, no temp file, a clean working tree
// (nothing the author=user pre-commit could claim) and no new commit.
func assertRecoveredClean(t *testing.T, s *Server, repoRoot string) {
	t.Helper()
	head := runGitT(t, repoRoot, "rev-parse", "HEAD")
	if err := s.RecoverPendingWrites(context.Background(), "trace-boot"); err != nil {
		t.Fatalf("RecoverPendingWrites: %v", err)
	}
	if names := pendingEntries(t, repoRoot); len(names) != 0 {
		t.Fatalf("journal after recovery = %v, want empty", names)
	}
	if st := runGitT(t, repoRoot, "status", "--porcelain", "--untracked-files=all"); st != "" {
		t.Fatalf("working tree after recovery = %q, want clean (a torn or half-placed note would be pre-committed as the user)", st)
	}
	if now := runGitT(t, repoRoot, "rev-parse", "HEAD"); now != head {
		t.Fatalf("recovery committed something (%s -> %s); nothing kahyad finished writing was on disk", head, now)
	}
}

// TestNoteWriteKilledMidWriteNeverTearsTheNote: a kill halfway through
// writing a memory_write append, a memory confirm and a new ingest note
// leaves the committed note byte-identical (or, for the new note, absent)
// - before the fix the append was a truncating in-place write, and the
// half-written note was dropped by the recovery as "changed" and left for
// the author=user pre-commit.
func TestNoteWriteKilledMidWriteNeverTearsTheNote(t *testing.T) {
	repoRoot, memDir := newFixtureRepo(t)
	s := newTestServer(memDir, nil, &fakeIndexer{episodeID: 1}, &fakeLedger{})
	ctx := context.Background()
	if _, err := s.HandleWrite(ctx, "tr", MemoryWriteArgs{File: "inbox/a.md", Content: strings.Repeat("ilk satır\n", 40)}); err != nil {
		t.Fatal(err)
	}
	committed := runGitT(t, repoRoot, "show", "HEAD:memory/inbox/a.md")

	killMidWrite(t)
	runUntilKilled(t, func() {
		_, _ = s.HandleWrite(ctx, "tr", MemoryWriteArgs{File: "inbox/a.md", Content: strings.Repeat("ikinci satır\n", 40)})
	})
	if got, _ := os.ReadFile(filepath.Join(memDir, "inbox", "a.md")); strings.TrimSpace(string(got)) != committed {
		t.Fatalf("note after a kill mid-append = %q..., want its committed bytes", firstBytes(got))
	}
	noteBytesWriter = func(f *os.File, b []byte) (int, error) { return f.Write(b) }
	assertRecoveredClean(t, s, repoRoot)

	// memory confirm of a tiered note, killed mid-rewrite.
	if _, err := s.HandleWriteTiered(ctx, "tr", tieredArgs("ingest/r.md", false)); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(memDir, "ingest", "r.md"))
	killMidWrite(t)
	runUntilKilled(t, func() { _, _ = s.HandleConfirm(ctx, "tr", MemoryConfirmArgs{File: "ingest/r.md"}) })
	if got, _ := os.ReadFile(filepath.Join(memDir, "ingest", "r.md")); string(got) != string(before) {
		t.Fatalf("note after a kill mid-confirm = %q, want %q", got, before)
	}
	noteBytesWriter = func(f *os.File, b []byte) (int, error) { return f.Write(b) }
	assertRecoveredClean(t, s, repoRoot)

	// A new ingest note, killed mid-write: no note at all.
	killMidWrite(t)
	runUntilKilled(t, func() { _, _ = s.HandleWriteTiered(ctx, "tr", tieredArgs("ingest/new.md", false)) })
	if _, err := os.Lstat(filepath.Join(memDir, "ingest", "new.md")); !os.IsNotExist(err) {
		t.Fatalf("a half-written new ingest note is on disk (%v)", err)
	}
	noteBytesWriter = func(f *os.File, b []byte) (int, error) { return f.Write(b) }
	assertRecoveredClean(t, s, repoRoot)
}

// TestNoteWriteKilledBeforePlaceKeepsPriorBytes: a kill after the bytes
// were written and synced but before they replace the note (memory_write
// append, memory_forget-by-heading) leaves the note's prior bytes; the
// recovery drops the intent and removes the temp file.
func TestNoteWriteKilledBeforePlaceKeepsPriorBytes(t *testing.T) {
	repoRoot, memDir := newFixtureRepo(t)
	s := newTestServer(memDir, nil, &fakeIndexer{episodeID: 1}, &fakeLedger{})
	ctx := context.Background()
	if _, err := s.HandleWrite(ctx, "tr", MemoryWriteArgs{File: "notes/b.md", Content: "# Başlık\nmetin\n\n## Alt\nsilinecek\n"}); err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(memDir, "notes", "b.md")
	before, _ := os.ReadFile(abs)

	killBeforePlace(t)
	runUntilKilled(t, func() { _, _ = s.HandleWrite(ctx, "tr", MemoryWriteArgs{File: "notes/b.md", Content: "ek"}) })
	if got, _ := os.ReadFile(abs); string(got) != string(before) {
		t.Fatalf("note after a kill before the rename = %q, want %q", got, before)
	}
	if names := pendingEntries(t, repoRoot); len(names) != 2 {
		t.Fatalf("journal = %v, want the intent and its synced temp file", names)
	}
	beforeNotePlace = nil
	assertRecoveredClean(t, s, repoRoot)

	killBeforePlace(t)
	runUntilKilled(t, func() { _, _ = s.HandleForget(ctx, "tr", MemoryForgetArgs{File: "notes/b.md", Heading: "Alt"}) })
	if got, _ := os.ReadFile(abs); string(got) != string(before) {
		t.Fatalf("note after a kill before the forget's rename = %q, want %q", got, before)
	}
	beforeNotePlace = nil
	assertRecoveredClean(t, s, repoRoot)
}

// TestJournalledNoteWritesPlaceExactlyTheJournalledBytes: every journalled
// writer places a temp file whose bytes are the intent's (sha256), from the
// journal directory, and leaves no temp file and no intent behind.
func TestJournalledNoteWritesPlaceExactlyTheJournalledBytes(t *testing.T) {
	repoRoot, memDir := newFixtureRepo(t)
	s := newTestServer(memDir, nil, &fakeIndexer{episodeID: 1}, &fakeLedger{})
	ctx := context.Background()
	var placed []string
	prev := beforeNotePlace
	t.Cleanup(func() { beforeNotePlace = prev })
	pendingDir, err := filepath.EvalSymlinks(filepath.Join(repoRoot, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	pendingDir = filepath.Join(pendingDir, pendingDirName)
	beforeNotePlace = func(tmp, dst string) error {
		if filepath.Dir(tmp) != pendingDir || !strings.HasSuffix(tmp, noteTempSuffix) {
			t.Errorf("temp file %s is not a journal temp under %s", tmp, pendingDir)
		}
		b, err := os.ReadFile(tmp)
		if err != nil {
			t.Fatal(err)
		}
		// the intent recorded for this write carries the same sha256
		found := false
		for _, name := range pendingEntries(t, repoRoot) {
			raw, _ := os.ReadFile(filepath.Join(pendingDir, name))
			if strings.Contains(string(raw), `"sha256":"`+sha256Hex(b)+`"`) {
				found = true
			}
		}
		if !found {
			t.Errorf("no journalled intent carries the placed bytes' sha256 (%s)", dst)
		}
		placed = append(placed, filepath.Base(dst)+"="+sha256Hex(b))
		return nil
	}
	steps := []struct {
		name string
		op   func() error
		file string
	}{
		{"memory_write new", func() error {
			_, err := s.HandleWrite(ctx, "tr", MemoryWriteArgs{File: "inbox/a.md", Content: "# A\nbir\n\n## B\niki\n"})
			return err
		}, "inbox/a.md"},
		{"memory_write append", func() error {
			_, err := s.HandleWrite(ctx, "tr", MemoryWriteArgs{File: "inbox/a.md", Content: "üç"})
			return err
		}, "inbox/a.md"},
		{"memory_forget heading", func() error {
			_, err := s.HandleForget(ctx, "tr", MemoryForgetArgs{File: "inbox/a.md", Heading: "B"})
			return err
		}, "inbox/a.md"},
		{"ingest note", func() error {
			_, err := s.HandleWriteTiered(ctx, "tr", tieredArgs("ingest/r.md", false))
			return err
		}, "ingest/r.md"},
		{"memory confirm", func() error {
			_, err := s.HandleConfirm(ctx, "tr", MemoryConfirmArgs{File: "ingest/r.md"})
			return err
		}, "ingest/r.md"},
	}
	for _, step := range steps {
		n := len(placed)
		if err := step.op(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if len(placed) != n+1 {
			t.Fatalf("%s placed %d files, want 1", step.name, len(placed)-n)
		}
		got, err := os.ReadFile(filepath.Join(memDir, filepath.FromSlash(step.file)))
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Base(step.file) + "=" + sha256Hex(got); placed[n] != want {
			t.Fatalf("%s: placed %s, on disk %s", step.name, placed[n], want)
		}
		if names := pendingEntries(t, repoRoot); len(names) != 0 {
			t.Fatalf("%s left %v in the journal", step.name, names)
		}
	}
	if st := runGitT(t, repoRoot, "status", "--porcelain", "--untracked-files=all"); st != "" {
		t.Fatalf("tree not clean: %q", st)
	}
}

// TestHandleWriteTieredNeverReplacesANoteCreatedMeanwhile: the new ingest
// note is linked into place (never renamed over): a file that appeared
// under its name after the free-name check is left as it is, the write
// fails, and neither a temp file nor an intent stays behind.
func TestHandleWriteTieredNeverReplacesANoteCreatedMeanwhile(t *testing.T) {
	repoRoot, memDir := newFixtureRepo(t)
	s := newTestServer(memDir, nil, &fakeIndexer{episodeID: 1}, &fakeLedger{})
	prev := beforeNotePlace
	t.Cleanup(func() { beforeNotePlace = prev })
	beforeNotePlace = func(tmp, dst string) error {
		return os.WriteFile(dst, []byte("kullanıcının notu\n"), 0o600)
	}
	if _, err := s.HandleWriteTiered(context.Background(), "tr", tieredArgs("ingest/x.md", false)); err == nil {
		t.Fatal("HandleWriteTiered replaced a note created under its name")
	}
	if got, _ := os.ReadFile(filepath.Join(memDir, "ingest", "x.md")); string(got) != "kullanıcının notu\n" {
		t.Fatalf("the concurrently created note = %q, want it untouched", got)
	}
	if names := pendingEntries(t, repoRoot); len(names) != 0 {
		t.Fatalf("journal = %v, want empty", names)
	}
}

// TestNoteReplaceKeepsTheStoredName: replacing an existing note through a
// case variant of its name keeps the spelling the volume stores (the
// episodes.source_path key) - a rename onto the variant would re-spell it.
func TestNoteReplaceKeepsTheStoredName(t *testing.T) {
	repoRoot, memDir := newFixtureRepo(t)
	if !caseInsensitiveVolume(t) {
		t.Skip("case-sensitive volume")
	}
	s := newTestServer(memDir, nil, &fakeIndexer{episodeID: 1}, &fakeLedger{})
	h, err := s.recordWriteIntent(mustEval(t, repoRoot), filepath.Join(mustEval(t, memDir), "Not.md"), "memory_write", "tr", []byte("ikinci\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memDir, "Not.md"), []byte("bir\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := writeNoteDurable(h.pendingDir, filepath.Join(memDir, "not.md"), []byte("ikinci\n"), false); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(memDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.EqualFold(e.Name(), "not.md") && e.Name() != "Not.md" {
			t.Fatalf("stored name = %q, want Not.md kept", e.Name())
		}
	}
	if got, _ := os.ReadFile(filepath.Join(memDir, "Not.md")); string(got) != "ikinci\n" {
		t.Fatalf("note = %q", got)
	}
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func firstBytes(b []byte) string {
	if len(b) > 60 {
		b = b[:60]
	}
	return string(b)
}
