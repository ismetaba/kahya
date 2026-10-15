// schemas.go defines the W4-03 Reader output schemas: mail_summary_v1 and
// webpage_extract_v1 (task spec step 5). Every validator is a hand-
// written Go function against stdlib types ONLY - no JSON-Schema
// library (HANDOFF §4/§9 name no such dependency, and CLAUDE.md forbids
// inventing one). Every free-text field runs the W3-06 sanitizer
// (kahyad/internal/canon.Normalize), THEN is rejected outright (never
// silently truncated/stripped) on any control/bidi/zero-width code point
// or an over-length result - task spec step 5, verbatim: "Validation
// failure => Reader job fails closed (no partial output)".
package reader

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"kahya/kahyad/internal/canon"
	"kahya/kahyad/internal/factengine"
)

// JobType values - the registered schema names task spec step 5 fixes.
// JobTypeFactExtract (G13 MEM-3) is the knowledge extractor's schema: the
// (subject, predicate, object) facts and entities one or more memory
// chunks state, each fact citing a verbatim span of a chunk it was shown.
const (
	JobTypeMailSummary    = "mail_summary_v1"
	JobTypeWebpageExtract = "webpage_extract_v1"
	JobTypeFactExtract    = "fact_extract_v1"
)

// Field length ceilings (task spec step 5, verbatim char counts - RUNE
// counts in this implementation, since Turkish text has plenty of
// multi-byte, single-rune characters a byte-length cap would wrongly
// penalize).
const (
	mailFromDisplayMaxLen = 120
	mailSubjectMaxLen     = 200
	mailSummaryMaxLen     = 500
	webpageTitleMaxLen    = 200
	webpageKeyPointMaxLen = 300
	webpageKeyPointsMax   = 10
)

// MailSummaryV1 is the registered Go struct for JobTypeMailSummary.
type MailSummaryV1 struct {
	FromDisplay string   `json:"from_display"`
	Subject     string   `json:"subject"`
	Summary     string   `json:"summary"`
	Dates       []string `json:"dates"`
	Amounts     []string `json:"amounts"`
}

// WebpageExtractV1 is the registered Go struct for JobTypeWebpageExtract.
type WebpageExtractV1 struct {
	Title     string   `json:"title"`
	KeyPoints []string `json:"key_points"`
}

// amountRe is the task spec's own pattern, verbatim: `^[0-9.,]+ ?(TL|USD|EUR)$`.
var amountRe = regexp.MustCompile(`^[0-9.,]+ ?(TL|USD|EUR)$`)

