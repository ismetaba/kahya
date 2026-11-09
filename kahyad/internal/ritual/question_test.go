package ritual

import (
	"context"
	"sort"
	"strings"
	"testing"

	"kahya/kahyad/internal/memlane"
	"kahya/kahyad/internal/reader"
	"kahya/kahyad/internal/store/sqlcgen"
)

func triple(subject, predicate, object string) sqlcgen.Fact {
	return sqlcgen.Fact{Subject: subject, Predicate: predicate, Object: object}
}

// TestQuestionTextRendersEveryLabelledPredicate (FIX-FINAL TUX-1): every
// predicate the extraction prompts name renders through its Turkish label,
// byte-exact. A changed label fails here before it reaches a user.
func TestQuestionTextRendersEveryLabelledPredicate(t *testing.T) {
	want := map[string]string{
		"works_at":     "Emre — çalıştığı yer: Acme",
		"role_at":      "Emre — görevi: Acme",
		"lives_in":     "Emre — yaşadığı yer: Acme",
		"located_in":   "Emre — bulunduğu yer: Acme",
		"member_of":    "Emre — üyesi olduğu grup: Acme",
		"owns":         "Emre — sahip olduğu şey: Acme",
		"founded":      "Emre — kurduğu kuruluş: Acme",
		"parent_of":    "Emre — ebeveyni olduğu kişi: Acme",
		"child_of":     "Emre — çocuğu olduğu kişi: Acme",
		"spouse_of":    "Emre — eşi: Acme",
		"friend_of":    "Emre — arkadaşı: Acme",
		"colleague_of": "Emre — iş arkadaşı: Acme",
		"manages":      "Emre — yönettiği kişi veya ekip: Acme",
		"reports_to":   "Emre — bağlı olduğu yönetici: Acme",
		"prefers":      "Emre — tercih ettiği şey: Acme",
		"dislikes":     "Emre — sevmediği şey: Acme",
		"decided":      "Emre — verdiği karar: Acme",
		"promised":     "Emre — verdiği söz: Acme",
		"met_with":     "Emre — görüştüğü kişi: Acme",
		"contacted":    "Emre — iletişime geçtiği kişi: Acme",
		"attended":     "Emre — katıldığı etkinlik: Acme",
		"owes":         "Emre — borcu: Acme",
		"paid":         "Emre — yaptığı ödeme: Acme",
		"received":     "Emre — aldığı şey: Acme",
		"scheduled":    "Emre — planladığı şey: Acme",
		"phone_of":     "Emre — telefon: Acme",
		"email_of":     "Emre — e-posta: Acme",
		"birthday_of":  "Emre — doğum günü: Acme",
		"relates_to":   "Emre — ilgili olduğu konu: Acme",
	}
	if len(want) != len(predicateLabels) {
		t.Fatalf("pinned %d renderings, predicateLabels has %d entries", len(want), len(predicateLabels))
	}
	for predicate, text := range want {
		if got := questionText(triple("Emre", predicate, "Acme")); got != text {
			t.Errorf("questionText(%s) = %q, want %q", predicate, got, text)
		}
	}
}

// TestPredicateLabelsCoverTheExtractorVocabulary (FIX-FINAL TUX-1): the
// label table and the extraction prompts' predicate list are the same set,
// so a predicate added to the prompts cannot reach a user unlabelled.
func TestPredicateLabelsCoverTheExtractorVocabulary(t *testing.T) {
	vocabulary := reader.FactPredicates()
	sort.Strings(vocabulary)
	labelled := make([]string, 0, len(predicateLabels))
	for predicate, label := range predicateLabels {
		labelled = append(labelled, predicate)
		if strings.TrimSpace(label) == "" || strings.ContainsAny(label, "_%") {
			t.Errorf("label for %s = %q: want plain Turkish text", predicate, label)
		}
	}
	sort.Strings(labelled)
	if strings.Join(vocabulary, ",") != strings.Join(labelled, ",") {
		t.Fatalf("extractor vocabulary and label table differ:\n prompts: %v\n labels:  %v", vocabulary, labelled)
	}
}

