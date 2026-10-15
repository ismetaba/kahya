// lane.go implements W5-02's secret-lane ORDERING INVARIANT for nightly
// consolidation (HANDOFF §4 ⚑, quoted verbatim in the task spec):
//
//	Hicbir bayt, gizli-serit siniflandirmasi yerel/deterministik olarak
//	tamamlanmadan bulut modele gitmez. policy.yaml globlari YALNIZ dosya
//	yollari icin.
//
// A memory file's lane is decided by its path against policy.yaml's
// secret_lane_globs (kahyad/internal/policy.Policy.SecretLaneGlobs), the
// exact same PolicyGlobMatcher mechanism kahyad/internal/briefing already
// uses for its own file collector, AND - since FIX-FINAL
// cloud-hafiza-chunks-not-content-classified - by its content: a file at
// an ordinary path whose bytes the deterministic local pre-pass
// (memlane.ContentIsSecret: IBAN, TCKN, the finans/saglik/kimlik lexicon)
// classifies secret goes to the local lane too. PartitionByLane runs
// BEFORE either the cloud or local session is ever constructed - a file
// that matches a secret-lane glob (or classifies secret by content) is
// placed in the "local" map and NEVER copied into the "cloud" map,
// so it is structurally impossible for the cloud-lane envelope builder
// (worker.go) to ever see its bytes, regardless of what either session's
// own call does afterward.
package consolidation

import (
	"path/filepath"
	"sort"

	"kahya/kahyad/internal/memlane"
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
// are matched with matcher (PolicyGlobMatcher{} in production; nil = the
// file is treated as secret). A glob that cannot be evaluated counts as a
// MATCH (fail-closed, FIX-U-3): policy.Load has already validated every
// glob's syntax at boot (kahyad/internal/policy/loader.go's
// validateGlobSyntax), so a runtime error is a doublestar edge case - and
// over-blocking a file into the local lane is always safe, while treating
// it as "no match" would hand a possibly secret file (a gizli/** note
// memory_write redirected, a finans*.md note) to the cloud lane. The
// invariant this function exists to protect is "never cloud", not "never
// local". This is the same rule memlane.Classifier.PathIsSecret applies
// (memlane.MatchesSecretGlob: byte-exact OR case-/normalisation-folded -
// FIX-U-3 review: APFS stores "Gizli/" and "gizli/" as one directory).
// A file whose content memlane.ContentIsSecret classifies secret also goes
// to localFiles, whatever its path (FIX-FINAL cloud-hafiza-chunks-not-
// content-classified: the same deterministic rule the cloud <hafiza>
// block and the memory_search tool apply to chunk text).
func PartitionByLane(files map[string]string, memoryDir string, secretGlobs []string, matcher GlobMatcher) (cloudFiles, localFiles map[string]string) {
	cloudFiles = make(map[string]string, len(files))
	localFiles = make(map[string]string, len(files))

	for relPath, content := range files {
		absPath := filepath.Join(memoryDir, filepath.FromSlash(relPath))
		if matchesAnySecretGlob(absPath, secretGlobs, matcher) || memlane.ContentIsSecret(content) {
			localFiles[relPath] = content
		} else {
			cloudFiles[relPath] = content
		}
	}
	return cloudFiles, localFiles
}

// matchesAnySecretGlob reports whether absPath matches any entry in
// globs. A per-glob match error, or a nil matcher while globs are
// configured, is treated as a match - see PartitionByLane's own doc
// comment for why over-blocking (not over-trusting) is the safe direction.
// Each glob goes through memlane.MatchesSecretGlob - the same rule
// memlane.PathIsSecret applies: the byte-exact match, OR the match of the
// folded glob against the folded path. The folded half covers the name a
// case-/normalisation-insensitive volume STORES ("Gizli/notes/x.md" for a
// note written as gizli/notes/x.md into an existing Gizli/); the byte-
// exact half is kept because folding can change a glob's meaning (a
// negated class, a '?' for one rune of an NFD name), so nothing a byte-
// exact match covers ever lands in the cloud lane.
func matchesAnySecretGlob(absPath string, globs []string, matcher GlobMatcher) bool {
	for _, g := range globs {
		if memlane.MatchesSecretGlob(matcher, g, absPath) {
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