// hasDisallowedControlRune reports whether s contains a newline/tab or any
// Unicode control character (unicode.IsControl - the Latin-1 Cc range:
// NUL, ESC, a bare CR, ...). canon.Normalize's own HasControlFlags already
// catches the Trojan-Source-style Cf category (bidi/zero-width/other
// format controls); this catches the more mundane ASCII-control-character
// class the task spec's char-class validators must ALSO reject (step 5:
// "control chars, bidi, zero-width, or over-length") - every Reader
// schema field is a single-line value (a subject, a summary, a title, one
// key point), so a legitimate extraction never needs an embedded newline
// or tab either.
func hasDisallowedControlRune(s string) bool {
	for _, r := range s {
		if r == '\n' || r == '\t' || unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// sanitizeField runs the W3-06 sanitizer (canon.Normalize) then rejects -
// rather than silently stripping - any control/bidi/zero-width code point
// or an over-length result. maxLen is a rune count.
func sanitizeField(fieldName, raw string, maxLen int) (string, error) {
	res := canon.Normalize(raw)
	if res.HasControlFlags() {
		return "", fmt.Errorf("%s: contains a bidi/zero-width/format control code point", fieldName)
	}
	if hasDisallowedControlRune(res.Canonical) {
		return "", fmt.Errorf("%s: contains a disallowed control character", fieldName)
	}
	if n := utf8.RuneCountInString(res.Canonical); n > maxLen {
		return "", fmt.Errorf("%s: exceeds max length %d (got %d)", fieldName, maxLen, n)
	}
	return res.Canonical, nil
}

// ValidateMailSummaryV1 sanitizes and validates every field of v,
// returning a NEW, fully validated MailSummaryV1 - never a partially
// valid one (task spec step 5: Reader job fails closed with NO partial
// output on the first failure).
func ValidateMailSummaryV1(v MailSummaryV1) (MailSummaryV1, error) {
	from, err := sanitizeField("from_display", v.FromDisplay, mailFromDisplayMaxLen)
	if err != nil {
		return MailSummaryV1{}, err
	}
	subject, err := sanitizeField("subject", v.Subject, mailSubjectMaxLen)
	if err != nil {
		return MailSummaryV1{}, err
	}
	summary, err := sanitizeField("summary", v.Summary, mailSummaryMaxLen)
	if err != nil {
		return MailSummaryV1{}, err
	}

	dates := make([]string, 0, len(v.Dates))
	for i, d := range v.Dates {
		if _, perr := time.Parse(time.RFC3339, d); perr != nil {
			return MailSummaryV1{}, fmt.Errorf("dates[%d]: %q is not RFC3339: %w", i, d, perr)
		}
		dates = append(dates, d)
	}

	amounts := make([]string, 0, len(v.Amounts))
	for i, a := range v.Amounts {
		if !amountRe.MatchString(a) {
			return MailSummaryV1{}, fmt.Errorf("amounts[%d]: %q does not match ^[0-9.,]+ ?(TL|USD|EUR)$", i, a)
		}
		amounts = append(amounts, a)
	}

	return MailSummaryV1{
		FromDisplay: from, Subject: subject, Summary: summary,
		Dates: dates, Amounts: amounts,
	}, nil
}

// ValidateWebpageExtractV1 sanitizes and validates every field of v,
// returning a NEW, fully validated WebpageExtractV1 - same no-partial-
// output contract as ValidateMailSummaryV1.
func ValidateWebpageExtractV1(v WebpageExtractV1) (WebpageExtractV1, error) {
	title, err := sanitizeField("title", v.Title, webpageTitleMaxLen)
	if err != nil {
		return WebpageExtractV1{}, err
	}
	if len(v.KeyPoints) > webpageKeyPointsMax {
		return WebpageExtractV1{}, fmt.Errorf("key_points: %d entries exceeds max %d", len(v.KeyPoints), webpageKeyPointsMax)
	}

	points := make([]string, 0, len(v.KeyPoints))
	for i, p := range v.KeyPoints {
		sanitized, perr := sanitizeField(fmt.Sprintf("key_points[%d]", i), p, webpageKeyPointMaxLen)
		if perr != nil {
			return WebpageExtractV1{}, perr
		}
		points = append(points, sanitized)
	}

	return WebpageExtractV1{Title: title, KeyPoints: points}, nil
}

// fact_extract_v1 field ceilings (rune counts, the same posture as the
// mail/webpage schemas above). extractQuoteMaxLen equals
// factengine.MaxQuoteRunes (pinned by TestFactExtractQuoteCeilingMatchesFactEngine):
// a quote this validator accepts is never refused by WriteFact.
const (
	extractEntityNameMaxLen  = 120
	extractEntityAliasMaxLen = 120
	extractEntityAliasesMax  = 5
	extractEntitiesMax       = 40
	extractFactFieldMaxLen   = 200 // subject / object
	extractQuoteMaxLen       = 300
	extractFactsMax          = 60
	// extractQuotelessImportanceMax caps the importance of a fact that cites
	// no verbatim span: an unsupported claim is never "important".
	extractQuotelessImportanceMax = 0.5
)

// ExtractChunk is one memory chunk handed to the extractor: its chunks.id
// and raw text. A fact may only cite an id the caller sent.
type ExtractChunk struct {
	ID   int64
	Text string
}

// ExtractEntityV1 is one entity the extractor names. Name is the canonical
// surface form (Turkish case suffix dropped by the model), Kind one of
// factengine's entity kinds (anything else folds to "other").
type ExtractEntityV1 struct {
	Name    string   `json:"name"`
	Kind    string   `json:"kind"`
	Aliases []string `json:"aliases"`
}

// ExtractFactV1 is one extracted (subject, predicate, object) triple. There
// is deliberately NO trust-level / source-tier field: the tier is the call
// site's provenance (kahyad/internal/extract), never the model's say-so
// (HANDOFF §5 memory #1). Unknown JSON keys (e.g. a "source_tier" an
// injected text asked the model to add) are dropped by the decoder.
type ExtractFactV1 struct {
	ChunkID       int64   `json:"chunk_id"`
	Subject       string  `json:"subject"`
	Predicate     string  `json:"predicate"`
	Object        string  `json:"object"`
	Evidentiality string  `json:"evidentiality"`
	Quote         string  `json:"quote"`
	ValidFrom     string  `json:"valid_from"`
	ValidTo       string  `json:"valid_to"`
	Importance    float64 `json:"importance"`
	Retraction    bool    `json:"retraction"`
}

// FactExtractV1 is the registered Go struct for JobTypeFactExtract.
type FactExtractV1 struct {
	Entities []ExtractEntityV1 `json:"entities"`
	Facts    []ExtractFactV1   `json:"facts"`
}

// predicateRe is fact_extract_v1's predicate shape: a snake_case English
// relation, or "other:<snake_case>" for one outside the prompt's list.
var predicateRe = regexp.MustCompile(`^(other:)?[a-z][a-z_]{1,39}$`)

// ValidateFactExtractV1 sanitizes and validates every field of v and
// returns a NEW, fully validated FactExtractV1 - or the first failure, with
// no partial output (the same contract as the mail/webpage validators).
// chunkText is the set of chunks the caller SENT, by id: a fact citing any
// other id, or whose quote is not a verbatim span of that chunk, fails the
// whole job (the model cannot cite evidence it was not shown - HANDOFF §5
// memory #4, "ham kanıttan"). Rules:
//
//   - entities <= 40; name non-empty, <= 120 runes; kind folded through
//     factengine.NormalizeEntityKind (an unknown kind becomes "other", not
//     an error); <= 5 aliases of <= 120 runes, blank ones and repeats of the
//     name dropped;
//   - facts <= 60; subject/object non-empty, <= 200 runes; predicate
//     matches predicateRe; evidentiality via factengine.NormalizeEvidentiality
//     ("" -> inferred, anything else outside the enum is an error);
//   - quote <= 300 runes, no control/bidi/zero-width code point other than a
//     line break or tab, and a byte-exact substring of the cited chunk (an
//     NFC-equal match is accepted too, stored in its NFC form); an empty
//     quote caps importance at 0.5;
//   - valid_from/valid_to empty or RFC3339, from <= to; importance in [0,1].
func ValidateFactExtractV1(v FactExtractV1, chunkText map[int64]string) (FactExtractV1, error) {
	if len(v.Entities) > extractEntitiesMax {
		return FactExtractV1{}, fmt.Errorf("entities: %d entries exceeds max %d", len(v.Entities), extractEntitiesMax)
	}
	if len(v.Facts) > extractFactsMax {
		return FactExtractV1{}, fmt.Errorf("facts: %d entries exceeds max %d", len(v.Facts), extractFactsMax)
	}

	entities := make([]ExtractEntityV1, 0, len(v.Entities))
	for i, e := range v.Entities {
		name, err := sanitizeRequired(fmt.Sprintf("entities[%d].name", i), e.Name, extractEntityNameMaxLen)
		if err != nil {
			return FactExtractV1{}, err
		}
		if len(e.Aliases) > extractEntityAliasesMax {
			return FactExtractV1{}, fmt.Errorf("entities[%d].aliases: %d entries exceeds max %d", i, len(e.Aliases), extractEntityAliasesMax)
		}
		aliases := make([]string, 0, len(e.Aliases))
		seen := map[string]bool{name: true}
		for j, a := range e.Aliases {
			alias, err := sanitizeField(fmt.Sprintf("entities[%d].aliases[%d]", i, j), strings.TrimSpace(a), extractEntityAliasMaxLen)
			if err != nil {
				return FactExtractV1{}, err
			}
			if alias == "" || seen[alias] {
				continue
			}
			seen[alias] = true
			aliases = append(aliases, alias)
		}
		entities = append(entities, ExtractEntityV1{Name: name, Kind: factengine.NormalizeEntityKind(e.Kind), Aliases: aliases})
	}

	facts := make([]ExtractFactV1, 0, len(v.Facts))
	for i, f := range v.Facts {
		field := func(name string) string { return fmt.Sprintf("facts[%d].%s", i, name) }
		chunk, ok := chunkText[f.ChunkID]
		if !ok {
			return FactExtractV1{}, fmt.Errorf("%s: %d is not one of the chunks sent", field("chunk_id"), f.ChunkID)
		}
		subject, err := sanitizeRequired(field("subject"), f.Subject, extractFactFieldMaxLen)
		if err != nil {
			return FactExtractV1{}, err
		}
		object, err := sanitizeRequired(field("object"), f.Object, extractFactFieldMaxLen)
		if err != nil {
			return FactExtractV1{}, err
		}
		predicate := strings.TrimSpace(f.Predicate)
		if !predicateRe.MatchString(predicate) {
			return FactExtractV1{}, fmt.Errorf("%s: %q does not match %s", field("predicate"), f.Predicate, predicateRe)
		}
		evidentiality, err := factengine.NormalizeEvidentiality(f.Evidentiality)
		if err != nil {
			return FactExtractV1{}, fmt.Errorf("%s: %w", field("evidentiality"), err)
		}
		quote, err := validateQuote(field("quote"), f.Quote, chunk)
		if err != nil {
			return FactExtractV1{}, err
		}
		from, err := parseOptionalRFC3339(field("valid_from"), f.ValidFrom)
		if err != nil {
			return FactExtractV1{}, err
		}
		to, err := parseOptionalRFC3339(field("valid_to"), f.ValidTo)
		if err != nil {
			return FactExtractV1{}, err
		}
		if !from.IsZero() && !to.IsZero() && from.After(to) {
			return FactExtractV1{}, fmt.Errorf("%s: valid_from %q is after valid_to %q", field("valid_from"), f.ValidFrom, f.ValidTo)
		}
		if math.IsNaN(f.Importance) || f.Importance < 0 || f.Importance > 1 {
			return FactExtractV1{}, fmt.Errorf("%s: %v is outside [0,1]", field("importance"), f.Importance)
		}
		importance := f.Importance
		if quote == "" && importance > extractQuotelessImportanceMax {
			importance = extractQuotelessImportanceMax
		}
		facts = append(facts, ExtractFactV1{
			ChunkID: f.ChunkID, Subject: subject, Predicate: predicate, Object: object,
			Evidentiality: evidentiality, Quote: quote,
			ValidFrom: strings.TrimSpace(f.ValidFrom), ValidTo: strings.TrimSpace(f.ValidTo),
			Importance: importance, Retraction: f.Retraction,
		})
	}
	return FactExtractV1{Entities: entities, Facts: facts}, nil
}

// sanitizeRequired is sanitizeField on the trimmed value, which must not be
// empty.
func sanitizeRequired(fieldName, raw string, maxLen int) (string, error) {
	s, err := sanitizeField(fieldName, strings.TrimSpace(raw), maxLen)
	if err != nil {
		return "", err
	}
	if s == "" {
		return "", fmt.Errorf("%s: must not be empty", fieldName)
	}
	return s, nil
}

// validateQuote checks a fact's quote (empty is allowed): no bidi/zero-
// width/format control and no control character other than a line break or
// tab (a verbatim span may cross a line, unlike every other Reader field),
// at most extractQuoteMaxLen runes, and a substring of the cited chunk -
// byte-exact (returned as is), or NFC-equal (returned in NFC form).
func validateQuote(fieldName, raw, chunk string) (string, error) {
	if raw == "" {
		return "", nil
	}
	res := canon.Normalize(raw)
	if res.HasControlFlags() {
		return "", fmt.Errorf("%s: contains a bidi/zero-width/format control code point", fieldName)
	}
	for _, r := range raw {
		if r != '\n' && r != '\t' && unicode.IsControl(r) {
			return "", fmt.Errorf("%s: contains a disallowed control character", fieldName)
		}
	}
	if n := utf8.RuneCountInString(raw); n > extractQuoteMaxLen {
		return "", fmt.Errorf("%s: exceeds max length %d (got %d)", fieldName, extractQuoteMaxLen, n)
	}
	if strings.TrimSpace(raw) == "" {
		return "", fmt.Errorf("%s: blank quote", fieldName)
	}
	if strings.Contains(chunk, raw) {
		return raw, nil
	}
	if nfc := norm.NFC.String(raw); strings.Contains(norm.NFC.String(chunk), nfc) {
		return nfc, nil
	}
	return "", fmt.Errorf("%s: not a verbatim span of the cited chunk", fieldName)
}

// parseOptionalRFC3339 parses s ("" = unset) as RFC3339.
func parseOptionalRFC3339(fieldName, s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s: %q is not RFC3339: %w", fieldName, s, err)
	}
	return t, nil
}

// RenderExtractInput renders chunks as the extractor's input: one
// "### CHUNK <id>" header line per chunk, then its text and a blank line.
// It is the EXACT byte string Runner.RunExtract classifies and then hands
// to the model; a caller guarding a cloud call (kahyad/internal/extract's
// GuardCloudExtraction step) classifies this same string, so the bytes
// classified are the bytes sent.
func RenderExtractInput(chunks []ExtractChunk) string {
	var b strings.Builder
	for _, c := range chunks {
		fmt.Fprintf(&b, "### CHUNK %d\n%s\n\n", c.ID, c.Text)
	}
	return b.String()
}