// TestQuestionTextFallbacks (FIX-FINAL TUX-1): an English relation with no
// label is shown as words inside a Turkish frame, never as a snake_case
// identifier; a user's own free-text predicate keeps the plain join.
func TestQuestionTextFallbacks(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    sqlcgen.Fact
		want string
	}{
		{"other prefix", triple("Ayşe", "other:plays_instrument", "keman"), "Ayşe — keman (ilişki: plays instrument)"},
		{"other single word", triple("Ayşe", "other:plays", "keman"), "Ayşe — keman (ilişki: plays)"},
		{"unlisted snake_case", triple("Emre", "studied_at", "ODTÜ"), "Emre — ODTÜ (ilişki: studied at)"},
		{"repeated underscores", triple("Emre", "other:went__to_", "Ankara"), "Emre — Ankara (ilişki: went to)"},
		{"bare other prefix", triple("Emre", "other:", "Ankara"), "Emre Ankara"},
		{"underscores only", triple("Emre", "__", "Ankara"), "Emre Ankara"},
		{"free Turkish predicate", triple("Emre", "sever", "kahve"), "Emre sever kahve"},
		{"free Turkish phrase", triple("Emre", "pazartesileri gider", "yüzme kursuna"), "Emre pazartesileri gider yüzme kursuna"},
		{"padded fields", triple(" Emre ", " works_at ", " Acme "), "Emre — çalıştığı yer: Acme"},
		{"percent in a name", triple("%s Emre", "works_at", "%d Acme"), "%s Emre — çalıştığı yer: %d Acme"},
		{"empty object", triple("Emre", "works_at", " "), "Emre çalıştığı yer"},
		{"empty subject", triple("", "other:plays_instrument", "keman"), "plays instrument keman"},
		{"inner padding", triple("Emre ", " sever", "kahve"), "Emre sever kahve"},
	} {
		if got := questionText(tc.f); got != tc.want {
			t.Errorf("%s: questionText = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestQuestionTextNeverShowsASnakeCasePredicate (FIX-FINAL TUX-1): no
// predicate the extractor's validator accepts reaches the user with an
// underscore or the "other:" marker.
func TestQuestionTextNeverShowsASnakeCasePredicate(t *testing.T) {
	predicates := reader.FactPredicates()
	for _, p := range reader.FactPredicates() {
		predicates = append(predicates, "other:"+p)
	}
	predicates = append(predicates, "other:plays_instrument", "has_a_long_relation_name")
	for _, p := range predicates {
		got := questionText(triple("Ayşe", p, "keman"))
		if strings.Contains(got, "_") || strings.Contains(got, "other:") {
			t.Errorf("questionText(%s) = %q: shows a raw identifier", p, got)
		}
		if !strings.HasPrefix(got, "Ayşe — ") || !strings.Contains(got, "keman") {
			t.Errorf("questionText(%s) = %q: want the subject and the object in a Turkish frame", p, got)
		}
	}
}

// TestNoPredicateLabelIsSecretLaneOnItsOwn (FIX-FINAL TUX-1): a label never
// trips the deterministic secret-lane pre-pass by itself - otherwise every
// fact of that predicate would silently drop out of the ritual.
func TestNoPredicateLabelIsSecretLaneOnItsOwn(t *testing.T) {
	for _, p := range reader.FactPredicates() {
		if text := questionText(triple("Emre", p, "Acme")); memlane.ContentIsSecret(text) {
			t.Errorf("questionText(%s) = %q is secret-lane content on its own", p, text)
		}
	}
}

// TestRunAsksExtractorFactsInTurkish (FIX-FINAL TUX-1): the two facts the
// reviewer's probe seeded (extractor-shaped: agent_derived, English
// predicates) are delivered - and recorded in eval_labels - as Turkish
// question bodies. Before the fix the bodies were "Emre works_at Acme" and
// "Ayşe other:plays_instrument keman".
func TestRunAsksExtractorFactsInTurkish(t *testing.T) {
	st := newTestStore(t)
	ep := seedEpisode(t, st, "notlar/gunluk.md")
	for _, f := range []sqlcgen.Fact{
		seedFact(t, st, "Emre", "works_at", "Acme", "agent_derived", -0.2, false),
		seedFact(t, st, "Ayşe", "other:plays_instrument", "keman", "agent_derived", -0.2, false),
	} {
		seedClassifyingEvidence(t, st, f.ID, ep)
	}
	delivery := &fakeDelivery{sendResult: true}
	engine := newTestEngine(t, st, delivery)

	const traceID = "run-trace-tux1-0000000000000000001"
	asked, err := engine.Run(context.Background(), traceID)
	if err != nil || asked != 2 {
		t.Fatalf("Run() = %d, %v; want 2 questions", asked, err)
	}
	want := []string{"Emre — çalıştığı yer: Acme", "Ayşe — keman (ilişki: plays instrument)"}
	var sent []string
	for _, c := range delivery.calls {
		sent = append(sent, c.FactText)
	}
	if strings.Join(sent, "\n") != strings.Join(want, "\n") {
		t.Fatalf("delivered question bodies = %q, want %q", sent, want)
	}
	rows, err := st.Queries.ListEvalLabelsByTrace(context.Background(), traceID)
	if err != nil || len(rows) != 2 {
		t.Fatalf("eval_labels rows = %d (err %v), want 2", len(rows), err)
	}
	for i, row := range rows {
		if row.QuestionText != want[i] {
			t.Errorf("eval_labels[%d].question_text = %q, want %q", i, row.QuestionText, want[i])
		}
	}
}

// TestRunClassifiesTheRenderedQuestionText (FIX-FINAL TUX-1, Safety #5):
// the string that would be sent is the string that is classified. The
// stored triple "Emre other:tc_kimlik 123" passes the deterministic
// pre-pass (the lexicon term is "tc kimlik", with a space); its rendering
// "Emre — 123 (ilişki: tc kimlik)" does not - so the fact is never sent,
// opens no eval_labels row and is ledgered content_deterministic.
func TestRunClassifiesTheRenderedQuestionText(t *testing.T) {
	st := newTestStore(t)
	ep := seedEpisode(t, st, "notlar/genel.md")
	f := seedFact(t, st, "Emre", "other:tc_kimlik", "123", "agent_derived", -0.2, false)
	seedClassifyingEvidence(t, st, f.ID, ep)
	if memlane.ContentIsSecret(memlane.FactText(f)) {
		t.Fatalf("fixture: the stored triple %q must pass the pre-pass on its own", memlane.FactText(f))
	}
	if !memlane.ContentIsSecret(questionText(f)) {
		t.Fatalf("fixture: the rendering %q must be secret-lane content", questionText(f))
	}
	delivery := &fakeDelivery{sendResult: true}
	engine := newTestEngine(t, st, delivery)
	engine.sampler.SetLedger(st)

	const traceID = "run-trace-tux1-0000000000000000002"
	asked, err := engine.Run(context.Background(), traceID)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if asked != 0 || len(delivery.calls) != 0 {
		t.Fatalf("Run() asked = %d, deliveries = %+v; want 0 and none", asked, delivery.calls)
	}
	rows, err := st.Queries.ListEvalLabelsByTrace(context.Background(), traceID)
	if err != nil || len(rows) != 0 {
		t.Fatalf("eval_labels rows = %d (err %v), want 0", len(rows), err)
	}
	ex := excludedRows(t, st)
	if len(ex) != 1 || ex[0].reason != ExcludeReasonContentDeterministic || ex[0].traceID != traceID || ex[0].factID != f.ID {
		t.Fatalf("exclusion rows = %+v, want one {fact %d, %s, %s}", ex, f.ID, ExcludeReasonContentDeterministic, traceID)
	}
}
