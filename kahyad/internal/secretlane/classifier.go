// Package secretlane implements W3-08: the ingest-time content classifier
// and Go routing branch that makes finans/sağlık/kimlik (identity) data
// physically unable to reach a cloud model (HANDOFF §4 ⚑ ordering
// invariant + §5 product principle "gizlilik kodda").
//
// This file (classifier.go) is the classifier itself. Order (task spec,
// verbatim): DETERMINISTIC regex/lexicon FIRST - a hit is FINAL (secret-
// lane, no model needed); OTHERWISE fall through to the local Qwen3-30B-A3B
// server for a strict-JSON classification. Any Qwen error, timeout, or
// non-JSON response is FAIL-CLOSED: secret_lane:true. router.go is the Go
// branch that acts on a Verdict (envelope lane pinning, task-row
// persistence, the proxy backstop).
package secretlane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"kahya/kahyad/internal/textnorm"
)

// Category values a Verdict may carry (task spec: "strict JSON
// {secret_lane: bool, category: finans|saglik|kimlik|none}").
const (
	CategoryFinans = "finans"
	CategorySaglik = "saglik"
	CategoryKimlik = "kimlik"
	CategoryNone   = "none"
	// CategoryUnknown is used ONLY by this package's own fail-closed
	// branches (Qwen error/timeout/no-classifier-wired) - never a value
	// the Qwen prompt itself is asked to return. secret_lane is still
	// true in every case that uses this - HANDOFF's fail-closed posture
	// cares about the LANE, not about knowing precisely which of the
	// three categories applies when the classifier itself couldn't run.
	CategoryUnknown = "unknown"
)

// Verdict is one classification result.
type Verdict struct {
	SecretLane bool
	Category   string
	// Reason is an internal, English-only diagnostic note of which rule
	// produced this verdict (e.g. "iban", "tckn", "keyword:tahlil
	// sonuçları", "qwen", "qwen_error_fail_closed") - logged, never shown
	// to the user (HANDOFF §3 language policy: user-facing strings are
	// Turkish; this is not one).
	Reason string
}

// --- Deterministic pre-pass: IBAN / TCKN / card number / CVV ---

// ibanCandidateRe finds a Turkish IBAN CANDIDATE: a word-bounded "TR"
// immediately followed by a run of digits and conventional grouping
// separators (space or dash) long enough to contain the required 24
// digits. This is deliberately format-agnostic - it matches the
// four-by-four convention ("TR33 0006 1005 1978 6457 8413 26"), fully
// unspaced ("TR330006100519786457841326"), dash-separated
// ("TR33-0006-1005-1978-6457-8413-26"), or irregular/mixed grouping alike,
// and is NOT anchored to the whole string so it fires just as well
// embedded mid-sentence ("Hesap numaram: TR33... lütfen bu hesaba
// gönderin"). A candidate is only FINAL once isValidIBANStructure
// confirms the right digit COUNT once separators are stripped - "TR"
// followed by some unrelated digits is not enough on its own.
var ibanCandidateRe = regexp.MustCompile(`(?i)\bTR[0-9 \-]{24,40}`)

