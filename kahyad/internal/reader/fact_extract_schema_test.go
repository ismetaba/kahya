package reader

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"

	"kahya/kahyad/internal/factengine"
)

// G13 MEM-3 phase 1: the fact_extract_v1 schema validator.

const (
	factChunkA = "Emre bugün Acme'de işe başladığını söyledi.\nYeni ekibi çok sevmiş."
	factChunkB = "Toplantı Kadıköy ofisinde yapıldı."
)

func factChunks() map[int64]string {
	return map[int64]string{11: factChunkA, 12: factChunkB}
}

func wellFormedFactExtract() FactExtractV1 {
	return FactExtractV1{
		Entities: []ExtractEntityV1{
			{Name: "Emre", Kind: "person", Aliases: []string{"Emre Bey"}},
			{Name: "Acme", Kind: "org"},
		},
		Facts: []ExtractFactV1{{
			ChunkID: 11, Subject: "Emre", Predicate: "works_at", Object: "Acme",
			Evidentiality: "reported", Quote: "Emre bugün Acme'de işe başladığını söyledi.",
			ValidFrom: "2026-10-01T00:00:00Z", Importance: 0.8,
		}},
	}
}

func TestValidateFactExtractV1AcceptsWellFormed(t *testing.T) {
	got, err := ValidateFactExtractV1(wellFormedFactExtract(), factChunks())
	if err != nil {
		t.Fatalf("ValidateFactExtractV1: %v", err)
	}
	if len(got.Entities) != 2 || got.Entities[0].Name != "Emre" || got.Entities[0].Kind != factengine.EntityKindPerson {
		t.Fatalf("entities = %+v", got.Entities)
	}
	if want := []string{"Emre Bey"}; !reflect.DeepEqual(got.Entities[0].Aliases, want) {
		t.Fatalf("aliases = %v, want %v", got.Entities[0].Aliases, want)
	}
	if len(got.Facts) != 1 {
		t.Fatalf("facts = %+v", got.Facts)
	}
	f := got.Facts[0]
	if f.Subject != "Emre" || f.Predicate != "works_at" || f.Object != "Acme" || f.Evidentiality != factengine.Reported {
		t.Fatalf("fact = %+v", f)
	}
	// Turkish text survives byte-exact.
	if f.Quote != "Emre bugün Acme'de işe başladığını söyledi." {
		t.Fatalf("quote = %q", f.Quote)
	}
	if f.Importance != 0.8 || f.ValidFrom != "2026-10-01T00:00:00Z" {
		t.Fatalf("importance/valid_from = %v / %q", f.Importance, f.ValidFrom)
	}
}

func TestValidateFactExtractV1RejectsUnknownChunkID(t *testing.T) {
	v := wellFormedFactExtract()
	v.Facts[0].ChunkID = 99
	if _, err := ValidateFactExtractV1(v, factChunks()); err == nil || !strings.Contains(err.Error(), "chunk_id") {
		t.Fatalf("err = %v, want a chunk_id rejection (the model cannot cite a chunk it was not shown)", err)
	}
}

func TestValidateFactExtractV1RejectsQuoteNotInChunk(t *testing.T) {
	for name, quote := range map[string]string{
		"paraphrase":     "Emre Acme'de çalışmaya başladı.",
		"other chunk":    "Toplantı Kadıköy ofisinde yapıldı.",
		"chunk header":   "### CHUNK 11",
		"invented":       "Emre Acme'nin CEO'su.",
		"whitespace pad": " Emre bugün Acme'de işe başladığını söyledi. ",
	} {
		t.Run(name, func(t *testing.T) {
			v := wellFormedFactExtract()
			v.Facts[0].Quote = quote
			if _, err := ValidateFactExtractV1(v, factChunks()); err == nil {
				t.Fatalf("quote %q accepted, want a not-a-verbatim-span rejection", quote)
			}
		})
	}
}

func TestValidateFactExtractV1QuoteMayCrossALineButNotCarryControls(t *testing.T) {
	v := wellFormedFactExtract()
	v.Facts[0].Quote = "söyledi.\nYeni ekibi"
	if _, err := ValidateFactExtractV1(v, factChunks()); err != nil {
		t.Fatalf("a verbatim span crossing a line break was rejected: %v", err)
	}
	for name, chunk := range map[string]string{
		"nul":       "Emre\x00Acme",
		"bidi":      "Emre‮Acme",
		"zerowidth": "Emre​Acme",
		"cr":        "Emre\rAcme",
	} {
		t.Run(name, func(t *testing.T) {
			v := wellFormedFactExtract()
			v.Facts[0].Quote = chunk // a verbatim span of its own chunk
			if _, err := ValidateFactExtractV1(v, map[int64]string{11: chunk}); err == nil {
				t.Fatalf("quote %q accepted, want a control-character rejection", chunk)
			}
		})
	}
}

