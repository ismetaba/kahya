package pathfold

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// TestFoldMapsAPFSFoldedRunesOntoTheirASCIIName: every non-ASCII spelling
// the attacker probe showed APFS opening as an ASCII name folds onto that
// name; strings.ToLower (the old key) leaves all but the Kelvin sign alone.
func TestFoldMapsAPFSFoldedRunesOntoTheirASCIIName(t *testing.T) {
	for in, want := range map[string]string{
		"\u017f":        "s",   // LATIN SMALL LETTER LONG S
		"\u00df":        "ss",  // LATIN SMALL LETTER SHARP S
		"\u1e9e":        "ss",  // LATIN CAPITAL LETTER SHARP S
		"\ufb00":        "ff",  // LATIN SMALL LIGATURE FF
		"\ufb01":        "fi",  // LATIN SMALL LIGATURE FI
		"\ufb02":        "fl",  // LATIN SMALL LIGATURE FL
		"\ufb03":        "ffi", // LATIN SMALL LIGATURE FFI
		"\ufb04":        "ffl", // LATIN SMALL LIGATURE FFL
		"\ufb05":        "st",  // LATIN SMALL LIGATURE LONG S T
		"\ufb06":        "st",  // LATIN SMALL LIGATURE ST
		"\u212a":        "k",   // KELVIN SIGN
		"~/.z\u017fhrc": "~/.zshrc",
		"Application \u017fupport/Kahya/brain.db": "application support/kahya/brain.db",
		"Library/LaunchAgent\u017f/x.plist":       "library/launchagents/x.plist",
		"Documents/\u017faglik/tahlil.md":         "documents/saglik/tahlil.md",
		"Documents/sag\u0306lik":                  "documents/sa\u011flik", // NFD in, NFC out
	} {
		if got := Fold(in); got != want {
			t.Errorf("Fold(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestFoldIsCoarserThanLower: for every rune, Fold(r) == Fold(ToLower(r))
// and Fold is idempotent - so any two names the old lower-cased key equated
// still share one key, and folding a folded key changes nothing.
func TestFoldIsCoarserThanLower(t *testing.T) {
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if !utf8.ValidRune(r) {
			continue
		}
		s := string(r)
		f := Fold(s)
		if f != Fold(strings.ToLower(s)) {
			t.Fatalf("Fold(U+%04X) = %q but Fold(ToLower) = %q", r, f, Fold(strings.ToLower(s)))
		}
		if Fold(f) != f {
			t.Fatalf("Fold not idempotent at U+%04X: %q -> %q", r, f, Fold(f))
		}
	}
}

// caseBearingRanges are the Unicode blocks with case pairs (Latin,
// Greek, Cyrillic, Armenian, Georgian, Cherokee, Glagolitic, Coptic,
// Latin Extended Additional, Greek Extended, letterlike symbols, the
// alphabetic presentation forms, fullwidth Latin, Deseret, Osage, Vithkuqi,
// Old Hungarian, Warang Citi, Medefaidrin, Adlam).
var caseBearingRanges = [][2]rune{
	{0x0021, 0x058F}, {0x10A0, 0x10FF}, {0x13A0, 0x13FF}, {0x1C80, 0x1CBF},
	{0x1D00, 0x1FFF}, {0x2100, 0x218F}, {0x24B6, 0x24E9}, {0x2C00, 0x2D2F},
	{0xA640, 0xA69F}, {0xA720, 0xA7FF}, {0xAB30, 0xABBF}, {0xFB00, 0xFB4F},
	{0xFF00, 0xFF5F}, {0x10400, 0x104FF}, {0x10570, 0x105BF}, {0x10C80, 0x10CFF},
	{0x118A0, 0x118FF}, {0x16E40, 0x16E9F}, {0x1E900, 0x1E95F},
}

// TestFoldCoversAPFSNameEquivalence asks the volume itself: one name per
// rune (and per multi-letter fold target) is created O_EXCL in a fresh
// directory on the test machine's temp volume; a create that fails with
// EEXIST hit a name the volume treats as the same file, and both names
// must fold to one key. Skipped off macOS and on a case-sensitive volume,
// where no such pair exists.
func TestFoldCoversAPFSNameEquivalence(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("APFS name equivalence is a macOS property")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "probe-a"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "PROBE-A")); err != nil {
		t.Skip("temp volume is case-sensitive")
	}
	names := []string{"qss", "qff", "qfi", "qfl", "qffi", "qffl", "qst"}
	for _, rg := range caseBearingRanges {
		for r := rg[0]; r <= rg[1]; r++ {
			if r == '/' || r == '.' || !(unicode.IsLetter(r) || unicode.IsSymbol(r) || unicode.IsNumber(r) || unicode.IsPunct(r)) {
				continue
			}
			names = append(names, "q"+string(r))
		}
	}
	collisions := 0
	for _, name := range names {
		p := filepath.Join(dir, name)
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, werr := f.WriteString(name)
			if cerr := f.Close(); werr != nil || cerr != nil {
				t.Fatalf("write %q: %v %v", name, werr, cerr)
			}
			continue
		}
		if !errors.Is(err, fs.ErrExist) {
			continue // a name the volume refuses outright opens nothing
		}
		collisions++
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read back %q: %v", name, err)
		}
		if other := string(b); Fold(other) != Fold(name) {
			t.Errorf("the volume opens %q as %q, but Fold gives %q and %q", name, other, Fold(name), Fold(other))
		}
	}
	if collisions < 100 {
		t.Fatalf("only %d name collisions on a case-insensitive volume; the probe exercised nothing", collisions)
	}
}