// isValidIBANStructure reports whether s (an ibanCandidateRe match),
// once every ASCII space/dash grouping separator is stripped out, is
// exactly "TR" + 24 digits - a Turkish IBAN's fixed length regardless of
// how it was grouped in the source text. This mirrors the task spec's own
// format ("TR" + 24 digits) - no mod-97 checksum is required or
// implemented, matching the pre-existing behavior this replaces.
func isValidIBANStructure(s string) bool {
	stripped := stripSeparators(strings.ToUpper(s))
	if !strings.HasPrefix(stripped, "TR") {
		return false
	}
	digits := stripped[2:]
	if len(digits) != 24 {
		return false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// stripSeparators removes ASCII space and dash grouping separators (the
// only two this package's IBAN/card normalization tolerates) from s.
func stripSeparators(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == ' ' || r == '-' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// tcknCandidateRe finds any word-bounded run of exactly 11 digits - a TCKN
// (Turkish national ID number) candidate, verified below by its checksum
// (an arbitrary 11-digit run is NOT enough on its own - e.g. a phone
// number with a leading 0 stripped could collide). \b is a digit/non-digit
// transition, not a whole-string anchor, so this already fires on an 11-
// digit run embedded anywhere in a larger sentence ("TC kimlik numaram
// 10000000146 şeklinde") - confirmed by
// TestClassifyDeterministicTCKNEmbeddedInSentence.
var tcknCandidateRe = regexp.MustCompile(`\b\d{11}\b`)

// cardCandidateRe finds digit runs shaped like a payment card number: 13
// to 19 digits, optionally grouped by spaces OR dashes, in any mix
// (every real card network's PAN length range) - verified below by the
// Luhn checksum after stripNonDigits normalizes away whatever separator
// was used. Confirmed dash-grouped ("4111-1111-1111-1111"), space-grouped
// ("4111 1111 1111 1111") and unspaced all normalize correctly by
// TestClassifyDeterministicCardNumberDashSeparated /
// TestClassifyDeterministicCardNumberUnspaced.
var cardCandidateRe = regexp.MustCompile(`\b(?:\d[ -]?){12,18}\d\b`)

// cvvRe matches an explicit CVV/CVC label followed by a 3-4 digit code -
// task spec fixture keyword: "kredi kartı ekstresi" is covered by the
// finans keyword lexicon below, this pattern is the CVV-specific one. A
// bare 3-4 digit run with NO "cvv"/"cvv2" cue never matches this (avoids
// false-positiving on any random short number) - the "güvenlik kodu" cue
// variant ("güvenlik kodu 123") is instead covered unconditionally by the
// finans keyword lexicon below (it already lists "güvenlik kodu"/
// "guvenlik kodu" as a phrase, independent of whether a number follows),
// so between the two, every context-cued CVV mention is caught while a
// bare "123" alone still is not.
var cvvRe = regexp.MustCompile(`(?i)\bcvv2?\b\s*[:\-=]?\s*\d{3,4}\b`)

// isValidTCKN implements the standard Turkish national ID checksum
// algorithm (11 digits d1..d11, d1 != 0):
//
//	d10 = ((d1+d3+d5+d7+d9)*7 - (d2+d4+d6+d8)) mod 10
//	d11 = (d1+d2+...+d10) mod 10
func isValidTCKN(s string) bool {
	if len(s) != 11 {
		return false
	}
	d := make([]int, 11)
	for i, r := range s {
		if r < '0' || r > '9' {
			return false
		}
		d[i] = int(r - '0')
	}
	if d[0] == 0 {
		return false
	}
	oddSum := d[0] + d[2] + d[4] + d[6] + d[8]
	evenSum := d[1] + d[3] + d[5] + d[7]
	d10 := ((oddSum*7-evenSum)%10 + 10) % 10
	if d10 != d[9] {
		return false
	}
	sum := 0
	for i := 0; i < 10; i++ {
		sum += d[i]
	}
	d11 := sum % 10
	return d11 == d[10]
}

// luhnValid implements the standard Luhn (mod-10) checksum used by every
// major card network's PAN.
func luhnValid(digits string) bool {
	sum := 0
	alt := false
	for i := len(digits) - 1; i >= 0; i-- {
		n := int(digits[i] - '0')
		if n < 0 || n > 9 {
			return false
		}
		if alt {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
		alt = !alt
	}
	return len(digits) > 0 && sum%10 == 0
}

// stripNonDigits removes every non-digit rune (spaces/dashes from a
// grouped card number) before Luhn-checking it.
func stripNonDigits(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// --- Deterministic pre-pass: keyword/lexicon (sağlık / finans / kimlik) ---
//
// Every entry is pre-folded (textnorm.Fold - Turkish dotted/dotless-I +
// Unicode lowercase, the SAME folding kahyad/internal/search's trigram
// index uses) so matching against Fold(text) is exact-substring, never a
// second ad-hoc casefold. Entries deliberately include BOTH the correct
// Turkish-diacritic spelling AND its common ASCII-folded variant (Fold
// does NOT strip ğ/ş/ç/ı - only the I-family - so "sağlık" and "saglik"
// are different folded strings and both need their own entry) - this is a
// lexicon, not a stemmer (tasks/README.md forbids hand-rolled morphology),
// so coverage is deliberately representative, not exhaustive.

var saglikKeywords = foldAll([]string{
	"tahlil sonuçları", "tahlil sonuclari", "tahlil sonucu",
	// Bare "tahlil" (lab test/analysis) ALSO stands alone so it substring-
	// matches inside a Turkish suffix compound the phrases above don't
	// cover, e.g. "tahlillerim" (my test results): "tahlillerim" contains
	// "tahlil" as a prefix, so no separate suffix table is needed here -
	// this is exactly the review fixture ("tahlil" in "tahlillerim").
	"tahlil",
	"sağlık", "saglik",
	// "sağlığ"/"saglig" are the Turkish consonant-softening (yumuşama)
	// stem of "sağlık"/"saglik": a vowel-initial possessive suffix turns
	// the word-final k into ğ (sağlık + -ım -> sağlığım, "my health"), so
	// plain "sağlık" is NEVER a substring of "sağlığım" - this stem entry
	// is what the review fixture ("sağlık" in "sağlığım") actually needs,
	// not the unsuffixed form above. Both the Turkish-diacritic and
	// ASCII-folded spellings are listed for the same reason every other
	// pair in this lexicon is (Fold does not touch ğ).
	"sağlığ", "saglig",
	"hastane", "reçete", "recete",
	"teşhis", "teshis", "diagnoz", "ilaç", "ilac",
	"ameliyat", "psikiyatr", "psikolog", "doktor raporu",
	"epikriz", "biyopsi", "tetkik sonucu", "kan tahlili",
	"hasta dosyası", "hasta dosyasi",
})

var finansKeywords = foldAll([]string{
	"kredi kartı ekstresi", "kredi karti ekstresi",
	"kredi kartı numarası", "kredi karti numarasi",
	"banka hesap", "hesap numarası", "hesap numarasi",
	"maaş bordrosu", "maas bordrosu", "vergi levhası", "vergi levhasi",
	"kredi notu", "banka ekstresi", "havale", "eft",
	"şifre", "sifre", "güvenlik kodu", "guvenlik kodu",
})

var kimlikKeywords = foldAll([]string{
	"tc kimlik", "t.c. kimlik", "tckn", "kimlik numarası", "kimlik numarasi",
	"pasaport numarası", "pasaport numarasi", "ehliyet numarası", "ehliyet numarasi",
	"doğum tarihi", "dogum tarihi", "anne kızlık soyadı", "anne kizlik soyadi",
	"nüfus cüzdanı", "nufus cuzdani",
})

func foldAll(words []string) []string {
	out := make([]string, len(words))
	for i, w := range words {
		out[i] = textnorm.Fold(w)
	}
	return out
}

// classifyDeterministic runs the regex/lexicon pre-pass. A hit is FINAL
// (task spec: "a hit is final: secret-lane, no model needed") - ok=false
// means nothing matched, so the caller falls through to Qwen.
func classifyDeterministic(text string) (Verdict, bool) {
	for _, m := range ibanCandidateRe.FindAllString(text, -1) {
		if isValidIBANStructure(m) {
			return Verdict{SecretLane: true, Category: CategoryFinans, Reason: "iban"}, true
		}
	}
	for _, m := range tcknCandidateRe.FindAllString(text, -1) {
		if isValidTCKN(m) {
			return Verdict{SecretLane: true, Category: CategoryKimlik, Reason: "tckn"}, true
		}
	}
	for _, m := range cardCandidateRe.FindAllString(text, -1) {
		digits := stripNonDigits(m)
		if len(digits) >= 13 && len(digits) <= 19 && luhnValid(digits) {
			return Verdict{SecretLane: true, Category: CategoryFinans, Reason: "card_number"}, true
		}
	}
	if cvvRe.MatchString(text) {
		return Verdict{SecretLane: true, Category: CategoryFinans, Reason: "cvv"}, true
	}

	folded := textnorm.Fold(text)
	for _, kw := range saglikKeywords {
		if strings.Contains(folded, kw) {
			return Verdict{SecretLane: true, Category: CategorySaglik, Reason: "keyword:" + kw}, true
		}
	}
	for _, kw := range finansKeywords {
		if strings.Contains(folded, kw) {
			return Verdict{SecretLane: true, Category: CategoryFinans, Reason: "keyword:" + kw}, true
		}
	}
	for _, kw := range kimlikKeywords {
		if strings.Contains(folded, kw) {
			return Verdict{SecretLane: true, Category: CategoryKimlik, Reason: "keyword:" + kw}, true
		}
	}
	return Verdict{}, false
}

// --- Qwen fallback classifier ---

// QwenClassifier classifies text via the local Qwen3-30B-A3B server (the
// ONLY thing this interface exists for - kahyad's ordering-invariant test
// and every deterministic-path test never need a real implementation of
// this at all). Implementations MUST fail closed themselves is NOT
// required - Classifier.Classify (below) already applies fail-closed
// uniformly to any error this returns; a QwenClassifier only needs to
// return a Verdict{secret_lane:false} for genuinely non-sensitive text and
// a non-nil error for anything it cannot confidently parse.
type QwenClassifier interface {
	Classify(ctx context.Context, text string) (Verdict, error)
}

// QwenClassifierFunc adapts a plain function to QwenClassifier (mirrors
// stdlib's http.HandlerFunc pattern) - used by this package's own tests to
// build hanging/erroring/canned-response fakes with no boilerplate type.
type QwenClassifierFunc func(ctx context.Context, text string) (Verdict, error)

func (f QwenClassifierFunc) Classify(ctx context.Context, text string) (Verdict, error) {
	return f(ctx, text)
}

// classifierSystemPrompt instructs Qwen to answer with STRICT JSON ONLY
// (task spec: "strict JSON {secret_lane: bool, category: finans|saglik|
// kimlik|none}"). English (this is a model instruction, not a user-facing
// string - CLAUDE.md's Turkish-user-facing-strings policy does not apply
// to prompts).
const classifierSystemPrompt = `You are a strict content classifier running entirely on the user's own machine. Given a piece of text, decide whether it contains Turkish financial (finans), health (sağlık), or identity (kimlik) information that must never leave this machine.

Respond with STRICT JSON ONLY, no other text, exactly this shape:
{"secret_lane": true|false, "category": "finans"|"saglik"|"kimlik"|"none"}

category must be "none" when secret_lane is false.`

// HTTPQwenClassifier is the production QwenClassifier: an OpenAI-compatible
// chat-completion call against the local Qwen3-30B-A3B server
// (kahyad/internal/mlx.Supervisor.BaseURL(), e.g.
// "http://127.0.0.1:8765/v1" - task spec step 4). Fully unit-testable
// against a fake httptest.Server standing in for mlx_lm.server's own
// OpenAI-compatible endpoint - no real model/MLX dependency at all; only
// the live, KAHYA_MLX_TESTS=1-gated test exercises this against the real
// server.
type HTTPQwenClassifier struct {
	// BaseURL is the OpenAI-compatible base ("http://127.0.0.1:<port>/v1"),
	// no trailing slash required.
	BaseURL string
	// Model is the model name field the chat-completion request body
	// carries - mlx_lm.server accepts (and mostly ignores/echoes) any
	// string here, since it always serves whatever single model it was
	// started with.
	Model  string
	Client *http.Client
}

// NewHTTPQwenClassifier constructs an HTTPQwenClassifier with a default
// *http.Client (no blanket timeout of its own - the caller's ctx is what
// bounds each call, matching kahyad's own anthproxy client conventions).
func NewHTTPQwenClassifier(baseURL, model string) *HTTPQwenClassifier {
	return &HTTPQwenClassifier{BaseURL: baseURL, Model: model, Client: &http.Client{}}
}

type classifyJSON struct {
	SecretLane bool   `json:"secret_lane"`
	Category   string `json:"category"`
}

// validCategory reports whether cat is one of the four values Qwen is
// instructed to return - anything else is treated exactly like invalid
// JSON (fail-closed, see Classify below).
func validCategory(cat string) bool {
	switch cat {
	case CategoryFinans, CategorySaglik, CategoryKimlik, CategoryNone:
		return true
	default:
		return false
	}
}

// Classify implements QwenClassifier against the real (or fake, in tests)
// local Qwen server's OpenAI-compatible /chat/completions endpoint.
func (c *HTTPQwenClassifier) Classify(ctx context.Context, text string) (Verdict, error) {
	content, err := postChatCompletion(ctx, c.Client, c.BaseURL, c.Model, []chatMessage{
		{Role: "system", Content: classifierSystemPrompt},
		{Role: "user", Content: text},
	}, 0, 64)
	if err != nil {
		return Verdict{}, fmt.Errorf("secretlane: qwen classify: %w", err)
	}

	var parsed classifyJSON
	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		// Non-JSON response - task spec: "Non-JSON or error => secret_lane:
		// true (FAIL-CLOSED)". Classify's caller (Classifier.Classify)
		// applies that; this method just reports the parse failure.
		return Verdict{}, fmt.Errorf("secretlane: qwen classify: non-JSON model response: %w", err)
	}
	if !validCategory(parsed.Category) {
		return Verdict{}, fmt.Errorf("secretlane: qwen classify: invalid category %q", parsed.Category)
	}
	category := parsed.Category
	if !parsed.SecretLane {
		category = CategoryNone
	}
	return Verdict{SecretLane: parsed.SecretLane, Category: category, Reason: "qwen"}, nil
}

// --- The full classifier ---

// DefaultBudget is the task spec's warm-model classification budget
// ("300ms budget after WARM load"). This is advisory - Classifier itself
// applies no internal timeout beyond ctx (see Classify's own doc comment
// for why); callers that know the model is warm may derive a
// context.WithTimeout(ctx, DefaultBudget) themselves before calling
// Classify.
const DefaultBudget = 300 * time.Millisecond

// Classifier is the full W3-08 ingest-time pre-classifier: deterministic
// pre-pass FIRST (final on a hit, Qwen never consulted), otherwise Qwen -
// with fail-closed (secret_lane:true) on ANY Qwen error, including "no
// QwenClassifier wired at all" (nil Qwen).
type Classifier struct {
	Qwen QwenClassifier
}

// NewClassifier constructs a Classifier. qwen may be nil - every text that
// does not match the deterministic pre-pass then fails closed
// unconditionally (there is no local model to consult), which is the
// correct posture before kahyad/internal/mlx's Qwen supervisor is wired in
// at all (e.g. very early boot, or MaxRestarts already exceeded).
func NewClassifier(qwen QwenClassifier) *Classifier {
	return &Classifier{Qwen: qwen}
}

// Classify runs the full ordering-invariant pipeline for one piece of
// content. It NEVER itself imposes a timeout beyond ctx: task spec's
// ordering invariant is explicit that "cold model means the classification
// WAITS for load or fails closed - it never skips ahead to cloud" - i.e.
// the only thing allowed to make this return early is the CALLER's own ctx
// deadline (which the caller derives according to whether the model is
// currently warm or cold - kahyad/internal/mlx.Supervisor.State() tells it
// which). A ctx cancellation reaching QwenClassifier.Classify surfaces as
// an ordinary error here, which - like every other Qwen error - fails
// closed (secret_lane:true), never silently "none".
func (c *Classifier) Classify(ctx context.Context, text string) (Verdict, error) {
	if v, ok := classifyDeterministic(text); ok {
		return v, nil
	}

	if c.Qwen == nil {
		return Verdict{SecretLane: true, Category: CategoryUnknown, Reason: "qwen_unavailable_fail_closed"},
			fmt.Errorf("secretlane: no local classifier model wired")
	}

	v, err := c.Qwen.Classify(ctx, text)
	if err != nil {
		return Verdict{SecretLane: true, Category: CategoryUnknown, Reason: "qwen_error_fail_closed"}, err
	}
	if !validCategory(v.Category) {
		return Verdict{SecretLane: true, Category: CategoryUnknown, Reason: "qwen_invalid_category_fail_closed"},
			fmt.Errorf("secretlane: qwen returned invalid category %q", v.Category)
	}
	return v, nil
}

// ClassifyDeterministic runs ONLY the regex/lexicon pre-pass (classifier.
// go's classifyDeterministic) - no Qwen fallback, and critically, NO
// fail-closed-on-unavailable-model behavior: text that matches nothing
// here is reported as NOT secret-lane (Verdict{SecretLane: false,
// Category: CategoryNone}), never an error. This is a DIFFERENT, narrower
// contract than Classifier.Classify (which fails closed to secret_lane:
// true whenever Qwen is unavailable or errors) - it exists for ingestion
// points that must never take a live-Qwen dependency merely to decide
// "is this ordinary content", the way an arbitrary chat prompt does
// (kahyad/internal/server's POST /v1/task handler uses exactly this: the
// task spec's own ingestion-point list names memory_write content, fs
// reads flagged for model consumption, and (W4-03) mail/web Reader input -
// it does NOT name the raw chat prompt itself, so requiring a running
// Qwen server just to have an ordinary cloud-routed conversation would be
// a scope overreach, not a spec requirement). The deterministic pre-pass
// ALONE already needs no model at all and is exactly as strong a
// guarantee for IBAN/TCKN/card-number/keyword hits either way - the only
// difference is what happens on a NON-match: ClassifyDeterministic says
// "not secret" (safe default for an ingestion point with no fail-closed
// mandate of its own), Classify says "fail closed" (the mandate for the
// three NAMED ingestion points, once W4-03/W4-08 wire the full
// Qwen-backed Classifier into them).
func ClassifyDeterministic(text string) Verdict {
	if v, ok := classifyDeterministic(text); ok {
		return v
	}
	return Verdict{SecretLane: false, Category: CategoryNone, Reason: "deterministic_no_match"}
}
