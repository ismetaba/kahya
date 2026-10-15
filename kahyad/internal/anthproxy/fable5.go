package anthproxy

import (
	"encoding/json"
	"strings"

	"kahya/kahyad/internal/router"
)

// fable5Models is the set of model ids this shaping ever triggers on: the
// current Fable (router.ModelFable, claude-fable-5-1) and the legacy one
// (router.LegacyModelFable, claude-fable-5) a task created before the
// G12-N2 bump may still resume with (owner decision G12-6: both stay
// shaped for as long as both are allowlisted). Derived from the router's
// HANDOFF §9 constants, never a new literal.
var fable5Models = map[string]bool{
	router.ModelFable:       true,
	router.LegacyModelFable: true,
}

// Fable5ShapedModels returns the ids shapeFable5Body rewrites (sorted
// copy, for cross-package drift tests).
func Fable5ShapedModels() []string {
	return []string{router.LegacyModelFable, router.ModelFable}
}

// Fable5RequiredBeta is HANDOFF §4 routing's own fixed beta, verbatim:
// "Fable ... daima betas:["server-side-fallback-2026-06-01"] +
// fallbacks:[{model:"claude-opus-4-8"}] ile". The array form of fallbacks
// requires EXACTLY this beta: pairing the array with the newer
// server-side-fallback-2026-07-01 (whose form is fallbacks:"default") is a
// 400, so shapeFable5Body drops any other server-side-fallback-* beta.
const Fable5RequiredBeta = "server-side-fallback-2026-06-01"

// fallbackBetaPrefix is the family Fable5RequiredBeta belongs to; any other
// member of it in a shaped body is removed (see Fable5RequiredBeta).
const fallbackBetaPrefix = "server-side-fallback-"

// Fable5FallbackModel is the ONLY model Fable's own fallback ever names
// (HANDOFF §4 :109). Owner decision G12-6: Fable 5.1's permitted
// server-side fallback targets are only claude-opus-4-8 and claude-opus-5,
// so the fallback is NOT the current Opus (claude-opus-5-5 - not a
// permitted target) and claude-opus-4-8 therefore stays allowlisted and
// priced (kahyad/internal/server's TestModelIDsMatchHandoffSection9 pins
// it to the HANDOFF text, the allowlist and the pricing table). A literal
// on purpose: it is an upstream API fact about Fable, not a routing choice,
// so it must not move when router.ModelOpus moves. Never the reverse -
// this shaping only ever applies to a request already naming a Fable id;
// it never upgrades any other model INTO Fable.
const Fable5FallbackModel = "claude-opus-4-8"

type fable5Fallback struct {
	Model string `json:"model"`
}

// shapeFable5Body implements the task spec's step 7, verbatim: "for any
// request body with model:claude-fable-5, ensure betas contains
// server-side-fallback-2026-06-01 and fallbacks equals
// [{model:claude-opus-4-8}]; inject them if absent" - since FIX-G12-N2
// for every id in fable5Models. Returns the (possibly rewritten) body and
// whether this body named a Fable id at all (shaped==true whenever the
// model matched, regardless of whether an injection actually changed
// anything - kahyad still ledgers proxy.fable5_shaped every time, an audit
// trail of "this call's shaping invariant was checked" rather than only
// "something changed this time").
//
// A malformed/non-JSON body, or one naming any OTHER model, is returned
// completely unchanged (shaped=false) - this function never invents a
// model field, never upgrades a non-Fable request into one (HANDOFF §4:
// "Fable asla varsayılan değil"), and never adds fallbacks to any other
// model (Haiku 5.5 has no server-side fallback at all). FIX-I-10
// (review): the body is read through topLevelFields, so an ambiguous one
// (duplicate or case-variant member names) is never re-marshaled into a
// clean single-model body here - it stays unchanged and the model
// allowlist refuses it.
func shapeFable5Body(body []byte) (out []byte, shaped bool) {
	doc, ok := topLevelFields(body)
	if !ok {
		return body, false
	}

	var model string
	if raw, ok := doc["model"]; ok {
		_ = json.Unmarshal(raw, &model)
	}
	if !fable5Models[model] {
		return body, false
	}

	betas := []string{}
	for _, b := range decodeBetas(doc["betas"]) {
		if strings.HasPrefix(b, fallbackBetaPrefix) && b != Fable5RequiredBeta {
			continue
		}
		betas = append(betas, b)
	}
	if !containsString(betas, Fable5RequiredBeta) {
		betas = append(betas, Fable5RequiredBeta)
	}
	betasJSON, err := json.Marshal(betas)
	if err != nil {
		return body, false
	}
	doc["betas"] = betasJSON

	fallbacks := []fable5Fallback{{Model: Fable5FallbackModel}}
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
