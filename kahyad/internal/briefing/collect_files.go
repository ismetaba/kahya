// collect_files.go implements the W5-01 watched-file collector: an
// mtime-diff over configured doublestar globs since the last run (task
// spec step 2). Deterministic Go code (os.Stat + doublestar.FilepathGlob),
// never a shelled-out `find`/`ls` - no Docker-shell rule concerns here at
// all (kahyad reads the filesystem directly, in-process). Every returned
// ChangedFile.Path is ALSO the ordering-invariant gate's file-path-glob
// input (gate.go's GlobMatcher, checked against policy.yaml's
// secret_lane_globs) - this is the ONE collector whose CollectedItem
// carries a non-empty Path, precisely because it is the ONE collector
// whose signal genuinely originates from the filesystem HANDOFF's
// "policy.yaml globs are file-path-only" rule is about.
package briefing

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/bmatcuk/doublestar/v4"
)

// filePathMaxLen is this collector's own length cap (charclass-
// constrained via capText) - a filename is a short label, never a
// paragraph.
const filePathMaxLen = 300

// defaultMaxFileItems caps how many changed files ONE briefing reports
// (newest mtime first) - cfg.briefing_max_file_items (FIX-P-23). Every
// file item costs one local classification call in the gate loop and a
// prompt line in the cloud summarizer; an uncapped first run over a large
// glob spent minutes in the gate and bloated the prompt.
const defaultMaxFileItems = 50

// ChangedFile is one collected, already length/charclass-capped watched-
// file signal: its canonical path and the RFC3339 modification time that
// made it "changed" relative to the collector's own since cutoff.
type ChangedFile struct {
	Path       string
	ModifiedAt string

	// modTime is the full-precision mtime Collect sorts by (newest first);
	// ModifiedAt is its second-resolution display form.
	modTime time.Time
}

// FileGlobCollector mtime-diffs Globs (doublestar patterns, `~`-expanded
// by the caller - config.Config.BriefingFileGlobs is already expanded by
// config.Load's own expandHomeEach) since a since cutoff.
type FileGlobCollector struct {
	Globs []string
	// MaxItems caps the returned slice (<= 0 = defaultMaxFileItems).
	MaxItems int
}

func (c FileGlobCollector) maxItems() int {
	if c.MaxItems > 0 {
		return c.MaxItems
	}
	return defaultMaxFileItems
}

// Collect returns every regular file matched by any configured glob whose
// mtime is strictly after since, NEWEST FIRST, capped at maxItems();
// truncated reports how many matching files the cap dropped (FIX-P-23).
// An empty Globs list is a documented no-op (nil, 0, nil) - the files
// section simply has zero items until one is configured. A glob-expansion
// or stat failure for one entry/file is NON-FATAL (skipped, matching every
// other collector's own tolerance of a single-item failure never aborting
// the whole briefing).
func (c FileGlobCollector) Collect(since time.Time) (files []ChangedFile, truncated int, err error) {
	if len(c.Globs) == 0 {
		return nil, 0, nil
	}
	var out []ChangedFile
	seen := make(map[string]bool)
	for _, g := range c.Globs {
		matches, err := doublestar.FilepathGlob(g)
		if err != nil {
			continue
		}
		for _, m := range matches {
			if seen[m] {
				continue
			}
			fi, err := os.Stat(m)
			if err != nil || fi.IsDir() {
				continue
			}
			if fi.ModTime().After(since) {
				seen[m] = true
				out = append(out, ChangedFile{Path: m, ModifiedAt: fi.ModTime().UTC().Format(time.RFC3339), modTime: fi.ModTime()})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].modTime.After(out[j].modTime) })
	if n := c.maxItems(); len(out) > n {
		truncated = len(out) - n
		out = out[:n]
	}
	return out, truncated, nil
}

// fileItems adapts a []ChangedFile into []CollectedItem (gate.go),
// Section="file" - Path is set to the file's own canonical path (the
// ordering-invariant gate's file-path-glob check, gate.go's GlobMatcher,
// consults exactly this field), Text is a capped, charclass-constrained
// display line (basename + modification time - never the file's own
// CONTENT, which this collector never reads at all).
func fileItems(files []ChangedFile) []CollectedItem {
	items := make([]CollectedItem, len(files))
	for i, f := range files {
		items[i] = CollectedItem{
			Section: "file",
			Text:    capText(fmt.Sprintf("%s (%s)", filepath.Base(f.Path), f.ModifiedAt), filePathMaxLen),
			Path:    f.Path,
		}
	}
	return items
}
