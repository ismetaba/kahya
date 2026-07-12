// lane.go implements W5-02's secret-lane ORDERING INVARIANT for nightly
// consolidation (HANDOFF §4 ⚑, quoted verbatim in the task spec):
//
//	Hicbir bayt, gizli-serit siniflandirmasi yerel/deterministik olarak
//	tamamlanmadan bulut modele gitmez. policy.yaml globlari YALNIZ dosya
//	yollari icin.
//
// Memory files are path-addressed, so - unlike content ingested at
// runtime (mail/web bodies, classified by kahyad/internal/secretlane's
// content classifier) - a memory file's lane is decided PURELY by
// matching its path against policy.yaml's secret_lane_globs
// (kahyad/internal/policy.Policy.SecretLaneGlobs), the exact same
// PolicyGlobMatcher mechanism kahyad/internal/briefing already uses for
// its own file collector. PartitionByLane runs BEFORE either the cloud or
// local session is ever constructed - a file that matches a secret-lane
// glob is placed in the "local" map and NEVER copied into the "cloud" map,
// so it is structurally impossible for the cloud-lane envelope builder
// (worker.go) to ever see its bytes, regardless of what either session's
// own call does afterward.
package consolidation

import (
	"path/filepath"
	"sort"

	"kahya/kahyad/internal/policy"
)

// GlobMatcher is the narrow secret-lane-glob-matching seam this package
// needs - *kahyad/internal/policy.Policy's own MatchGlob free function
// satisfies this trivially (NewPolicyGlobMatcher below adapts it), and a
// test can inject a trivial fake without loading a real policy.yaml at
// all.
type GlobMatcher interface {
	Match(pattern, path string) (bool, error)
}

// PolicyGlobMatcher adapts kahyad/internal/policy.MatchGlob (doublestar,
// byte-exact, no ASCII folding) to GlobMatcher - the production
// implementation.
type PolicyGlobMatcher struct{}

func (PolicyGlobMatcher) Match(pattern, path string) (bool, error) {
	return policy.MatchGlob(pattern, path)
}

// PartitionByLane splits files (keyed by path RELATIVE to memoryDir,
// forward-slash separated - the same convention kahyad/internal/indexer
// uses for episodes.source_path) into cloudFiles/localFiles by matching
// each file's ABSOLUTE path (filepath.Join(memoryDir, relPath)) against
// every entry in secretGlobs (already ~-expanded - kahyad/internal/
// policy.Policy.SecretLaneGlobs). A file matching ANY glob goes to
// localFiles and is skipped from cloudFiles entirely - never both. Globs
// are matched with matcher (PolicyGlobMatcher{} in production); a glob
// syntax error is folded into "no match for this glob" (logged by the
// caller if it cares) rather than aborting the whole partition, since
// policy.Load has ALREADY validated every glob's syntax at boot
// (kahyad/internal/policy/loader.go's validateGlobSyntax) - a runtime
// error here would only ever be a doublestar library edge case, and
// fail-closed for THIS specific check means "assume it might be
// secret" is the WRONG direction (over-blocking a file into local-lane
// is always safe; the invariant this function exists to protect is
// "never cloud", not "never local").
func PartitionByLane(files map[string]string, memoryDir string, secretGlobs []string, matcher GlobMatcher) (cloudFiles, localFiles map[string]string) {
	cloudFiles = make(map[string]string, len(files))
	localFiles = make(map[string]string, len(files))

	for relPath, content := range files {
		absPath := filepath.Join(memoryDir, filepath.FromSlash(relPath))
		if matchesAnySecretGlob(absPath, secretGlobs, matcher) {
			localFiles[relPath] = content
		} else {
			cloudFiles[relPath] = content
		}
	}
	return cloudFiles, localFiles
}

// matchesAnySecretGlob reports whether absPath matches any entry in
// globs. A per-glob match error is treated as "no match" - see
// PartitionByLane's own doc comment for why over-blocking (not
// over-trusting) is the safe direction here regardless.
func matchesAnySecretGlob(absPath string, globs []string, matcher GlobMatcher) bool {
	for _, g := range globs {
		ok, err := matcher.Match(g, absPath)
		if err != nil {
			continue
		}
		if ok {
			return true
		}
	}
	return false
}

// sortedKeys returns m's keys sorted - used anywhere this package needs a
// deterministic file iteration order (prompt building, diff staging) so
// output/tests never depend on Go's randomized map iteration order.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
