package policy

// deny_globs_apfs_fold_test.go (FIX-FINAL attacker2-apfs-fold-glob-bypass):
// every committed fs_write_deny_globs and secret_lane_globs entry must
// still cover its path when the path is spelled the way macOS APFS folds
// names (long s for "s", sharp s for "ss", the ff/fi/fl/ffi/ffl/st
// ligatures, the Kelvin sign) - through the very matcher mcp/fs, mcp/shell
// and the osascript tools enforce with (mcp/fs.MatchesAnyGlobCI).

import (
	"os"
	"strings"
	"testing"

	mcpfs "kahya/mcp/fs"
	"kahya/mcp/fs/pathfold"
)

// apfsFoldSpellings maps an ASCII run to the non-ASCII spellings APFS
// opens as that run.
var apfsFoldSpellings = []struct{ ascii, folded string }{
	{"ffi", "\ufb03"}, {"ffl", "\ufb04"},
	{"ss", "\u00df"}, {"ss", "\u1e9e"},
	{"ff", "\ufb00"}, {"fi", "\ufb01"}, {"fl", "\ufb02"},
	{"st", "\ufb05"}, {"st", "\ufb06"},
	{"s", "\u017f"}, {"k", "\u212a"},
}

// apfsFoldVariants returns every spelling of p with exactly one ASCII run
// replaced by a non-ASCII run APFS folds onto it.
func apfsFoldVariants(p string) []string {
	var out []string
	lower := strings.ToLower(p)
	for _, sp := range apfsFoldSpellings {
		for i := 0; i+len(sp.ascii) <= len(p); i++ {
			if lower[i:i+len(sp.ascii)] == sp.ascii {
				out = append(out, p[:i]+sp.folded+p[i+len(sp.ascii):])
			}
		}
	}
	return out
}

// sampleForGlob builds a concrete path the (absolute, ~-expanded) glob g
// matches byte-exact: "**" -> one dir level, "*" / "?" -> letters.
func sampleForGlob(g string) string {
	parts := strings.Split(g, "/")
	for i, part := range parts {
		switch {
		case part == "**":
			parts[i] = "dir"
		default:
			part = strings.ReplaceAll(part, "*", "xyz")
			parts[i] = strings.ReplaceAll(part, "?", "q")
		}
	}
	return strings.Join(parts, "/")
}

func TestCommittedPolicyGlobsHoldUnderAPFSFolding(t *testing.T) {
	if _, err := os.UserHomeDir(); err != nil {
		t.Fatalf("resolve home dir: %v", err)
	}
	for _, name := range committedPolicyFiles {
		pol, err := Load(repoRoot(t) + "/" + name)
		if err != nil {
			t.Fatalf("Load(%s): %v", name, err)
		}
		for _, list := range []struct {
			field string
			globs []string
		}{{"fs_write_deny_globs", pol.FSWriteDenyGlobs}, {"secret_lane_globs", pol.SecretLaneGlobs}} {
			variants := 0
			for _, g := range list.globs {
				if !strings.HasPrefix(g, "/") && !strings.HasPrefix(g, "**/") {
					t.Fatalf("%s %s: unexpected glob shape %q", name, list.field, g)
				}
				sample := sampleForGlob(g)
				if strings.HasPrefix(sample, "dir/") {
					sample = "/Users/u/code/" + sample // a relative "**/x" glob
				}
				if ok, err := MatchGlob(g, sample); err != nil || !ok {
					t.Fatalf("%s %s: sample %q does not match %q byte-exact (%v)", name, list.field, sample, g, err)
				}
				for _, v := range apfsFoldVariants(sample) {
					if pathfold.Fold(v) != pathfold.Fold(sample) {
						t.Fatalf("variant %q does not fold onto %q", v, sample)
					}
					variants++
					hit, err := mcpfs.MatchesAnyGlobCI(v, list.globs)
					if err != nil || !hit {
						t.Errorf("%s %s: %q (APFS opens it as %q) is not covered (hit=%v err=%v)", name, list.field, v, sample, hit, err)
					}
				}
			}
			if variants == 0 {
				t.Fatalf("%s %s: no folded variant generated", name, list.field)
			}
		}
	}
}
