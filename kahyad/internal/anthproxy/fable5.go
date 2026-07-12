package anthproxy

import "encoding/json"

// fable5Model is the one model literal this shaping ever triggers on
// (HANDOFF §9: model IDs come from the handoff only, never invented here).
const fable5Model = "claude-fable-5"

// fable5RequiredBeta/fable5RequiredFallback are HANDOFF §4 routing's own
// fixed values, verbatim: "Fable 5 ... daima betas:["server-side-fallback-
// 2026-06-01"] + fallbacks:[{model:"claude-opus-4-8"}] ile".
const fable5RequiredBeta = "server-side-fallback-2026-06-01"

// fable5FallbackModel is the ONLY model Fable-5's own fallback ever names
// (HANDOFF §9's own routing table: "claude-opus-4-8"). Never the reverse -
// this shaping only ever applies to a request already naming
// fable5Model; it never upgrades any other model INTO Fable-5.
const fable5FallbackModel = "claude-opus-4-8"

type fable5Fallback struct {
	Model string `json:"model"`
}

// shapeFable5Body implements the task spec's step 7, verbatim: "for any
// request body with model:claude-fable-5, ensure betas contains
// server-side-fallback-2026-06-01 and fallbacks equals
// [{model:claude-opus-4-8}]; inject them if absent". Returns the
// (possibly rewritten) body and whether this body named claude-fable-5 at
// all (shaped==true whenever the model matched, regardless of whether an
// injection actually changed anything - kahyad still ledgers
// proxy.fable5_shaped every time, an audit trail of "this call's shaping
// invariant was checked" rather than only "something changed this time").
//
// A malformed/non-JSON body, or one naming any OTHER model, is returned
// completely unchanged (shaped=false) - this function never invents a
// model field, and never upgrades a non-Fable-5 request into one (HANDOFF
// §4: "Fable 5 asla varsayılan değil").
func shapeFable5Body(body []byte) (out []byte, shaped bool) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		return body, false
	}

	var model string
	if raw, ok := doc["model"]; ok {
		_ = json.Unmarshal(raw, &model)
	}
	if model != fable5Model {
		return body, false
	}

	betas := decodeBetas(doc["betas"])
	if !containsString(betas, fable5RequiredBeta) {
		betas = append(betas, fable5RequiredBeta)
	}
	betasJSON, err := json.Marshal(betas)
	if err != nil {
		return body, false
	}
	doc["betas"] = betasJSON

	fallbacks := []fable5Fallback{{Model: fable5FallbackModel}}
	fallbacksJSON, err := json.Marshal(fallbacks)
	if err != nil {
		return body, false
	}
	doc["fallbacks"] = fallbacksJSON

	rewritten, err := json.Marshal(doc)
	if err != nil {
		return body, false
	}
	return rewritten, true
}

// decodeBetas best-effort decodes an existing "betas" field as a string
// array - any other shape (absent, malformed, wrong type) is treated as
// "no existing betas", so shapeFable5Body always ends up with a valid
// array containing at least the required beta.
func decodeBetas(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var betas []string
	if err := json.Unmarshal(raw, &betas); err != nil {
		return nil
	}
	return betas
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
