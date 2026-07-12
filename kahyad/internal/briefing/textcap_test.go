package briefing

import "testing"

func TestCapTextTruncatesToMaxRunes(t *testing.T) {
	s := "abcdefghij"
	got := capText(s, 5)
	if len(got) != 5 {
		t.Fatalf("capText(%q, 5) = %q (len %d), want len 5", s, got, len(got))
	}
}

func TestCapTextReplacesControlAndNewlineRunes(t *testing.T) {
	got := capText("line one\nline\ttwo", 100)
	for _, r := range got {
		if r == '\n' || r == '\t' {
			t.Fatalf("capText result %q still contains a raw newline/tab", got)
		}
	}
}

func TestCapTextHandlesMultibyteTurkishRunes(t *testing.T) {
	s := "sağlık ile ilgili çok uzun bir başlık burada devam ediyor"
	got := capText(s, 10)
	if n := len([]rune(got)); n > 10 {
		t.Fatalf("capText rune count = %d, want <= 10", n)
	}
}
