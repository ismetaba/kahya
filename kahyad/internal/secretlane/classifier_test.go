package secretlane

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// --- Deterministic pre-pass fixtures (task spec step 7, byte-exact) ---

func TestClassifyDeterministicIBAN(t *testing.T) {
	c := NewClassifier(nil) // no Qwen wired - if the pre-pass didn't catch this, the test would fail closed for the WRONG reason
	v, err := c.Classify(context.Background(), "Hesap numaram: TR33 0006 1005 1978 6457 8413 26 lütfen bu hesaba gönderin")
	if err != nil {
		t.Fatalf("Classify() error = %v, want nil (deterministic hit)", err)
	}
	if !v.SecretLane {
		t.Fatal("Classify() SecretLane = false, want true for an IBAN")
	}
	if v.Category != CategoryFinans {
		t.Errorf("Category = %q, want %q", v.Category, CategoryFinans)
	}
}

func TestClassifyDeterministicTCKN(t *testing.T) {
	c := NewClassifier(nil)
	v, err := c.Classify(context.Background(), "TC kimlik no: 10000000146")
	if err != nil {
		t.Fatalf("Classify() error = %v, want nil (deterministic hit)", err)
	}
	if !v.SecretLane {
		t.Fatal("Classify() SecretLane = false, want true for a valid TCKN")
	}
	if v.Category != CategoryKimlik {
		t.Errorf("Category = %q, want %q", v.Category, CategoryKimlik)
	}
}

func TestClassifyDeterministicRejectsInvalidTCKNChecksum(t *testing.T) {
	// An 11-digit run that FAILS the checksum must not itself trigger the
	// deterministic pre-pass (it would fall through to Qwen instead) -
	// proves this isn't just "any 11 digits".
	if isValidTCKN("10000000147") {
		t.Fatal("isValidTCKN(10000000147) = true, want false (bad checksum)")
	}
}

func TestClassifyDeterministicSaglikKeyword(t *testing.T) {
	c := NewClassifier(nil)
	v, err := c.Classify(context.Background(), "Doktor bugün tahlil sonuçları geldi dedi.")
	if err != nil {
		t.Fatalf("Classify() error = %v, want nil (deterministic hit)", err)
	}
	if !v.SecretLane {
		t.Fatal("Classify() SecretLane = false, want true for 'tahlil sonuçları'")
	}
	if v.Category != CategorySaglik {
		t.Errorf("Category = %q, want %q", v.Category, CategorySaglik)
	}
}

func TestClassifyDeterministicFinansKeyword(t *testing.T) {
	c := NewClassifier(nil)
	v, err := c.Classify(context.Background(), "Şu dosyayı özetle: kredi kartı ekstresi Ocak 2026")
	if err != nil {
		t.Fatalf("Classify() error = %v, want nil (deterministic hit)", err)
	}
	if !v.SecretLane {
		t.Fatal("Classify() SecretLane = false, want true for 'kredi kartı ekstresi'")
	}
	if v.Category != CategoryFinans {
		t.Errorf("Category = %q, want %q", v.Category, CategoryFinans)
	}
}

func TestClassifyDeterministicCardNumberLuhn(t *testing.T) {
	c := NewClassifier(nil)
	// 4539 1488 0343 6467 is a well-known Luhn-valid test Visa PAN.
	v, err := c.Classify(context.Background(), "kart no 4539 1488 0343 6467")
	if err != nil {
		t.Fatalf("Classify() error = %v, want nil", err)
	}
	if !v.SecretLane || v.Category != CategoryFinans {
		t.Errorf("Classify() = %+v, want secret-lane finans", v)
	}
}

func TestClassifyDeterministicCVV(t *testing.T) {
	c := NewClassifier(nil)
	v, err := c.Classify(context.Background(), "cvv: 123")
	if err != nil {
		t.Fatalf("Classify() error = %v, want nil", err)
	}
	if !v.SecretLane || v.Category != CategoryFinans {
		t.Errorf("Classify() = %+v, want secret-lane finans", v)
	}
}

func TestClassifyDeterministicASCIIFoldedKeywordVariant(t *testing.T) {
	c := NewClassifier(nil)
	v, err := c.Classify(context.Background(), "ozetle: ~/Documents/saglik/tahlil.md")
	if err != nil {
		t.Fatalf("Classify() error = %v, want nil", err)
	}
	if !v.SecretLane {
		t.Fatal("Classify() SecretLane = false, want true for ASCII-folded 'saglik'")
	}
}

