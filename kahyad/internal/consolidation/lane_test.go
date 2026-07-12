package consolidation

import (
	"errors"
	"os"
	"testing"

	"kahya/kahyad/internal/policy"
)

// fakeMatcher is a trivial GlobMatcher a test can wire without loading a
// real policy.yaml - PartitionByLane's OWN tests below use the real
// PolicyGlobMatcher (doublestar) since exercising the actual glob syntax
// matters for this specific invariant; fakeMatcher exists for the one
// test that wants a forced-error path.
type fakeMatcher struct {
	matchFn func(pattern, path string) (bool, error)
}

func (f fakeMatcher) Match(pattern, path string) (bool, error) {
	return f.matchFn(pattern, path)
}

func TestPartitionByLaneSecretGlobGoesLocalNeverCloud(t *testing.T) {
	files := map[string]string{
		"finans-notes.md": "iban: TR330006100519786457841326",
		"genel.md":        "ordinary non-secret note",
	}
	globs := []string{"/home/user/Kahya/memory/finans*.md"}

	cloud, local := PartitionByLane(files, "/home/user/Kahya/memory", globs, PolicyGlobMatcher{})

	if _, ok := cloud["finans-notes.md"]; ok {
		t.Fatalf("finans-notes.md leaked into cloudFiles: %+v", cloud)
	}
	if _, ok := local["finans-notes.md"]; !ok {
		t.Fatalf("finans-notes.md missing from localFiles: %+v", local)
	}
	if _, ok := cloud["genel.md"]; !ok {
		t.Fatalf("genel.md missing from cloudFiles: %+v", cloud)
	}
	if _, ok := local["genel.md"]; ok {
		t.Fatalf("genel.md leaked into localFiles: %+v", local)
	}
}

func TestPartitionByLaneNoGlobsEverythingCloud(t *testing.T) {
	files := map[string]string{"a.md": "x", "b.md": "y"}
	cloud, local := PartitionByLane(files, "/home/user/Kahya/memory", nil, PolicyGlobMatcher{})
	if len(cloud) != 2 || len(local) != 0 {
		t.Fatalf("cloud=%v local=%v, want everything in cloud", cloud, local)
	}
}

func TestPartitionByLaneMatcherErrorTreatedAsNoMatch(t *testing.T) {
	files := map[string]string{"a.md": "x"}
	m := fakeMatcher{matchFn: func(pattern, path string) (bool, error) {
		return false, errors.New("forced glob error")
	}}
	cloud, local := PartitionByLane(files, "/home/user/Kahya/memory", []string{"**"}, m)
	if _, ok := cloud["a.md"]; !ok {
		t.Fatalf("a.md should stay in cloudFiles when the matcher errors: cloud=%v local=%v", cloud, local)
	}
}

// TestPartitionByLaneRealPolicyGlobs proves this package's glob matching
// is byte-for-byte the SAME mechanism policy.yaml itself declares
// (kahyad/internal/policy.Load + MatchGlob), not a hand-rolled
// reimplementation - a fixture matching this repo's actual committed
// policy.yaml glob "~/Kahya/memory/finans*.md" is exercised end to end.
func TestPartitionByLaneRealPolicyGlobs(t *testing.T) {
	pol, err := policy.Load("../../../policy.yaml")
	if err != nil {
		t.Fatalf("policy.Load() error = %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home dir: %v", err)
	}
	memoryDir := home + "/Kahya/memory"
	files := map[string]string{
		"finans-2026.md":  "secret",
		"genel-notlar.md": "not secret",
	}
	cloud, local := PartitionByLane(files, memoryDir, pol.SecretLaneGlobs, PolicyGlobMatcher{})
	if _, ok := local["finans-2026.md"]; !ok {
		t.Errorf("finans-2026.md should be local-lane per the real policy.yaml glob; local=%v", local)
	}
	if _, ok := cloud["genel-notlar.md"]; !ok {
		t.Errorf("genel-notlar.md should be cloud-lane; cloud=%v", cloud)
	}
}