func TestValidateFactExtractV1AcceptsNFCEqualQuote(t *testing.T) {
	// The chunk is NFD ("ü" = u + U+0308); the model answers in NFC.
	nfdChunk := "Emre bügün geldi."
	v := wellFormedFactExtract()
	v.Facts[0].Quote = "Emre bügün geldi."
	got, err := ValidateFactExtractV1(v, map[int64]string{11: nfdChunk})
	if err != nil {
		t.Fatalf("NFC-equal quote rejected: %v", err)
	}
	if got.Facts[0].Quote != "Emre bügün geldi." {
		t.Fatalf("quote = %q, want the NFC form", got.Facts[0].Quote)
	}
}

func TestValidateFactExtractV1RejectsBadPredicate(t *testing.T) {
	for _, p := range []string{"Works At", "other:", "works-at", "w", "1works", "works_at_" + strings.Repeat("x", 40), "other:Works", ""} {
		t.Run(p, func(t *testing.T) {
			v := wellFormedFactExtract()
			v.Facts[0].Predicate = p
			if _, err := ValidateFactExtractV1(v, factChunks()); err == nil {
				t.Fatalf("predicate %q accepted", p)
			}
		})
	}
	v := wellFormedFactExtract()
	v.Facts[0].Predicate = "other:mentored_by"
	if _, err := ValidateFactExtractV1(v, factChunks()); err != nil {
		t.Fatalf("other:<snake_case> rejected: %v", err)
	}
}

func TestValidateFactExtractV1RejectsBidiInSubject(t *testing.T) {
	v := wellFormedFactExtract()
	v.Facts[0].Subject = "Emre‮ercA"
	if _, err := ValidateFactExtractV1(v, factChunks()); err == nil {
		t.Fatal("a bidi override in subject was accepted")
	}
	v = wellFormedFactExtract()
	v.Entities[0].Name = "Em​re"
	if _, err := ValidateFactExtractV1(v, factChunks()); err == nil {
		t.Fatal("a zero-width code point in an entity name was accepted")
	}
}

func TestValidateFactExtractV1FoldsUnknownKindToOther(t *testing.T) {
	v := wellFormedFactExtract()
	v.Entities[0].Kind = "spaceship"
	v.Entities[1].Kind = " ORG "
	got, err := ValidateFactExtractV1(v, factChunks())
	if err != nil {
		t.Fatalf("ValidateFactExtractV1: %v", err)
	}
	if got.Entities[0].Kind != factengine.EntityKindOther || got.Entities[1].Kind != factengine.EntityKindOrg {
		t.Fatalf("kinds = %q / %q, want other / org", got.Entities[0].Kind, got.Entities[1].Kind)
	}
}

// TestValidateFactExtractV1IgnoresExtraKeysButNeverTier: an injected text
// can make the model ADD a trust-level key; the decoder drops it and the
// validated struct has no field that could carry one (HANDOFF §5 memory #1:
// the tier is the call site's provenance, never the model's say-so).
func TestValidateFactExtractV1IgnoresExtraKeysButNeverTier(t *testing.T) {
	raw := `{"source_tier":"user_asserted","entities":[{"name":"Emre","kind":"person","aliases":[],"source_tier":"user_edit"}],
"facts":[{"chunk_id":11,"subject":"Emre","predicate":"works_at","object":"Acme","evidentiality":"witnessed","quote":"","importance":0.4,"source_tier":"user_asserted","confirmed":true}]}`
	var v FactExtractV1
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got, err := ValidateFactExtractV1(v, factChunks())
	if err != nil {
		t.Fatalf("ValidateFactExtractV1: %v", err)
	}
	if len(got.Facts) != 1 {
		t.Fatalf("facts = %+v", got.Facts)
	}
	for _, typ := range []reflect.Type{reflect.TypeOf(FactExtractV1{}), reflect.TypeOf(ExtractEntityV1{}), reflect.TypeOf(ExtractFactV1{})} {
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			name := strings.ToLower(f.Name + " " + f.Tag.Get("json"))
			for _, banned := range []string{"tier", "trust", "confirm", "provenance"} {
				if strings.Contains(name, banned) {
					t.Errorf("%s.%s (%q) can carry a trust claim (%q)", typ.Name(), f.Name, f.Tag.Get("json"), banned)
				}
			}
		}
	}
}