// --- Post-review regression fixtures: format-robustness of the
// deterministic pre-pass (mis-formatted / sentence-embedded structured
// secrets must not sail past to the cloud). See
// tasks/w3-policy-tools/W3-08-secret-lane-local.md's "Post-review scope
// note".

func TestClassifyDeterministicIBANUnspaced(t *testing.T) {
	c := NewClassifier(nil)
	v, err := c.Classify(context.Background(), "IBAN'ım TR330006100519786457841326")
	if err != nil {
		t.Fatalf("Classify() error = %v, want nil (deterministic hit)", err)
	}
	if !v.SecretLane || v.Category != CategoryFinans {
		t.Errorf("Classify() = %+v, want secret-lane finans for an unspaced IBAN", v)
	}
}

func TestClassifyDeterministicIBANDashSeparated(t *testing.T) {
	c := NewClassifier(nil)
	v, err := c.Classify(context.Background(), "Hesap: TR33-0006-1005-1978-6457-8413-26 bu hesaba gönderin")
	if err != nil {
		t.Fatalf("Classify() error = %v, want nil (deterministic hit)", err)
	}
	if !v.SecretLane || v.Category != CategoryFinans {
		t.Errorf("Classify() = %+v, want secret-lane finans for a dash-separated IBAN", v)
	}
}

func TestClassifyDeterministicIBANEmbeddedInSentence(t *testing.T) {
	c := NewClassifier(nil)
	v, err := c.Classify(context.Background(), "arkadaşıma TR33 0006 1005 1978 6457 8413 26 numaralı hesaptan para gönderdim dün akşam")
	if err != nil {
		t.Fatalf("Classify() error = %v, want nil (deterministic hit)", err)
	}
	if !v.SecretLane || v.Category != CategoryFinans {
		t.Errorf("Classify() = %+v, want secret-lane finans for a mid-sentence IBAN", v)
	}
}

func TestClassifyDeterministicTCKNEmbeddedInSentence(t *testing.T) {
	c := NewClassifier(nil)
	v, err := c.Classify(context.Background(), "TC kimlik numaram 10000000146 şeklinde, gerekirse iletirim")
	if err != nil {
		t.Fatalf("Classify() error = %v, want nil (deterministic hit)", err)
	}
	if !v.SecretLane || v.Category != CategoryKimlik {
		t.Errorf("Classify() = %+v, want secret-lane kimlik for a sentence-embedded TCKN", v)
	}
}

func TestClassifyDeterministicCardNumberDashSeparated(t *testing.T) {
	c := NewClassifier(nil)
	v, err := c.Classify(context.Background(), "kartım 4111-1111-1111-1111 son kullanma 12/29")
	if err != nil {
		t.Fatalf("Classify() error = %v, want nil (deterministic hit)", err)
	}
	if !v.SecretLane || v.Category != CategoryFinans {
		t.Errorf("Classify() = %+v, want secret-lane finans for a dash-separated card number", v)
	}
}

func TestClassifyDeterministicCardNumberUnspaced(t *testing.T) {
	c := NewClassifier(nil)
	v, err := c.Classify(context.Background(), "kart no 4111111111111111")
	if err != nil {
		t.Fatalf("Classify() error = %v, want nil (deterministic hit)", err)
	}
	if !v.SecretLane || v.Category != CategoryFinans {
		t.Errorf("Classify() = %+v, want secret-lane finans for an unspaced card number", v)
	}
}

func TestClassifyDeterministicCVVWithCue(t *testing.T) {
	c := NewClassifier(nil)
	v, err := c.Classify(context.Background(), "kart arkasındaki cvv 123 lazım olacak")
	if err != nil {
		t.Fatalf("Classify() error = %v, want nil (deterministic hit)", err)
	}
	if !v.SecretLane || v.Category != CategoryFinans {
		t.Errorf("Classify() = %+v, want secret-lane finans for 'cvv 123'", v)
	}
}

func TestClassifyDeterministicGuvenlikKoduCue(t *testing.T) {
	c := NewClassifier(nil)
	v, err := c.Classify(context.Background(), "güvenlik kodu 123 az önce geldi")
	if err != nil {
		t.Fatalf("Classify() error = %v, want nil (deterministic hit)", err)
	}
	if !v.SecretLane || v.Category != CategoryFinans {
		t.Errorf("Classify() = %+v, want secret-lane finans for 'güvenlik kodu 123'", v)
	}
}

