package anthproxy

import (
	"bytes"
	"encoding/json"
	"unicode"
	"unicode/utf8"
)

// topLevelFields decodes body's top-level JSON object into its members,
// keyed by their EXACT (case-sensitive) names - the way the upstream API
// reads them. It reports ok=false unless body is exactly one well-formed
// JSON object whose member names are pairwise distinct under encoding/
// json's own case folding (FIX-I-10, review).
//
// Why: encoding/json matches a struct tag such as `json:"model"` case-
// insensitively and lets the LAST match win, while the upstream reads the
// exact key (and another parser may keep the first of two duplicates). A
// body like {"model":"A","MODEL":"B"} or {"model":"A","model":"B"} could
// therefore be allowlisted and priced by this proxy as one model and run
// upstream as another. No real client sends such a body, so the proxy
// refuses it instead of guessing which member the upstream will honor
// (fail-closed). Every decision the proxy takes from a request body - the
// model allowlist and pricing (probeRequest), Fable-5 shaping
// (shapeFable5Body) and the governor's max_tokens estimate
// (parseEstimateProbe) - reads the body through this one function.
func topLevelFields(body []byte) (map[string]json.RawMessage, bool) {
	if !json.Valid(body) {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, false
	}
	fields := make(map[string]json.RawMessage)
	seen := make(map[string]bool)
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, isKey := tok.(string)
		if !isKey {
			return nil, false
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, false
		}
		folded := foldJSONName(key)
		if seen[folded] {
			return nil, false
		}
		seen[folded] = true
		fields[key] = raw
	}
	return fields, true
}

// foldJSONName folds a member name exactly as encoding/json does when it
// matches names to struct fields (ASCII upper-casing; for other runes
// unicode.ToUpper(unicode.ToLower(r))), so two names collide here exactly
// when encoding/json could confuse them.
func foldJSONName(name string) string {
	out := make([]byte, 0, len(name))
	for i := 0; i < len(name); {
		c := name[i]
		if c < utf8.RuneSelf {
			if 'a' <= c && c <= 'z' {
				c -= 'a' - 'A'
			}
			out = append(out, c)
			i++
			continue
		}
		r, n := utf8.DecodeRuneInString(name[i:])
		out = utf8.AppendRune(out, unicode.ToUpper(unicode.ToLower(r)))
		i += n
	}
	return string(out)
}
