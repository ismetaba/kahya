package briefing

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileGlobCollectorFindsChangedFilesSinceCutoff(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "old.md")
	newf := filepath.Join(dir, "new.md")
	if err := os.WriteFile(old, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now()
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(newf, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Backdate old.md well before the cutoff so this test is not sensitive
	// to filesystem mtime-resolution granularity.
	past := cutoff.Add(-time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}

	c := FileGlobCollector{Globs: []string{filepath.Join(dir, "*.md")}}
	files, _, err := c.Collect(cutoff)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(files) != 1 || files[0].Path != newf {
		t.Fatalf("files = %+v, want exactly [new.md]", files)
	}
}

func TestFileGlobCollectorEmptyGlobsIsNoop(t *testing.T) {
	files, _, err := (FileGlobCollector{}).Collect(time.Now())
	if err != nil || files != nil {
		t.Fatalf("Collect(no globs) = %v, %v, want nil, nil", files, err)
	}
}

func TestFileScanStateRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.txt")
	s := FileScanState{Path: path}

	loaded, err := s.Load()
	if err != nil {
		t.Fatalf("Load (missing file): %v", err)
	}
	if !loaded.IsZero() {
		t.Fatalf("Load (missing file) = %v, want zero time", loaded)
	}

	now := time.Now().UTC().Truncate(time.Second)
	if err := s.Save(now); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err = s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !loaded.Equal(now) {
		t.Fatalf("Load after Save = %v, want %v", loaded, now)
	}
}

// writeFileAt writes path and backdates its mtime to at.
func writeFileAt(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// TestFileGlobCollectorCapsToMaxItemsNewestFirst (FIX-P-23): Collect
// returns at most MaxItems files, newest mtime first, and reports how many
// matching files the cap dropped.
func TestFileGlobCollectorCapsToMaxItemsNewestFirst(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	for i := 1; i <= 7; i++ {
		writeFileAt(t, filepath.Join(dir, fmt.Sprintf("f%d.md", i)), now.Add(-time.Duration(i)*time.Hour))
	}
	c := FileGlobCollector{Globs: []string{filepath.Join(dir, "*.md")}, MaxItems: 3}
	files, truncated, err := c.Collect(now.Add(-24 * time.Hour))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(files) != 3 || truncated != 4 {
		t.Fatalf("Collect = %d files, truncated %d; want 3, 4", len(files), truncated)
	}
	for i, want := range []string{"f1.md", "f2.md", "f3.md"} {
		if got := filepath.Base(files[i].Path); got != want {
			t.Errorf("files[%d] = %s, want %s (newest first)", i, got, want)
		}
	}

	// Default cap (MaxItems unset) is defaultMaxFileItems, never unbounded.
	for i := 0; i < defaultMaxFileItems+5; i++ {
		writeFileAt(t, filepath.Join(dir, fmt.Sprintf("bulk%03d.txt", i)), now.Add(-time.Minute))
	}
	all, truncated, err := (FileGlobCollector{Globs: []string{filepath.Join(dir, "*.txt")}}).Collect(now.Add(-24 * time.Hour))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(all) != defaultMaxFileItems || truncated != 5 {
		t.Errorf("default cap: %d files, truncated %d; want %d, 5", len(all), truncated, defaultMaxFileItems)
	}
}
