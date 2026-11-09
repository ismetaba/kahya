// question.go renders a fact as the ritual question body the user reads on
// Telegram (FIX-FINAL TUX-1). The stored predicate stays what the writer
// produced - the G13 extractor writes English snake_case relations by
// design (reader.FactPredicates, or "other:<snake_case>") - and only the
// rendering is Turkish: the user is asked "Emre — çalıştığı yer: Acme",
// never "Emre works_at Acme".
package ritual

import (
	"fmt"
	"strings"

	"kahya/kahyad/internal/store/sqlcgen"
)

// questionLabelFmt is the rendering of a fact whose predicate has a Turkish
// label: "<subject> — <label>: <object>". A label is a noun phrase that
// needs no suffix on either name, so no vowel harmony has to be guessed for
// a subject or object this package has never seen.
const questionLabelFmt = "%s — %s: %s"

// questionRelationFmt is the fallback for an English predicate with no
// label ("other:<snake_case>", or any snake_case relation outside the
// table): "<subject> — <object> (ilişki: <predicate words>)", the prefix
// dropped and every underscore turned into a space.
const questionRelationFmt = "%s — %s (ilişki: %s)"

// otherPredicatePrefix marks an extractor predicate outside its own list.
const otherPredicatePrefix = "other:"

// predicateLabels maps every predicate the extraction prompts name
// (reader.FactPredicates; TestPredicateLabelsCoverTheExtractorVocabulary
// keeps the two in step) to its Turkish label. Each label reads with the
// SUBJECT as the owner: works_at is "<subject> — çalıştığı yer: <object>".
// The three *_of contact predicates get a bare noun, which reads the same
// whichever side the extractor put the person on.
var predicateLabels = map[string]string{
	"works_at":     "çalıştığı yer",
	"role_at":      "görevi",
	"lives_in":     "yaşadığı yer",
	"located_in":   "bulunduğu yer",
	"member_of":    "üyesi olduğu grup",
	"owns":         "sahip olduğu şey",
	"founded":      "kurduğu kuruluş",
	"parent_of":    "ebeveyni olduğu kişi",
	"child_of":     "çocuğu olduğu kişi",
	"spouse_of":    "eşi",
	"friend_of":    "arkadaşı",
	"colleague_of": "iş arkadaşı",
	"manages":      "yönettiği kişi veya ekip",
	"reports_to":   "bağlı olduğu yönetici",
	"prefers":      "tercih ettiği şey",
	"dislikes":     "sevmediği şey",
	"decided":      "verdiği karar",
	"promised":     "verdiği söz",
	"met_with":     "görüştüğü kişi",
	"contacted":    "iletişime geçtiği kişi",
	"attended":     "katıldığı etkinlik",
	"owes":         "borcu",
	"paid":         "yaptığı ödeme",
	"received":     "aldığı şey",
	"scheduled":    "planladığı şey",
	"phone_of":     "telefon",
	"email_of":     "e-posta",
	"birthday_of":  "doğum günü",
	"relates_to":   "ilgili olduğu konu",
}

// questionText renders fact f as the ritual question body ("Bu doğru
// mu?\n\n<question text>"'s own second half):
//
//   - a predicate in predicateLabels: questionLabelFmt with its label;
//   - any other English relation - "other:<snake_case>", or a predicate
//     carrying an underscore: questionRelationFmt with the predicate's
//     words, so a raw snake_case identifier is never shown;
//   - anything else (a user's own free Turkish predicate such as "sever"):
//     the plain "subject predicate object" join, which is memlane.FactText
//     for a fact whose fields carry no padding.
//
// A fact with an empty subject or object has nothing to hang a frame on:
// its non-empty parts are joined by single spaces, the predicate still as
// its label or its words. The sampler classifies exactly this string
// (Sampler.classify), next to the stored triple, so the sent text is
// always text the secret-lane classification has seen (FIX-P-17).
func questionText(f sqlcgen.Fact) string {
	subject := strings.TrimSpace(f.Subject)
	predicate := strings.TrimSpace(f.Predicate)
	object := strings.TrimSpace(f.Object)

	label, labelled := predicateLabels[predicate]
	english := !labelled && (strings.HasPrefix(predicate, otherPredicatePrefix) || strings.Contains(predicate, "_"))
	switch {
	case labelled:
		predicate = label
	case english:
		predicate = strings.Join(strings.Fields(strings.ReplaceAll(strings.TrimPrefix(predicate, otherPredicatePrefix), "_", " ")), " ")
	}

	if subject != "" && object != "" && predicate != "" {
		if labelled {
			return fmt.Sprintf(questionLabelFmt, subject, predicate, object)
		}
		if english {
			return fmt.Sprintf(questionRelationFmt, subject, object, predicate)
		}
	}
	parts := make([]string, 0, 3)
	for _, part := range []string{subject, predicate, object} {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, " ")
}
