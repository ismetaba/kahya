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
