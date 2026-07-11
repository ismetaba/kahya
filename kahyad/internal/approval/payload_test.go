package approval

import (
	"strings"
	"testing"

	"kahya/kahyad/internal/canon"

	"golang.org/x/text/unicode/norm"
)

// TestBuildFileEdit_SameSerializerBothTimes proves the WYSIWYE invariant
// this whole package exists for: building the SAME payload twice from
// identical inputs (approval time, then again at execution time) yields
// an identical Hash.
func TestBuildFileEdit_SameSerializerBothTimes(t *testing.T) {
	old := []byte("satır bir\nsatır iki\n")
	new := []byte("satır bir\nsatır iki değişti\n")

	p1 := BuildFileEdit("~/notlar.md", old, new)
	p2 := BuildFileEdit("~/notlar.md", old, new)
	if p1.Hash != p2.Hash {
		t.Fatalf("identical inputs must hash identically: %s != %s", p1.Hash, p2.Hash)
	}
	if !strings.Contains(p1.Render(), "satır iki değişti") {
		t.Fatalf("rendered diff missing added line: %s", p1.Render())
	}
}

// TestBuildFileEdit_MutatedByteChangesHash is the payload-level half of
// this task's mutated-byte regression test: a single trailing-space byte
// added to the path changes the Hash.
func TestBuildFileEdit_MutatedByteChangesHash(t *testing.T) {
	content := []byte("hello")
	p1 := BuildFileEdit("~/x.txt", nil, content)
	p2 := BuildFileEdit("~/x.txt ", nil, content) // trailing space
	if p1.Hash == p2.Hash {
		t.Fatalf("a trailing-space path mutation must change the hash")
	}
}

