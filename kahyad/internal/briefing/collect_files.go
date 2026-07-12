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
	"time"

	"github.com/bmatcuk/doublestar/v4"
)

// filePathMaxLen is this collector's own length cap (charclass-
// constrained via capText) - a filename is a short label, never a
// paragraph.
const filePathMaxLen = 300

// ChangedFile is one collected, already length/charclass-capped watched-
// file signal: its canonical path and the RFC3339 modification time that
// made it "changed" relative to the collector's own since cutoff.
type ChangedFile struct {
	Path       string
	ModifiedAt string
}

// FileGlobCollector mtime-diffs Globs (doublestar patterns, `~`-expanded
// by the caller - config.Config.BriefingFileGlobs is already expanded by
// config.Load's own expandHomeEach) since a since cutoff.
type FileGlobCollector struct {
	Globs []string
}

// Collect returns every regular file matched by any configured glob whose
// mtime is strictly after since. An empty Globs list is a documented
// no-op (nil, nil) - the files section simply has zero items until one is
// configured. A glob-expansion or stat failure for one entry/file is
// NON-FATAL (skipped, matching every other collector's own tolerance of a
// single-item failure never aborting the whole briefing).
func (c FileGlobCollector) Collect(since time.Time) ([]ChangedFile, error) {
	if len(c.Globs) == 0 {
		return nil, nil
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
				out = append(out, ChangedFile{Path: m, ModifiedAt: fi.ModTime().UTC().Format(time.RFC3339)})
			}
		}
	}
	return out, nil
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
