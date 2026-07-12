package reader

import (
	"strings"
	"testing"
)

func TestValidateMailSummaryV1Valid(t *testing.T) {
	v := MailSummaryV1{
		FromDisplay: "Fatura Servisi",
		Subject:     "Fatura bildirimi",
		Summary:     "Son ödeme tarihi 15 Temmuz 2026, tutar 4.250,00 TL.",
		Dates:       []string{"2026-07-15T00:00:00Z"},
		Amounts:     []string{"4.250,00 TL"},
	}
	got, err := ValidateMailSummaryV1(v)
	if err != nil {
		t.Fatalf("ValidateMailSummaryV1: %v", err)
	}
	if got.Summary != v.Summary {
		t.Errorf("Summary = %q, want %q (Turkish diacritics must survive byte-exact)", got.Summary, v.Summary)
	}
}

func TestValidateMailSummaryV1RejectsOverLongSubject(t *testing.T) {
	v := MailSummaryV1{Subject: strings.Repeat("a", mailSubjectMaxLen+1)}
	if _, err := ValidateMailSummaryV1(v); err == nil {
		t.Fatal("expected an error for an over-length subject")
	}
}

func TestValidateMailSummaryV1RejectsControlChar(t *testing.T) {
	v := MailSummaryV1{Summary: "hello\x00world"}
	if _, err := ValidateMailSummaryV1(v); err == nil {
		t.Fatal("expected an error for a NUL control character")
	}
}

func TestValidateMailSummaryV1RejectsBidiOverride(t *testing.T) {
	// U+202E (RIGHT-TO-LEFT OVERRIDE) - the classic Trojan-Source vector.
	v := MailSummaryV1{Subject: "invoice‮gnp.exe"}
	if _, err := ValidateMailSummaryV1(v); err == nil {
		t.Fatal("expected an error for a bidi override code point")
	}
}

func TestValidateMailSummaryV1RejectsZeroWidth(t *testing.T) {
	v := MailSummaryV1{Subject: "invo​ice"}
	if _, err := ValidateMailSummaryV1(v); err == nil {
		t.Fatal("expected an error for a zero-width code point")
	}
}

func TestValidateMailSummaryV1RejectsNewlineInField(t *testing.T) {
	v := MailSummaryV1{Subject: "line one\nline two"}
	if _, err := ValidateMailSummaryV1(v); err == nil {
		t.Fatal("expected an error for an embedded newline")
	}
}

func TestValidateMailSummaryV1RejectsNonRFC3339Date(t *testing.T) {
	v := MailSummaryV1{Dates: []string{"15/07/2026"}}
	if _, err := ValidateMailSummaryV1(v); err == nil {
		t.Fatal("expected an error for a non-RFC3339 date")
	}
}

func TestValidateMailSummaryV1AcceptsUSDAndEUR(t *testing.T) {
	for _, amt := range []string{"100.00 USD", "50,00EUR", "4.250,00 TL"} {
		v := MailSummaryV1{Amounts: []string{amt}}
		if _, err := ValidateMailSummaryV1(v); err != nil {
			t.Errorf("amount %q: unexpected error: %v", amt, err)
		}
	}
}

func TestValidateMailSummaryV1RejectsMalformedAmount(t *testing.T) {
	for _, amt := range []string{"100 GBP", "TL 100", "abc"} {
		v := MailSummaryV1{Amounts: []string{amt}}
		if _, err := ValidateMailSummaryV1(v); err == nil {
			t.Errorf("amount %q: expected an error, got none", amt)
		}
	}
}

func TestValidateWebpageExtractV1Valid(t *testing.T) {
	v := WebpageExtractV1{
		Title:     "Örnek Başlık",
		KeyPoints: []string{"birinci nokta", "ikinci nokta"},
	}
	got, err := ValidateWebpageExtractV1(v)
	if err != nil {
		t.Fatalf("ValidateWebpageExtractV1: %v", err)
	}
	if len(got.KeyPoints) != 2 {
		t.Errorf("KeyPoints = %+v, want 2 entries", got.KeyPoints)
	}
}

func TestValidateWebpageExtractV1RejectsTooManyKeyPoints(t *testing.T) {
	points := make([]string, webpageKeyPointsMax+1)
	for i := range points {
		points[i] = "nokta"
	}
	v := WebpageExtractV1{Title: "t", KeyPoints: points}
	if _, err := ValidateWebpageExtractV1(v); err == nil {
		t.Fatal("expected an error for more than 10 key_points")
	}
}

func TestValidateWebpageExtractV1RejectsOverLongKeyPoint(t *testing.T) {
	v := WebpageExtractV1{Title: "t", KeyPoints: []string{strings.Repeat("a", webpageKeyPointMaxLen+1)}}
	if _, err := ValidateWebpageExtractV1(v); err == nil {
		t.Fatal("expected an error for an over-length key_point")
	}
}
