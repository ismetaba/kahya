package briefing

import (
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
	files, err := c.Collect(cutoff)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(files) != 1 || files[0].Path != newf {
		t.Fatalf("files = %+v, want exactly [new.md]", files)
	}
}

func TestFileGlobCollectorEmptyGlobsIsNoop(t *testing.T) {
	files, err := (FileGlobCollector{}).Collect(time.Now())
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