// TestBuildFileEdit_HomoglyphSwapChangesHash: a Cyrillic-for-Latin swap in
// the path also changes the hash — canonicalization never treats a
// homoglyph as equivalent to its look-alike (it is FLAGGED, never
// silently rewritten to match).
func TestBuildFileEdit_HomoglyphSwapChangesHash(t *testing.T) {
	content := []byte("hello")
	p1 := BuildFileEdit("~/paypal.txt", nil, content)
	p2 := BuildFileEdit("~/pаypal.txt", nil, content) // Cyrillic а (U+0430)
	if p1.Hash == p2.Hash {
		t.Fatalf("a homoglyph swap must change the hash")
	}
	found := false
	for _, f := range p2.Flags {
		if f.Kind == canon.FlagMixedScript {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a mixed-script flag on the homoglyph path, got %v", p2.Flags)
	}
}

// TestBuildFileEdit_NFDPathHashEqualsNFC: an NFD-encoded path must hash
// identically to its NFC form (kahyad/internal/canon's own guarantee,
// exercised here at the payload-builder level too). Uses a string with
// GENUINELY decomposable Turkish letters (ç ö ğ ü ş), not just the
// dotless ı in this task's literal nfd_path fixture, which has no
// canonical decomposition at all (see kahyad/internal/canon/
// normalize_test.go's own note on that).
func TestBuildFileEdit_NFDPathHashEqualsNFC(t *testing.T) {
	composed := "~/Kahya/memory/proje-notları-çöğüş.md"
	decomposed := norm.NFD.String(composed)
	if composed == decomposed {
		t.Fatalf("test setup error: expected composed/decomposed forms to actually differ")
	}

	p1 := BuildFileEdit(composed, nil, []byte("x"))
	p2 := BuildFileEdit(decomposed, nil, []byte("x"))
	if p1.Hash != p2.Hash {
		t.Fatalf("NFD-encoded path must hash equal to its NFC form: %s != %s", p1.Hash, p2.Hash)
	}
}

// TestBuildShellScript_ImageDigestAndWorkdirPresent checks shell_script's
// serialization actually incorporates all three named fields (image
// digest, workdir, script bytes) - changing any one changes the hash.
func TestBuildShellScript_ImageDigestAndWorkdirPresent(t *testing.T) {
	base := BuildShellScript("sha256:aaa", "~/work", []byte("echo hi"))
	diffDigest := BuildShellScript("sha256:bbb", "~/work", []byte("echo hi"))
	diffWorkdir := BuildShellScript("sha256:aaa", "~/other", []byte("echo hi"))
	diffScript := BuildShellScript("sha256:aaa", "~/work", []byte("echo bye"))

	if base.Hash == diffDigest.Hash || base.Hash == diffWorkdir.Hash || base.Hash == diffScript.Hash {
		t.Fatalf("changing image digest, workdir, or script must each change the hash")
	}
}

// TestBuildEgress_Fields checks egress's serialization incorporates
// method, canonical host, and byte count.
func TestBuildEgress_Fields(t *testing.T) {
	base := BuildEgress("POST", "api.example.com", 128)
	diffMethod := BuildEgress("GET", "api.example.com", 128)
	diffHost := BuildEgress("POST", "other.example.com", 128)
	diffCount := BuildEgress("POST", "api.example.com", 256)

	if base.Hash == diffMethod.Hash || base.Hash == diffHost.Hash || base.Hash == diffCount.Hash {
		t.Fatalf("changing method, host, or byte count must each change the hash")
	}
}

// TestBuildMessage_Fields checks message's serialization incorporates
// recipient and body.
func TestBuildMessage_Fields(t *testing.T) {
	base := BuildMessage("alice@example.com", "merhaba")
	diffRecipient := BuildMessage("bob@example.com", "merhaba")
	diffBody := BuildMessage("alice@example.com", "selam")

	if base.Hash == diffRecipient.Hash || base.Hash == diffBody.Hash {
		t.Fatalf("changing recipient or body must each change the hash")
	}
}

// decodeLengthPrefixedFields reverses encodeFields' own
// [len uint64 big-endian][bytes]... layout — used ONLY by this test to
// prove a payload's CanonicalBytes carries EXACTLY the fields the spec
// says and no more, rather than merely re-deriving the same bytes
// encodeFields would produce (which would pass even if BuildShortcut
// silently started including an extra field, as long as this test's own
// expectation string also silently grew to match).
func decodeLengthPrefixedFields(t *testing.T, b []byte) [][]byte {
	t.Helper()
	var fields [][]byte
	for len(b) > 0 {
		if len(b) < 8 {
			t.Fatalf("truncated length prefix in %d remaining bytes", len(b))
		}
		n := uint64(0)
		for i := 0; i < 8; i++ {
			n = n<<8 | uint64(b[i])
		}
		b = b[8:]
		if uint64(len(b)) < n {
			t.Fatalf("field length %d exceeds %d remaining bytes", n, len(b))
		}
		fields = append(fields, b[:n])
		b = b[n:]
	}
	return fields
}

// TestBuildShortcut_PayloadContainsOnlyNameAndInputPath is W3-09's own
// acceptance criterion, verbatim: "shortcuts_run approval payload
// contains ONLY the shortcut name + canonical input path and nothing
// else (test asserts the serialized payload bytes)" — decoded field by
// field (not merely re-derived via encodeFields, which would trivially
// match itself), asserting EXACTLY three fields: kind, name, input path.
func TestBuildShortcut_PayloadContainsOnlyNameAndInputPath(t *testing.T) {
	p := BuildShortcut("Yedekle", "/Users/kahya/Desktop/girdi.txt")

	if p.Kind != KindShortcut {
		t.Fatalf("Kind = %q, want %q", p.Kind, KindShortcut)
	}

	fields := decodeLengthPrefixedFields(t, p.CanonicalBytes)
	if len(fields) != 3 {
		t.Fatalf("decoded %d fields, want exactly 3 (kind, name, input_path); fields=%q", len(fields), fields)
	}
	if got := string(fields[0]); got != string(KindShortcut) {
		t.Errorf("field[0] (kind) = %q, want %q", got, KindShortcut)
	}
	if got := string(fields[1]); got != "Yedekle" {
		t.Errorf("field[1] (name) = %q, want %q", got, "Yedekle")
	}
	if got := string(fields[2]); got != "/Users/kahya/Desktop/girdi.txt" {
		t.Errorf("field[2] (input_path) = %q, want %q", got, "/Users/kahya/Desktop/girdi.txt")
	}
}

// TestBuildShortcut_Fields checks the shortcut serialization incorporates
// name and input path (the hash-changes-on-either-field-changing pattern
// every other Build* test in this file already uses).
func TestBuildShortcut_Fields(t *testing.T) {
	base := BuildShortcut("Yedekle", "/tmp/a.txt")
	diffName := BuildShortcut("Baska", "/tmp/a.txt")
	diffPath := BuildShortcut("Yedekle", "/tmp/b.txt")
	noPath := BuildShortcut("Yedekle", "")

	if base.Hash == diffName.Hash || base.Hash == diffPath.Hash || base.Hash == noPath.Hash {
		t.Fatalf("changing name or input path must each change the hash")
	}
}

// TestBuildShortcut_FlagsSurfaceBidiInName proves a bidi/zero-width rune
// hidden inside the shortcut NAME is surfaced via Flags (HANDOFF §5
// safety #5: "never dropped invisibly") — mirrors every other Build*
// function's own Flags convention (BuildMessage/BuildEgress/
// BuildFileEdit), which BuildShortcut must not silently omit.
func TestBuildShortcut_FlagsSurfaceBidiInName(t *testing.T) {
	p := BuildShortcut("Yedekle‮evil", "/tmp/a.txt")
	if len(p.Flags) == 0 {
		t.Fatal("Flags is empty, want the RLO override rune surfaced")
	}
	found := false
	for _, f := range p.Flags {
		if f.Kind == canon.FlagBidi {
			found = true
		}
	}
	if !found {
		t.Errorf("Flags = %+v, want a FlagBidi entry", p.Flags)
	}
}
