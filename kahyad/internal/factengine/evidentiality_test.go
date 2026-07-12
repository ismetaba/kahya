package factengine

import "testing"

// TestDetectEvidentialityByteExactFixtures uses the task spec's own
// byte-exact Turkish fixtures verbatim - never ASCII-fold these
// (tasks/README.md, CLAUDE.md).
func TestDetectEvidentialityByteExactFixtures(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{"reported_ayrilmis", "Emre işten ayrılmış.", Reported},
		{"witnessed_ayrildi", "Emre işten ayrıldı, bugün konuştuk.", Witnessed},
		{"reported_gecmis", "Toplantı iyi geçmiş.", Reported},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DetectEvidentialityFromText(tc.text)
			if got != tc.want {
				t.Errorf("DetectEvidentialityFromText(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}

// TestDetectEvidentialityMusMuşCoverage exercises the two vowel-harmony
// variants ("-muş"/"-müş") the byte-exact fixtures above do not happen to
// cover (they only exercise "-mış"/"-miş", which fold to the same string
// via textnorm.Fold's dotless-i collapse).
func TestDetectEvidentialityMusMuşCoverage(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{"back_rounded_unutmus", "Toplantıyı unutmuş.", Reported},
		{"front_rounded_donmus", "Ali erken dönmüş.", Reported},
		{"witnessed_geldi", "Ali erken geldi, ben de gördüm.", Witnessed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DetectEvidentialityFromText(tc.text)
			if got != tc.want {
				t.Errorf("DetectEvidentialityFromText(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}

func TestNormalizeEvidentialityDefaultsToInferredWhenEmpty(t *testing.T) {
	got, err := NormalizeEvidentiality("")
	if err != nil {
		t.Fatalf("NormalizeEvidentiality(\"\") error = %v, want nil", err)
	}
	if got != Inferred {
		t.Errorf("NormalizeEvidentiality(\"\") = %q, want %q", got, Inferred)
	}
}

func TestNormalizeEvidentialityAcceptsValidEnum(t *testing.T) {
	for _, v := range []string{Witnessed, Reported, Inferred} {
		got, err := NormalizeEvidentiality(v)
		if err != nil {
			t.Fatalf("NormalizeEvidentiality(%q) error = %v, want nil", v, err)
		}
		if got != v {
			t.Errorf("NormalizeEvidentiality(%q) = %q, want %q", v, got, v)
		}
	}
}

func TestNormalizeEvidentialityRejectsInvalidEnum(t *testing.T) {
	if _, err := NormalizeEvidentiality("bogus"); err == nil {
		t.Fatal("NormalizeEvidentiality(\"bogus\") error = nil, want error")
	}
}
