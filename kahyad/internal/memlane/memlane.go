// Package memlane answers ONE question, fail-closed: "may this fact's text
// leave the machine?" - for a Telegram ritual card today (kahyad/internal/
// ritual), and for the later G13 cloud uses (graph facts in a cloud
// <hafiza> block, the fact tools a cloud-lane worker calls, the extractor's
// lane choice). It is the classification body kahyad/internal/ritual's
// sampler used to carry, moved here in its post-FIX-P-17 form (G00 C-20) so
// every caller applies the same rule:
//
//   - PATH: the SAME path-glob mechanism kahyad/internal/consolidation's
//     PartitionByLane uses for memory files (HANDOFF §4 ordering invariant:
//     policy.yaml's secret_lane_globs are FILE-PATH globs only). Every
//     evidence row citing the fact is walked back to its episode's
//     source_path; a path matching a secret-lane glob excludes the fact,
//     whatever else cites it. A fact with NO evidence citing a resolvable
//     path has no classification record and is excluded on that basis
//     alone ("no classification => treat as secret").
//   - CONTENT (FIX-P-17): the fact's own text (FactText) is classified
//     locally - always by the deterministic pre-pass
//     (secretlane.ClassifyDeterministic: IBAN/TCKN/card/CVV and the
//     finans/saglik/kimlik lexicon), plus an optional ContentClassifier on
//     top (fail-closed: any error excludes). A fact is path-independent (an
//     IBAN can sit in a note under an ordinary path), so the path check
//     alone is not enough.
//
// Fail-closed throughout: a store read error is returned (the caller then
// refuses), a glob that cannot be evaluated - or no matcher at all -
// makes the path secret, and glob and path are case-/normalisation-folded
// before matching (FoldForSecretGlob), so a variant spelling of a secret
// path on a case-insensitive volume is secret too. (The ritual copy
// treated a glob match error as "no match"; policy.Load validates every
// glob at boot, so that never happened in practice, but this package
// never fails open.)
//
// This package does not import kahyad/internal/policy: the matcher is
// injected (ritual.PolicyGlobMatcher in production). That keeps hermetic
// tests simple; it is not a boundary requirement - kahyad/internal/policy's
// import_graph_test guards the other direction (policy never imports the
// memory packages).
package memlane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"kahya/kahyad/internal/secretlane"
	"kahya/kahyad/internal/store/sqlcgen"
	"kahya/kahyad/internal/textnorm"
)

// Store is the brain.db read surface Classify needs - *sqlcgen.Queries
// satisfies it directly.
type Store interface {
	ListEpisodePathsForFact(ctx context.Context, factID int64) ([]sql.NullString, error)
}

var _ Store = (*sqlcgen.Queries)(nil)

// GlobMatcher matches one secret-lane glob against an absolute path
// (policy.MatchGlob's shape).
type GlobMatcher interface {
	Match(pattern, path string) (bool, error)
}

// ContentClassifier is the optional content seam on top of the
// deterministic pre-pass (ritual.FactClassifier's shape). A wired one must
// be local-only; an error excludes the fact.
type ContentClassifier interface {
	ClassifySecretLane(ctx context.Context, text string) (secret bool, err error)
}

// Exclusion reasons (Verdict.Reason). ritual ledgers them as its
// ritual.fact_excluded_secret_lane payload "reason".
const (
	ReasonPathGlob             = "path_glob"
	ReasonNoClassification     = "no_classification_record"
	ReasonContentDeterministic = "content_deterministic"
	ReasonContentClassifier    = "content_classifier"
	ReasonClassifyFailed       = "classify_failed"
)

// Verdict is Classify's answer: Eligible, or the Reason the fact must stay
// on the machine.
type Verdict struct {
	Eligible bool
	Reason   string
}

// Classifier holds the policy inputs. The zero value refuses everything
// that needs a store; a Classifier is a plain value, safe to copy and to use
// concurrently.
type Classifier struct {
	Store Store
	// MemoryDir is the memory corpus root episodes.source_path is relative
	// to (cfg.MemoryDir).
	MemoryDir string
	// SecretGlobs are policy.yaml's secret_lane_globs (absolute globs).
	SecretGlobs []string
	Matcher     GlobMatcher
	// Content is optional (nil = deterministic pre-pass only, never "skip
	// classification").
	Content ContentClassifier
}