func TestClassifyDeterministicCVVBareDigitsNotSecretLane(t *testing.T) {
	// Guards the false-positive side: a bare 3-digit number with NO
	// cvv/güvenlik-kodu cue must NOT itself trip the deterministic
	// pre-pass (it would fall through to Qwen instead, same as any other
	// ordinary text).
	v, ok := classifyDeterministic("oda numaram 123, akşam görüşürüz")
	if ok {
		t.Errorf("classifyDeterministic() = %+v, ok=true, want ok=false for a bare uncued 3-digit number", v)
	}
}

func TestClassifyDeterministicSaglikSuffixedSagligim(t *testing.T) {
	// "sağlığım" (my health) is the consonant-softened (yumuşama) form of
	// "sağlık" - plain "sağlık" is never a substring of it, so this
	// exercises the dedicated "sağlığ" stem entry.
	c := NewClassifier(nil)
	v, err := c.Classify(context.Background(), "son zamanlarda sağlığım pek iyi değil, doktora gitmem lazım")
	if err != nil {
		t.Fatalf("Classify() error = %v, want nil (deterministic hit)", err)
	}
	if !v.SecretLane || v.Category != CategorySaglik {
		t.Errorf("Classify() = %+v, want secret-lane saglik for 'sağlığım'", v)
	}
}

func TestClassifyDeterministicSaglikSuffixedTahlillerim(t *testing.T) {
	// "tahlillerim" (my test results) contains "tahlil" as a prefix - the
	// bare "tahlil" lexicon entry (added alongside the existing "tahlil
	// sonuçları" phrases) is what catches this.
	c := NewClassifier(nil)
	v, err := c.Classify(context.Background(), "tahlillerim yarın çıkacakmış, biraz endişeliyim")
	if err != nil {
		t.Fatalf("Classify() error = %v, want nil (deterministic hit)", err)
	}
	if !v.SecretLane || v.Category != CategorySaglik {
		t.Errorf("Classify() = %+v, want secret-lane saglik for 'tahlillerim'", v)
	}
}

func TestClassifyDeterministicMixedPayloadSecretOnOneLine(t *testing.T) {
	// A single secret-lane line anywhere in an otherwise ordinary
	// multi-line payload must classify the WHOLE payload secret-lane -
	// the deterministic pre-pass scans the entire text, not line-by-line
	// with an early "looks fine so far" exit.
	c := NewClassifier(nil)
	payload := "bugün hava çok güzel, parkta yürüyüş yaptım\n" +
		"akşam yemeği için markete uğrayacağım\n" +
		"bu arada IBAN'ım TR330006100519786457841326, unutmadan not aldım\n" +
		"yarın toplantı saat 10'da"
	v, err := c.Classify(context.Background(), payload)
	if err != nil {
		t.Fatalf("Classify() error = %v, want nil (deterministic hit)", err)
	}
	if !v.SecretLane || v.Category != CategoryFinans {
		t.Errorf("Classify() = %+v, want secret-lane finans for a multi-line payload with an IBAN on line 3", v)
	}
}

// --- Fail-closed: no local classifier / Qwen error / non-JSON ---

func TestClassifyNilQwenFailsClosed(t *testing.T) {
	c := NewClassifier(nil)
	v, err := c.Classify(context.Background(), "bugün hava çok güzel, parkta yürüyüş yaptım")
	if err == nil {
		t.Fatal("Classify() error = nil, want an error (no classifier model wired)")
	}
	if !v.SecretLane {
		t.Fatal("Classify() SecretLane = false, want true (fail-closed with no Qwen wired)")
	}
}

func TestClassifyQwenErrorFailsClosed(t *testing.T) {
	wantErr := errors.New("connection refused")
	qwen := QwenClassifierFunc(func(ctx context.Context, text string) (Verdict, error) {
		return Verdict{}, wantErr
	})
	c := NewClassifier(qwen)
	v, err := c.Classify(context.Background(), "bugün hava çok güzel")
	if !errors.Is(err, wantErr) {
		t.Fatalf("Classify() error = %v, want wrapping %v", err, wantErr)
	}
	if !v.SecretLane {
		t.Fatal("Classify() SecretLane = false, want true (fail-closed on qwen error)")
	}
}

