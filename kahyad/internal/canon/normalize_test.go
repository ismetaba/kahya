package canon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/text/unicode/norm"
)

// readFixture loads a byte-exact testdata fixture — this task's own
// instruction: "byte-exact, never re-typed by hand later".
func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(b)
}

// TestNormalize_BidiFilename is this task's bidi_filename fixture:
// "invoice<U+202E>fdp.txt" must render with a visible escape and survive
// round-trip hashing (canonicalizing twice is idempotent).
func TestNormalize_BidiFilename(t *testing.T) {
	raw := readFixture(t, "bidi_filename.txt")
	res := Normalize(raw)

	if !res.HasControlFlags() {
		t.Fatalf("expected a control flag for the embedded U+202E, got none (flags=%v)", res.Flags)
	}
	found := false
	for _, f := range res.Flags {
		if f.Kind == FlagBidi && f.Rune == 0x202E {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected FlagBidi for U+202E, got %v", res.Flags)
	}

	const wantEscape = "<U+202E>"
	if !strings.Contains(res.Display, wantEscape) {
		t.Fatalf("Display %q does not contain visible escape %q", res.Display, wantEscape)
	}
	if containsRune(res.Display, 0x202E) {
		t.Fatalf("Display %q must not contain the raw bidi rune itself", res.Display)
	}
	if containsRune(res.Canonical, 0x202E) {
		t.Fatalf("Canonical %q must not contain the stripped bidi rune", res.Canonical)
	}

	// Round-trip: canonicalizing the already-canonical form again must be
	// a no-op (idempotent), and must hash identically both times — the
	// same bytes shown at approval time must still hash the same at
	// execution time.
	again := Normalize(res.Canonical)
	if again.Canonical != res.Canonical {
		t.Fatalf("canonicalization is not idempotent: %q -> %q", res.Canonical, again.Canonical)
	}
	h1 := CanonicalizeBytes([]byte(raw))
	h2 := CanonicalizeBytes([]byte(raw))
	if string(h1) != string(h2) {
		t.Fatalf("CanonicalizeBytes is not deterministic across calls")
	}
}

// TestNormalize_HomoglyphHost is this task's homoglyph_host fixture:
// "pаypal.com" (Cyrillic а, U+0430) must be flagged mixed-script, and the
// confusable rune must NEVER be silently rewritten — Canonical/Display
// stay byte-identical to the raw input.
func TestNormalize_HomoglyphHost(t *testing.T) {
	raw := readFixture(t, "homoglyph_host.txt")
	res := Normalize(raw)

	found := false
	for _, f := range res.Flags {
		if f.Kind == FlagMixedScript && f.Token == "pаypal" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected FlagMixedScript for token \"pаypal\", got %v", res.Flags)
	}

	if res.Canonical != raw {
		t.Fatalf("homoglyph must never be silently rewritten: Canonical %q != raw %q", res.Canonical, raw)
	}
	if res.Display != raw {
		t.Fatalf("homoglyph must never be silently rewritten: Display %q != raw %q", res.Display, raw)
	}
}