// FactText is the text of a fact that leaves the machine with it (the
// ritual question body; the triple a later cloud rendering carries):
// "subject predicate object", trimmed.
func FactText(f sqlcgen.Fact) string {
	return strings.TrimSpace(f.Subject + " " + f.Predicate + " " + f.Object)
}

// PathIsSecret reports whether relPath (forward-slash, relative to
// MemoryDir, as episodes.source_path stores it) matches a secret-lane glob.
// A glob that errors, or a missing matcher while globs are configured,
// counts as a match (fail-closed).
//
// Both the glob and the path are folded first (FoldForSecretGlob: NFC,
// Turkish I-family, lowercase): macOS APFS is case- and normalisation-
// insensitive, so "Gizli/x.md" and "gizli/x.md" are the same directory
// and a byte-exact match would hand the former to the cloud lane.
// Over-matching only ever keeps a path on the machine.
func (c Classifier) PathIsSecret(relPath string) bool {
	abs := FoldForSecretGlob(filepath.Join(c.MemoryDir, filepath.FromSlash(relPath)))
	for _, g := range c.SecretGlobs {
		if c.Matcher == nil {
			return true
		}
		ok, err := c.Matcher.Match(FoldForSecretGlob(g), abs)
		if err != nil || ok {
			return true
		}
	}
	return false
}

// FoldForSecretGlob is the folding every memory-side secret-lane path rule
// applies to BOTH a secret_lane_globs pattern and the path it is matched
// against (memlane.PathIsSecret, consolidation.PartitionByLane):
// textnorm.Fold - NFC normalisation, the Turkish dotted/dotless I family
// collapsed onto "i", then Unicode lowercase. It is a superset of what an
// APFS volume treats as the same name (case- and normalisation-
// insensitive), so a case or normalisation variant of a secret path is
// secret too (mcp/fs.MatchesAnyGlobCI's rationale, HANDOFF S5 safety #6).
// Glob metacharacters are unaffected.
func FoldForSecretGlob(s string) string { return textnorm.Fold(s) }

// Classify decides whether fact f may leave the machine: path half first
// (any cited secret path excludes; no resolvable citation excludes), then
// the content half on FactText(f). An error means the store could not be
// read; the caller must then treat the fact as excluded.
func (c Classifier) Classify(ctx context.Context, f sqlcgen.Fact) (Verdict, error) {
	if c.Store == nil {
		return Verdict{}, errors.New("memlane: no store wired")
	}
	paths, err := c.Store.ListEpisodePathsForFact(ctx, f.ID)
	if err != nil {
		return Verdict{}, fmt.Errorf("memlane: list episode paths for fact %d: %w", f.ID, err)
	}
	classified := false
	for _, p := range paths {
		if !p.Valid || p.String == "" {
			continue
		}
		classified = true
		if c.PathIsSecret(p.String) {
			return Verdict{Reason: ReasonPathGlob}, nil
		}
	}
	if !classified {
		return Verdict{Reason: ReasonNoClassification}, nil
	}

	text := FactText(f)
	if secretlane.ClassifyDeterministic(text).SecretLane {
		return Verdict{Reason: ReasonContentDeterministic}, nil
	}
	if c.Content != nil {
		secret, err := c.Content.ClassifySecretLane(ctx, text)
		if err != nil {
			return Verdict{Reason: ReasonClassifyFailed}, nil
		}
		if secret {
			return Verdict{Reason: ReasonContentClassifier}, nil
		}
	}
	return Verdict{Eligible: true}, nil
}

// CloudEligible is Classify's yes/no form: false on any exclusion and on a
// store error (which is also returned).
func (c Classifier) CloudEligible(ctx context.Context, f sqlcgen.Fact) (bool, error) {
	v, err := c.Classify(ctx, f)
	if err != nil {
		return false, err
	}
	return v.Eligible, nil
}
