package canon

import "testing"

// TestParseConfusables_Loaded is a basic sanity check that the vendored
// data file actually parsed into a non-trivial table at init — a silent
// parse failure (e.g. an embed path typo) would otherwise leave every
// confusable-with-ASCII check permanently, silently blind.
func TestParseConfusables_Loaded(t *testing.T) {
	if len(confusableSkeleton) < 1000 {
		t.Fatalf("expected at least 1000 parsed confusable entries, got %d", len(confusableSkeleton))
	}
}

// TestConfusableSkeletonFor_CyrillicA is this task's own homoglyph_host
// fixture's underlying confusable: Cyrillic а (U+0430) must resolve to
// Latin "a" per the real vendored Unicode data.
func TestConfusableSkeletonFor_CyrillicA(t *testing.T) {
	skeleton, ok := confusableSkeletonFor(0x0430)
	if !ok {
		t.Fatalf("expected U+0430 to have a confusables.txt entry")
	}
	if skeleton != "a" {
		t.Fatalf("skeleton = %q, want %q", skeleton, "a")
	}
	if !isASCIIString(skeleton) {
		t.Fatalf("skeleton %q should be pure ASCII", skeleton)
	}
}

// TestConfusableSkeletonFor_TurkishDotlessI documents the real data's own
// mapping for dotless ı (U+0131 -> "i", pure ASCII) — the exact case
// normalize.go's Latin-script exclusion exists to guard against; see
// TestNormalize_TurkishLettersNotConfusable in normalize_test.go for the
// end-to-end proof that this entry alone does NOT cause a false positive.
func TestConfusableSkeletonFor_TurkishDotlessI(t *testing.T) {
	skeleton, ok := confusableSkeletonFor(0x0131)
	if !ok {
		t.Fatalf("expected U+0131 (dotless i) to have a confusables.txt entry in the real vendored data")
	}
	if skeleton != "i" {
		t.Fatalf("skeleton = %q, want %q", skeleton, "i")
	}
}
