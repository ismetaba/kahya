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

// TestPartitionByLaneMatcherErrorTreatedAsSecret (FIX-U-3): a glob that
// cannot be evaluated - or no matcher at all - puts the file in the LOCAL
// lane, never the cloud one (fail-closed; memlane.PathIsSecret applies
// the same rule). It replaces TestPartitionByLaneMatcherErrorTreatedAsNoMatch,
// which pinned the old fail-open "no match" folding.
func TestPartitionByLaneMatcherErrorTreatedAsSecret(t *testing.T) {
	files := map[string]string{"a.md": "x", "gizli/notes/inbox/b.md": "y"}
	m := fakeMatcher{matchFn: func(pattern, path string) (bool, error) {
		return false, errors.New("forced glob error")
	}}
	for _, tc := range []struct {
		name    string
		matcher GlobMatcher
	}{{"erroring", m}, {"nil", nil}} {
		cloud, local := PartitionByLane(files, "/home/user/Kahya/memory", []string{"/home/user/Kahya/memory/gizli/**"}, tc.matcher)
		if len(cloud) != 0 || len(local) != 2 {
			t.Fatalf("%s matcher: cloud=%v local=%v, want every file local (fail-closed)", tc.name, cloud, local)
		}
	}
	// No globs configured: nothing is evaluated, nothing is secret.
	cloud, local := PartitionByLane(files, "/home/user/Kahya/memory", nil, nil)
	if len(cloud) != 2 || len(local) != 0 {
		t.Fatalf("no globs: cloud=%v local=%v, want everything cloud", cloud, local)
	}
}

// TestPartitionByLaneGizliNoteGoesLocal (FIX-U-3): a note memory_write
// redirected under gizli/ is local-lane under the real policy.yaml glob.
func TestPartitionByLaneGizliNoteGoesLocal(t *testing.T) {
	pol, err := policy.Load("../../../policy.yaml")
	if err != nil {
		t.Fatalf("policy.Load() error = %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home dir: %v", err)
	}
	files := map[string]string{"gizli/notes/inbox/2026-10-13.md": "secret", "inbox/2026-10-13.md": "ordinary"}
	cloud, local := PartitionByLane(files, home+"/Kahya/memory", pol.SecretLaneGlobs, PolicyGlobMatcher{})
	if _, ok := local["gizli/notes/inbox/2026-10-13.md"]; !ok || len(local) != 1 {
		t.Fatalf("local = %v, want only the gizli/ note", local)
	}
	if _, ok := cloud["inbox/2026-10-13.md"]; !ok || len(cloud) != 1 {
		t.Fatalf("cloud = %v, want only the ordinary inbox note", cloud)
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

// TestPartitionByLaneContentSecretGoesLocal (FIX-FINAL
// cloud-hafiza-chunks-not-content-classified): a memory file at an
// ORDINARY path whose bytes the deterministic pre-pass classifies secret
// (an IBAN) goes to the local lane, never the cloud one.
func TestPartitionByLaneContentSecretGoesLocal(t *testing.T) {
	files := map[string]string{
		"notlar/kira.md":        "Ahmet'e kira için gönderilecek hesap: TR33 0006 1005 1978 6457 8413 26\n",
		"properties/kadikoy.md": "Kadıköy'de iki daire gezdik\n",
	}
	cloud, local := PartitionByLane(files, "/m", []string{"/m/gizli/**"}, PolicyGlobMatcher{})
	if _, ok := cloud["notlar/kira.md"]; ok {
		t.Errorf("content-secret file reached the cloud lane")
	}
	if _, ok := local["notlar/kira.md"]; !ok {
		t.Errorf("content-secret file missing from the local lane")
	}
	if _, ok := cloud["properties/kadikoy.md"]; !ok {
		t.Errorf("clean file missing from the cloud lane")
	}
}
