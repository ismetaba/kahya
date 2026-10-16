package briefing

import "testing"

// TestPolicyGlobMatcherFoldsAPFSSpellingsAndFailsClosed (FIX-FINAL
// attacker2-apfs-fold-glob-bypass): the file gate's production matcher
// applies memlane's rule - a case or APFS-folded spelling of a secret-lane
// path (the collector reports the name the volume stores) is dropped, and a
// glob that cannot be evaluated drops the item instead of counting as no
// match.
func TestPolicyGlobMatcherFoldsAPFSSpellingsAndFailsClosed(t *testing.T) {
	m := PolicyGlobMatcher{Globs: []string{"/Users/u/Documents/saglik/**", "/Users/u/Kahya/memory/finans*.md"}}
	for path, want := range map[string]bool{
		"/Users/u/Documents/saglik/tahlil.md":      true,
		"/Users/u/Documents/Saglik/tahlil.md":      true,
		"/Users/u/Documents/\u017faglik/tahlil.md": true,
		"/Users/u/Document\u017f/saglik/tahlil.md": true,
		"/Users/u/Kahya/memory/\ufb01nans-2026.md": true,
		"/Users/u/Documents/notlar.md":             false,
		"/Users/u/Kahya/memory/inbox/x.md":         false,
	} {
		if got := m.MatchesSecretLane(path); got != want {
			t.Errorf("MatchesSecretLane(%q) = %v, want %v", path, got, want)
		}
	}
	if !(PolicyGlobMatcher{Globs: []string{"/Users/u/["}}).MatchesSecretLane("/Users/u/x.md") {
		t.Error("a glob error counted as no match; the item must be dropped (fail-closed)")
	}
}
