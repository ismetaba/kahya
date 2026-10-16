package invariants

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// harnessWriter is one harness source file that writes a config.yaml for a
// real kahyad binary (recognized by its task_timeout_min: line).
type harnessWriter struct {
	rel string // repo-relative path
	src string // file content
}

// harnessConfigWriters walks every place a harness that boots a real kahyad
// can live (tests/, scripts/, eval/, kahyad/internal/mlxe2e) and returns the
// .go/.sh/.py files that write a kahyad config.yaml. tests/invariants itself
// is skipped (its guards spell the marker too). Fails t when none is found:
// the heuristic would then be stale.
func harnessConfigWriters(t *testing.T) []harnessWriter {
	t.Helper()
	root, err := RepoRoot("")
	if err != nil {
		t.Fatalf("RepoRoot: %v", err)
	}
	self := filepath.Join(root, "tests", "invariants")
	var writers []harnessWriter
	for _, top := range []string{"tests", "scripts", "eval", filepath.Join("kahyad", "internal", "mlxe2e")} {
		base := filepath.Join(root, top)
		if _, err := os.Stat(base); err != nil {
			continue
		}
		err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if strings.HasPrefix(d.Name(), ".") || p == self {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, ".sh") && !strings.HasSuffix(p, ".py") {
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if !strings.Contains(string(b), "task_timeout_min:") {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			writers = append(writers, harnessWriter{rel: rel, src: string(b)})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", top, err)
		}
	}
	if len(writers) == 0 {
		t.Fatal("found no harness config writers at all - the task_timeout_min: heuristic is stale")
	}
	return writers
}

// TestHarnessDaemonsNeverExecRealHammerspoon guards test hermeticity: every
// harness that writes a config.yaml for a real kahyad binary (recognized by
// its task_timeout_min: line) must also override ui_hs_cli. Left at its
// default (/opt/homebrew/bin/hs), the daemon execs the developer's real
// Hammerspoon CLI for every pending approval and notification, which pops a
// "Hammerspoon is not running" dialog on screen - or, with Hammerspoon
// running, shows real approval cards for test traffic.
func TestHarnessDaemonsNeverExecRealHammerspoon(t *testing.T) {
	for _, w := range harnessConfigWriters(t) {
		if !strings.Contains(w.src, "ui_hs_cli: /usr/bin/true") {
			t.Errorf("%s writes a kahyad config.yaml but does not set ui_hs_cli: /usr/bin/true", w.rel)
		}
	}
}