func TestClassifyQwenNonJSONResponseFailsClosed(t *testing.T) {
	// HTTPQwenClassifier against a fake server that answers a chat
	// completion whose message content is plain prose, not JSON at all -
	// task spec: "Non-JSON or error => secret_lane: true (FAIL-CLOSED)".
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Sure, I can help with that!"}}]}`))
	}))
	defer srv.Close()

	qwen := NewHTTPQwenClassifier(srv.URL, "qwen3-30b-a3b")
	c := NewClassifier(qwen)
	v, err := c.Classify(context.Background(), "bugün hava çok güzel")
	if err == nil {
		t.Fatal("Classify() error = nil, want error for a non-JSON model response")
	}
	if !v.SecretLane {
		t.Fatal("Classify() SecretLane = false, want true (fail-closed on non-JSON response)")
	}
}

func TestClassifyQwenInvalidCategoryFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"secret_lane\":false,\"category\":\"weather\"}"}}]}`))
	}))
	defer srv.Close()

	qwen := NewHTTPQwenClassifier(srv.URL, "qwen3-30b-a3b")
	c := NewClassifier(qwen)
	v, err := c.Classify(context.Background(), "bugün hava çok güzel")
	if err == nil {
		t.Fatal("Classify() error = nil, want error for an invalid category value")
	}
	if !v.SecretLane {
		t.Fatal("Classify() SecretLane = false, want true (fail-closed on invalid category)")
	}
}

func TestClassifyQwenUpstream500FailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	qwen := NewHTTPQwenClassifier(srv.URL, "qwen3-30b-a3b")
	c := NewClassifier(qwen)
	v, err := c.Classify(context.Background(), "bugün hava çok güzel")
	if err == nil {
		t.Fatal("Classify() error = nil, want error for a 500 upstream response")
	}
	if !v.SecretLane {
		t.Fatal("Classify() SecretLane = false, want true (fail-closed on upstream error)")
	}
}

// --- Happy path: a well-behaved fake Qwen server correctly says "none" ---

func TestClassifyQwenHappyPathNotSecretLane(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"secret_lane\":false,\"category\":\"none\"}"}}]}`))
	}))
	defer srv.Close()

	qwen := NewHTTPQwenClassifier(srv.URL, "qwen3-30b-a3b")
	c := NewClassifier(qwen)
	v, err := c.Classify(context.Background(), "bugün hava çok güzel, parkta yürüyüş yaptım")
	if err != nil {
		t.Fatalf("Classify() error = %v, want nil", err)
	}
	if v.SecretLane {
		t.Error("Classify() SecretLane = true, want false for genuinely benign text")
	}
	if v.Category != CategoryNone {
		t.Errorf("Category = %q, want %q", v.Category, CategoryNone)
	}
}

func TestClassifyQwenHappyPathSecretLane(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"secret_lane\":true,\"category\":\"saglik\"}"}}]}`))
	}))
	defer srv.Close()

	qwen := NewHTTPQwenClassifier(srv.URL, "qwen3-30b-a3b")
	c := NewClassifier(qwen)
	// This text does NOT trip the deterministic pre-pass, so this genuinely
	// exercises the Qwen leg.
	v, err := c.Classify(context.Background(), "geçen hafta doktora gittim, biraz endişeliyim")
	if err != nil {
		t.Fatalf("Classify() error = %v, want nil", err)
	}
	if !v.SecretLane || v.Category != CategorySaglik {
		t.Errorf("Classify() = %+v, want secret-lane saglik", v)
	}
}

// --- Ordering invariant at the classifier level: a hanging Qwen call must
// not itself resolve to a verdict until it actually returns. ---

func TestClassifyBlocksOnHangingQwenUntilCtxDone(t *testing.T) {
	release := make(chan struct{})
	qwen := QwenClassifierFunc(func(ctx context.Context, text string) (Verdict, error) {
		select {
		case <-release:
			return Verdict{SecretLane: false, Category: CategoryNone}, nil
		case <-ctx.Done():
			return Verdict{}, ctx.Err()
		}
	})
	c := NewClassifier(qwen)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		v, err := c.Classify(ctx, "hiçbir kalıba uymayan sıradan bir metin")
		if err != nil {
			t.Errorf("Classify() error = %v, want nil once released", err)
		}
		if v.SecretLane {
			t.Errorf("Classify() SecretLane = true, want false")
		}
	}()

	// Give the goroutine time to actually reach the hanging Qwen call.
	time.Sleep(100 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("Classify() returned before the hanging Qwen call was released")
	default:
	}

	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Classify() never returned after the Qwen call was released")
	}
}
