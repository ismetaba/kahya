package textnorm

import "testing"

// TestFoldCollapsesTurkishIFamily is the documented golden case from the
// task spec: all four members of the Turkish I-family fold to plain 'i',
// regardless of dotted/dotless or case.
//
// Runes are spelled with explicit \u escapes (rather than literal source
// bytes) throughout this file so no editor/tool in the write path can
// silently re-normalize a decomposed fixture into its precomposed form (or
// vice versa) before the test ever runs.
func TestFoldCollapsesTurkishIFamily(t *testing.T) {
	in := "Iıİi" // I, dotless-i (u0131), dotted-capital-I (u0130), i
	got := Fold(in)
	want := "iiii"
	if got != want {
		t.Errorf("Fold(%q) = %q, want %q", in, got, want)
	}
}

// TestFoldIstanbulBothDirections guards the exact behavior the W1-2
// acceptance gate depends on: a query typed with a plain ASCII 'i', or in
// all caps with the dotted capital İ, must fold to the same string as the
// dotted lowercase form actually written in the seed note.
func TestFoldIstanbulBothDirections(t *testing.T) {
	dottedCapital := "İstanbul" // "İstanbul" as actually written in the seed note
	cases := []string{dottedCapital, "istanbul", "ISTANBUL", "İSTANBUL"}
	want := Fold(cases[0])
	for _, c := range cases[1:] {
		if got := Fold(c); got != want {
			t.Errorf("Fold(%q) = %q, want %q (== Fold(%q))", c, got, want, cases[0])
		}
	}
	if want != "istanbul" {
		t.Errorf("Fold(%q) = %q, want %q", cases[0], want, "istanbul")
	}
}

// TestFoldKeepsOtherDiacritics guards against over-folding: Fold must only
// resolve the Turkish dotted/dotless-I ambiguity, never strip or
// ASCII-fold any other diacritic (tasks/README.md: no manual
// stemming/normalization beyond what the spec calls for).
func TestFoldKeepsOtherDiacritics(t *testing.T) {
	// "Kadıköy'de ÖĞÜŞÇ âêî" - ı (u0131), ö (u00f6), ğ (u011f), ü (u00fc),
	// ş (u015f), ç (u00e7), â (u00e2), ê (u00ea), î (u00ee).
	in := "Kadıköy'de ÖĞÜŞÇ âêî"
	got := Fold(in)
	want := "kadiköy'de öğüşç âêî"
	if got != want {
		t.Errorf("Fold(%q) = %q, want %q", in, got, want)
	}
}

// TestFoldAppliesNFC guards the NFC-normalization step: a decomposed
// base+combining-circumflex sequence must fold identically to its
// precomposed equivalent, or the trigram index and a query typed in the
// other normalization form would silently fail to match.
func TestFoldAppliesNFC(t *testing.T) {
	precomposed := "ê" // LATIN SMALL LETTER E WITH CIRCUMFLEX, single code point
	decomposed := "ê" // 'e' + COMBINING CIRCUMFLEX ACCENT, two code points
	if precomposed == decomposed {
		t.Fatal("test fixture invalid: precomposed and decomposed forms are byte-identical")
	}
	if len(decomposed) <= len(precomposed) {
		t.Fatalf("test fixture invalid: decomposed form (%d bytes) should be longer than precomposed (%d bytes)", len(decomposed), len(precomposed))
	}

	got1, got2 := Fold(precomposed), Fold(decomposed)
	if got1 != got2 {
		t.Errorf("Fold(precomposed) = %q, Fold(decomposed) = %q, want equal (NFC not applied)", got1, got2)
	}
	if got1 != precomposed {
		t.Errorf("Fold(%q) = %q, want %q (unchanged: already lowercase, not part of the I-family)", precomposed, got1, precomposed)
	}
}

// TestFoldEmptyString guards the trivial edge case used whenever a caller
// folds a zero-length token during relaxation-ladder truncation.
func TestFoldEmptyString(t *testing.T) {
	if got := Fold(""); got != "" {
		t.Errorf("Fold(\"\") = %q, want empty string", got)
	}
}