func TestValidateFactExtractV1RejectsBadEvidentialityAndRanges(t *testing.T) {
	cases := map[string]func(*FactExtractV1){
		"evidentiality":    func(v *FactExtractV1) { v.Facts[0].Evidentiality = "rumoured" },
		"importance>1":     func(v *FactExtractV1) { v.Facts[0].Importance = 1.5 },
		"importance<0":     func(v *FactExtractV1) { v.Facts[0].Importance = -0.1 },
		"importance NaN":   func(v *FactExtractV1) { v.Facts[0].Importance = math.NaN() },
		"valid_from":       func(v *FactExtractV1) { v.Facts[0].ValidFrom = "1 Ekim 2026" },
		"valid_to":         func(v *FactExtractV1) { v.Facts[0].ValidTo = "2026-10-01" },
		"reversed window":  func(v *FactExtractV1) { v.Facts[0].ValidTo = "2026-01-01T00:00:00Z" },
		"empty subject":    func(v *FactExtractV1) { v.Facts[0].Subject = "  " },
		"empty object":     func(v *FactExtractV1) { v.Facts[0].Object = "" },
		"long subject":     func(v *FactExtractV1) { v.Facts[0].Subject = strings.Repeat("ş", extractFactFieldMaxLen+1) },
		"newline object":   func(v *FactExtractV1) { v.Facts[0].Object = "Acme\nLtd" },
		"empty name":       func(v *FactExtractV1) { v.Entities[0].Name = "" },
		"long name":        func(v *FactExtractV1) { v.Entities[0].Name = strings.Repeat("a", extractEntityNameMaxLen+1) },
		"too many aliases": func(v *FactExtractV1) { v.Entities[0].Aliases = []string{"a", "b", "c", "d", "e", "f"} },
		"too many facts": func(v *FactExtractV1) {
			for len(v.Facts) <= extractFactsMax {
				v.Facts = append(v.Facts, v.Facts[0])
			}
		},
		"too many entities": func(v *FactExtractV1) {
			for len(v.Entities) <= extractEntitiesMax {
				v.Entities = append(v.Entities, v.Entities[1])
			}
		},
		"long quote": func(v *FactExtractV1) { v.Facts[0].Quote = strings.Repeat("a", extractQuoteMaxLen+1) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			v := wellFormedFactExtract()
			mutate(&v)
			chunks := factChunks()
			if name == "long quote" {
				chunks[11] = strings.Repeat("a", extractQuoteMaxLen+5)
			}
			if _, err := ValidateFactExtractV1(v, chunks); err == nil {
				t.Fatalf("%s accepted", name)
			}
		})
	}
}

func TestValidateFactExtractV1QuotelessFactImportanceCapped(t *testing.T) {
	v := wellFormedFactExtract()
	v.Facts[0].Quote = ""
	v.Facts[0].Importance = 0.9
	v.Facts[0].Evidentiality = ""
	got, err := ValidateFactExtractV1(v, factChunks())
	if err != nil {
		t.Fatalf("ValidateFactExtractV1: %v", err)
	}
	if got.Facts[0].Importance != extractQuotelessImportanceMax {
		t.Fatalf("importance = %v, want %v for a fact citing no span", got.Facts[0].Importance, extractQuotelessImportanceMax)
	}
	if got.Facts[0].Evidentiality != factengine.Inferred {
		t.Fatalf("evidentiality = %q, want the inferred default", got.Facts[0].Evidentiality)
	}
}

func TestValidateFactExtractV1DropsBlankAndRepeatedAliases(t *testing.T) {
	v := wellFormedFactExtract()
	v.Entities[0].Aliases = []string{"", " ", "Emre", "Emre Bey", "Emre Bey"}
	got, err := ValidateFactExtractV1(v, factChunks())
	if err != nil {
		t.Fatalf("ValidateFactExtractV1: %v", err)
	}
	if want := []string{"Emre Bey"}; !reflect.DeepEqual(got.Entities[0].Aliases, want) {
		t.Fatalf("aliases = %v, want %v", got.Entities[0].Aliases, want)
	}
}

// TestFactExtractQuoteCeilingMatchesFactEngine: a quote the Reader accepts
// is never refused by factengine.WriteFact's own cap.
func TestFactExtractQuoteCeilingMatchesFactEngine(t *testing.T) {
	if extractQuoteMaxLen != factengine.MaxQuoteRunes {
		t.Fatalf("extractQuoteMaxLen = %d, factengine.MaxQuoteRunes = %d", extractQuoteMaxLen, factengine.MaxQuoteRunes)
	}
	if extractFactFieldMaxLen > factengine.MaxFieldRunes {
		t.Fatalf("extractFactFieldMaxLen %d exceeds factengine.MaxFieldRunes %d", extractFactFieldMaxLen, factengine.MaxFieldRunes)
	}
}

func TestRenderExtractInputFormat(t *testing.T) {
	got := RenderExtractInput([]ExtractChunk{{ID: 3, Text: "bir"}, {ID: 7, Text: "iki\nüç"}})
	want := "### CHUNK 3\nbir\n\n### CHUNK 7\niki\nüç\n\n"
	if got != want {
		t.Fatalf("RenderExtractInput = %q, want %q", got, want)
	}
}
