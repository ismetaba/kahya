package secretlane

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kahya/kahyad/internal/textnorm"
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

// TestClassifyQwenIntentFlowsThroughOnHappyPath proves W4-08's combined-
// call extension: the classifier prompt's own "intent" field, once Qwen
// answers it, lands on Verdict.Intent unchanged - the SAME round trip that
// already decided secret_lane/category.
func TestClassifyQwenIntentFlowsThroughOnHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"secret_lane\":false,\"category\":\"none\",\"intent\":\"extract\"}"}}]}`))
	}))
	defer srv.Close()

	qwen := NewHTTPQwenClassifier(srv.URL, "qwen3-30b-a3b")
	c := NewClassifier(qwen)
	v, err := c.Classify(context.Background(), "şu maildeki tarihleri çıkarır mısın")
	if err != nil {
		t.Fatalf("Classify() error = %v, want nil", err)
	}
	if v.Intent != "extract" {
		t.Errorf("Intent = %q, want %q", v.Intent, "extract")
	}
}

// TestClassifyDeterministicHitNeverSetsIntent proves the deterministic
// pre-pass (no model consulted) never fabricates an intent value - a
// caller must fall through to router.ClassifyIntent's own model path (or a
// deterministic default of its own) to get one.
func TestClassifyDeterministicHitNeverSetsIntent(t *testing.T) {
	c := NewClassifier(nil)
	v, err := c.Classify(context.Background(), "IBAN: TR330006100519786457841326")
	if err != nil {
		t.Fatalf("Classify() error = %v, want nil (deterministic hit)", err)
	}
	if v.Intent != "" {
		t.Errorf("Intent = %q, want empty (deterministic pre-pass never classifies intent)", v.Intent)
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

// --- FIX-B-2: lexicon terms match only at a WORD START (prefix entries)
// or as a WHOLE WORD (whole-word entries), never buried inside another
// word. Fixtures are byte-exact Turkish (never ASCII-folded by hand). ---

// TestClassifyDeterministicLexiconWordStartFalsePositives is review
// finding B-2's reproduction: before the fix every row below except the
// guard rows was a deterministic secret-lane hit, because the lexicon was
// matched with strings.Contains on Fold(text) - the passive-future suffix
// "-ılacak" folds to "ilacak" and contains "ilac", and "defter"/"sefte"/
// "Eftalya" contain "eft".
func TestClassifyDeterministicLexiconWordStartFalsePositives(t *testing.T) {
	for _, s := range []string{
		// "ilaç"/"ilac" inside the passive future suffix -ılacak.
		"Toplantı yarın yapılacak",
		"kullanılacak",
		"anlatılacak",
		"yazılacak",
		"katılacak",
		// "eft" inside ordinary words (whole-word entry since FIX-B-2).
		"Bu defter çok güzel",
		"defterdeki notlar",
		"sefte",
		"Eftalya ile görüştüm",
		"eftelik",
		// "şifre"/"sifre" inside deşifre (decode).
		"deşifre etmek",
		"desifre etmek",
		// "psikolog" inside a longer non-medical word.
		"parapsikolog",
		// Guard rows: never a hit, before or after the fix.
		"kılavuz",
		"hesaplama",
		"havalandırma",
		"psikoloji dersi",
		// A digit glued to the term is not a word start (\p{N} is a word
		// rune) - a deliberate, accepted change from the pre-fix behavior.
		"2ilaç",
	} {
		if v, ok := classifyDeterministic(s); ok {
			t.Errorf("%q: ok=true verdict=%+v, want no deterministic hit", s, v)
		}
	}
}

// TestClassifyDeterministicLexiconSuffixedTruePositives locks the other
// side of the word-start rule: a prefix entry still tolerates any Turkish
// suffix chain after it, and every non-letter/non-digit rune (space,
// punctuation, '/', apostrophe, text start/end) is a word boundary.
func TestClassifyDeterministicLexiconSuffixedTruePositives(t *testing.T) {
	for _, tc := range []struct {
		text, category string
	}{
		{"ilaçlarımı aldım", CategorySaglik},
		{"ilacımı içtim", CategorySaglik},
		{"İLAÇ listesi", CategorySaglik},
		{"bu konu hakkında ilaç yok", CategorySaglik},
		{"hastaneye gittim", CategorySaglik},
		{"reçetemi yazdır", CategorySaglik},
		{"e-reçete kodu geldi", CategorySaglik},
		{"tahlillerim çıktı", CategorySaglik},
		{"sağlığım iyi", CategorySaglik},
		{"Doktor raporu ekte", CategorySaglik},
		{"ozetle: ~/Documents/saglik/tahlil.md", CategorySaglik},
		{"nöropsikiyatri randevum yarın", CategorySaglik},
		{"şifremi unuttum", CategoryFinans},
		{"havaleyi yaptım", CategoryFinans},
		{"banka hesabıma yatır", CategoryFinans},
		{"doğum tarihiniz nedir", CategoryKimlik},
		{"TCKN'im lazım", CategoryKimlik},
		// Prefix rule: same as the pre-fix behavior (decision G02-1 keeps
		// the sağlık root prefix-matched - fail-closed).
		{"sağlıklı beslenme", CategorySaglik},
		{"sağlıkta ve hastalıkta", CategorySaglik},
		{"(ilaç)", CategorySaglik},
		{"ilaç", CategorySaglik},
	} {
		v, ok := classifyDeterministic(tc.text)
		if !ok || !v.SecretLane {
			t.Errorf("%q: ok=%v verdict=%+v, want a deterministic secret-lane hit", tc.text, ok, v)
			continue
		}
		if v.Category != tc.category {
			t.Errorf("%q: category = %q, want %q", tc.text, v.Category, tc.category)
		}
	}
}

// TestClassifyDeterministicLexiconWholeWordEFT pins the three-letter "eft"
// entry to whole-word matching.
func TestClassifyDeterministicLexiconWholeWordEFT(t *testing.T) {
	for _, s := range []string{
		"EFT ile gönderdim",
		"eft'le yolladım",
		"ödeme/eft bilgisi",
		"eft",
		"(eft)",
	} {
		v, ok := classifyDeterministic(s)
		if !ok || v.Category != CategoryFinans || v.Reason != "keyword:eft" {
			t.Errorf("%q: ok=%v verdict=%+v, want finans keyword:eft", s, ok, v)
		}
	}
	// The lexicon is scanned in order and "havale" precedes "eft": the
	// first hit wins (same as before the fix).
	if v, ok := classifyDeterministic("havale/eft bilgisi"); !ok || v.Reason != "keyword:havale" {
		t.Errorf("havale/eft bilgisi: ok=%v verdict=%+v, want keyword:havale", ok, v)
	}
	for _, s := range []string{"defter", "sefte", "eftelik", "eft1"} {
		if v, ok := classifyDeterministic(s); ok {
			t.Errorf("%q: ok=true verdict=%+v, want no deterministic hit", s, v)
		}
	}
}

// TestClassifyDeterministicLexiconEntriesFolded keeps every lexicon entry
// pre-folded and non-empty: an unfolded entry would never match Fold(text),
// and an empty one would match at every offset.
func TestClassifyDeterministicLexiconEntriesFolded(t *testing.T) {
	for _, lex := range [][]lexEntry{saglikKeywords, finansKeywords, kimlikKeywords} {
		for _, e := range lex {
			if e.text == "" {
				t.Errorf("empty lexicon entry %+v", e)
				continue
			}
			if got := textnorm.Fold(e.text); got != e.text {
				t.Errorf("lexicon entry %q is not pre-folded (Fold = %q)", e.text, got)
			}
		}
	}
}

// TestClassifyDeterministicLexiconEveryEntryWordStartOnly gives EVERY
// lexicon entry its own false-positive check: the entry glued after a
// letter or digit (buried inside another word) never matches, while the
// entry at text start, after a space, and between punctuation always
// does; a prefix entry keeps matching with a Turkish suffix after it, a
// whole-word entry does not. The same entry text, run through the full
// pre-pass, is a hit of the lexicon's own category.
func TestClassifyDeterministicLexiconEveryEntryWordStartOnly(t *testing.T) {
	for _, lex := range []struct {
		category string
		entries  []lexEntry
	}{
		{CategorySaglik, saglikKeywords},
		{CategoryFinans, finansKeywords},
		{CategoryKimlik, kimlikKeywords},
	} {
		for _, e := range lex.entries {
			for _, glue := range []string{"a", "ş", "ü", "7"} {
				if matchLexicon(glue+e.text, e) {
					t.Errorf("%q matched inside %q, want no match mid-word", e.text, glue+e.text)
				}
				if matchLexicon("x "+glue+e.text+" y", e) {
					t.Errorf("%q matched inside %q, want no match mid-word", e.text, "x "+glue+e.text+" y")
				}
			}
			for _, ok := range []string{e.text, "x " + e.text, "(" + e.text + ")", "~/" + e.text + ".md", "a" + e.text + " " + e.text} {
				if !matchLexicon(ok, e) {
					t.Errorf("%q did not match %q, want a word-start match", e.text, ok)
				}
			}
			suffixed := e.text + "ler"
			if got := matchLexicon(suffixed, e); got == e.wholeWord {
				t.Errorf("%q in %q: match=%v, want %v (wholeWord=%v)", e.text, suffixed, got, !e.wholeWord, e.wholeWord)
			}
			if !matchLexicon(e.text+"'ler", e) {
				t.Errorf("%q did not match %q, want the apostrophe to end the word", e.text, e.text+"'ler")
			}
			v, ok := classifyDeterministic(e.text)
			if !ok || v.Category != lex.category {
				t.Errorf("classifyDeterministic(%q) = %+v ok=%v, want category %q", e.text, v, ok, lex.category)
			}
		}
	}
}

// Large-input benchmarks (review finding B-2a): fs_read classifies the
// WHOLE file on every read, so the lexicon must stay a SIMD substring scan
// (strings.Index) plus an O(1) boundary check per occurrence - never a
// per-entry regexp, which measured ~20x slower on 1 MiB. No timing
// assertion runs inside make test; compare with
//
//	go test -tags sqlite_fts5 -run '^$' -bench ClassifyDeterministicLarge ./kahyad/internal/secretlane/
func benchmarkClassifyDeterministicLarge(b *testing.B, unit string) {
	text := strings.Repeat(unit, (1<<20)/len(unit)+1)[:1<<20]
	b.SetBytes(1 << 20)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		classifyDeterministic(text)
	}
}

// BenchmarkClassifyDeterministicLargeNoHit: 1 MiB with no lexicon term at
// all (every entry scans the whole text).
func BenchmarkClassifyDeterministicLargeNoHit(b *testing.B) {
	benchmarkClassifyDeterministicLarge(b, "merhaba dünya bugün hava güzel toplantı yarın saat üçte ofiste olacak ")
}

// BenchmarkClassifyDeterministicLargePassive: 1 MiB where "ilac" and "eft"
// occur thousands of times INSIDE words (the boundary-check path) but no
// lexicon term occurs as a word.
func BenchmarkClassifyDeterministicLargePassive(b *testing.B) {
	benchmarkClassifyDeterministicLarge(b, "rapor yarın yapılacak ve sunum pazartesi anlatılacak; notlar defterde yazılacak ")
}