// TestNormalize_ZeroWidthURL is this task's zero_width_url fixture:
// "https://api.tele<U+200B>gram.org" must be flagged AND its canonical
// host must differ from the raw string (the ZWSP is stripped, revealing
// the real host it was hiding inside).
func TestNormalize_ZeroWidthURL(t *testing.T) {
	raw := readFixture(t, "zero_width_url.txt")
	res := Normalize(raw)

	found := false
	for _, f := range res.Flags {
		if f.Kind == FlagZeroWidth && f.Rune == 0x200B {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected FlagZeroWidth for U+200B, got %v", res.Flags)
	}

	const wantCanonical = "https://api.telegram.org"
	if res.Canonical != wantCanonical {
		t.Fatalf("Canonical = %q, want %q (ZWSP stripped)", res.Canonical, wantCanonical)
	}
	if res.Canonical == raw {
		t.Fatalf("canonical host must differ from the raw (zero-width-smuggled) host")
	}
	if !strings.Contains(res.Display, "<U+200B>") {
		t.Fatalf("Display %q does not contain the visible zero-width escape", res.Display)
	}
}

// TestNormalize_TurkishClean is this task's turkish_clean fixture: pure
// Turkish content must pass with ZERO flags — the Turkish alphabet is
// expected content, not a homoglyph signal.
func TestNormalize_TurkishClean(t *testing.T) {
	raw := readFixture(t, "turkish_clean.txt")
	res := Normalize(raw)

	if len(res.Flags) != 0 {
		t.Fatalf("pure-Turkish content must not be flagged, got %v", res.Flags)
	}
	if res.Canonical != raw {
		t.Fatalf("Canonical must equal raw for clean Turkish content: %q != %q", res.Canonical, raw)
	}
}

// TestNormalize_TurkishLettersNotConfusable is a targeted regression for
// the two Turkish letters the REAL vendored confusables.txt data does map
// to a pure-ASCII skeleton (ı -> i, ç -> c + a combining mark) — without
// this package's Latin-script exclusion (see normalize.go's package doc
// comment), naive per-rune confusable-with-ASCII lookups would wrongly
// flag nearly every Turkish word containing a dotless ı.
func TestNormalize_TurkishLettersNotConfusable(t *testing.T) {
	for _, word := range []string{"ısırgan", "çınar", "ışık", "açık"} {
		res := Normalize(word)
		if len(res.Flags) != 0 {
			t.Fatalf("Turkish word %q must not be flagged, got %v", word, res.Flags)
		}
	}
}

// TestNormalize_NFDPathHashEquality is this task's nfd_path fixture:
// "~/Kahya/memory/proje-notları.md" NFD-encoded must hash-equal its NFC
// form. The stored fixture's only non-ASCII code point (dotless ı,
// U+0131) has no canonical decomposition, so its NFD and NFC forms are
// already byte-identical — the equality below still holds (trivially).
// An auxiliary check further down additionally exercises a string with
// GENUINELY decomposable Turkish letters (ç ö ğ ü ş all decompose under
// NFD) to prove the NFC step itself does real work, not just pass unequal
// bytes through.
func TestNormalize_NFDPathHashEquality(t *testing.T) {
	nfcForm := readFixture(t, "nfd_path.txt")
	nfdForm := norm.NFD.String(nfcForm)

	hNFC := CanonicalizeBytes([]byte(nfcForm))
	hNFD := CanonicalizeBytes([]byte(nfdForm))
	if string(hNFC) != string(hNFD) {
		t.Fatalf("NFD-encoded path must canonicalize identically to its NFC form: %q vs %q", hNFC, hNFD)
	}

	// Auxiliary (not one of the five required fixtures): a string
	// containing letters that DO have a canonical decomposition, so this
	// assertion would fail if NFC-normalization were silently skipped.
	composed := "proje-notları-çöğüş.md"
	decomposed := norm.NFD.String(composed)
	if composed == decomposed {
		t.Fatalf("test setup error: expected the auxiliary string to actually decompose under NFD")
	}
	if string(CanonicalizeBytes([]byte(composed))) != string(CanonicalizeBytes([]byte(decomposed))) {
		t.Fatalf("auxiliary NFD/NFC pair must canonicalize identically")
	}
}

// TestNormalize_MutatedByteStillDiffers proves canonicalization does not
// over-normalize to the point of hiding a REAL mutation: a trailing space,
// or a homoglyph swap, must still produce a different Canonical form (and
// therefore a different hash) than the original — this is the property
// kahyad/internal/policy's mutated-byte regression test relies on.
func TestNormalize_MutatedByteStillDiffers(t *testing.T) {
	cases := []struct {
		name   string
		before string
		after  string
	}{
		{"trailing_space", "~/x.txt", "~/x.txt "},
		{"homoglyph_swap", "paypal.com", "pаypal.com"}, // а -> Cyrillic U+0430
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := CanonicalizeBytes([]byte(tc.before))
			after := CanonicalizeBytes([]byte(tc.after))
			if string(before) == string(after) {
				t.Fatalf("%s: canonical bytes must differ: %q vs %q produced the same canonical form", tc.name, tc.before, tc.after)
			}
		})
	}
}

func containsRune(s string, want rune) bool {
	for _, r := range s {
		if r == want {
			return true
		}
	}
	return false
}
