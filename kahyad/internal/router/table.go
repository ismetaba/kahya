// Package router implements W4-08: the HANDOFF §4 model-routing table as
// executable Go data plus a PURE function, SelectModel, that decides which
// model answers one task. Routing runs STRICTLY AFTER secret-lane
// classification (HANDOFF §4 ⚑ ordering invariant, verbatim: "Hiçbir bayt,
// gizli-şerit sınıflandırması yerel/deterministik olarak tamamlanmadan
// bulut modele gitmez.") - callers pass the ALREADY-DECIDED lane in via
// RouteInput.Lane; this package never classifies anything itself (see
// intent.go for the intent-classification half, which defers entirely to
// kahyad/internal/secretlane's own combined classify call).
//
// LOCAL LANE IS NOT AN ENVELOPE MODEL. kahyad/internal/spawn.Envelope.Model
// is validated against EXACTLY the four HANDOFF §9 cloud models
// (spawn.AllowedModels) - "local"/"yerel" is never a legal value there, and
// this package deliberately never invents one. When RouteDecision.Local is
// true, the caller (kahyad/internal/server's POST /v1/task handler) must
// answer the task via the SAME local-answer mechanism W3-08's secret-lane
// branch already uses (kahyad/internal/secretlane.Answerer,
// handleSecretLaneTask) - it must NEVER write a non-§9-model string into
// envelope.Model to represent "local"; RouteDecision.Local drives which
// BRANCH runs, never the cloud envelope.
package router

import "context"

// Intent is the task-type bucket the §4 routing table keys on (HANDOFF §4
// table rows, left column, Turkish verbatim in parens):
//
//	plan · code_multi_file · hard_exec   ("Planlama · zor yürütme · çok-dosya kod")
//	subagent_exec · fanout               ("Alt-ajan yürütme · fan-out")
//	route · classify                     ("Yönlendirme / sınıflandırma")
//	extract · writeback                  ("Çıkarım · geri-yazım")
//	reader                                ("Okuyucu")
//	chat                                  (unrecognized/absent - the default row)
//
// These are duplicated as plain string literals (never imported) by
// kahyad/internal/secretlane's extended classify prompt - the SAME "keep
// two copies in sync by hand" convention this codebase already uses for
// kahyad/internal/spawn.LaneSecret/secretlane.LaneSecret (see
// spawn/envelope.go's own doc comment for the identical rationale: a
// low-level package should not need to import a higher-level one just for
// a handful of string constants).
const (
	IntentPlan          = "plan"
	IntentCodeMultiFile = "code_multi_file"
	IntentHardExec      = "hard_exec"
	IntentSubagentExec  = "subagent_exec"
	IntentFanout        = "fanout"
	IntentExtract       = "extract"
	IntentWriteback     = "writeback"
	IntentRoute         = "route"
	IntentClassify      = "classify"
	IntentReader        = "reader"
	IntentChat          = "chat"
)

// Model* are the HANDOFF §9 cloud model literals (mirrors
// kahyad/internal/spawn.AllowedModels' own four keys, duplicated rather
// than imported for the same reason Intent's own doc comment gives).
const (
	ModelOpus   = "claude-opus-4-8"
	ModelSonnet = "claude-sonnet-5"
	ModelHaiku  = "claude-haiku-4-5"
	ModelFable  = "claude-fable-5"
)

// LaneSecret/LaneNormal mirror kahyad/internal/secretlane's OWN identical
// constants (same duplication convention as spawn.LaneSecret/LaneNormal).
const (
	LaneSecret = "secret"
	LaneNormal = "normal"
)

// Reason values RouteDecision.Reason may hold - an internal, English-only
// diagnostic (ledger/log payloads only, NEVER user-facing - CLAUDE.md's
// Turkish-user-facing-strings policy does not apply here).
const (
	ReasonSecretLanePin          = "secret_lane_pin"
	ReasonDeepThinkOptIn         = "deep_think_opt_in"
	ReasonTableRouteClassify     = "table_route_classify"
	ReasonTableReader            = "table_reader"
	ReasonTableDefaultChat       = "table_default_chat"
	ReasonDowngradeOpusToSonnet  = "downgrade_opus_to_sonnet"
	ReasonDowngradeSonnetToLocal = "downgrade_sonnet_to_local"
)

// tableReasonPrefix names an ordinary (non-pinned, non-downgraded) table
// row's Reason: "table_" + the Intent literal that produced it (e.g.
// "table_plan", "table_subagent_exec") - readable in a ledger dump without
// a lookup table of its own.
const tableReasonPrefix = "table_"

// RouteInput is SelectModel's pure input.
type RouteInput struct {
	// Intent is one of the Intent* constants above. Empty or unrecognized
	// values fall through to the table's own chat/unknown default row -
	// SelectModel never errors, it always returns a decision.
	Intent string
	// Lane is LaneSecret/LaneNormal ("" is treated identically to
	// LaneNormal, matching spawn.Envelope.Lane's own documented empty
	// convention). LaneSecret PINS the local lane for EVERY intent,
	// outranking DeepThink and Downgraded alike (HANDOFF §4 ⚑ ordering
	// invariant) - checked FIRST, before this function looks at anything
	// else.
	Lane string
	// DeepThink is the "derin düşün" opt-in (HANDOFF §4: pins
	// claude-fable-5 - the ONLY way it is ever selected, see
	// TestSelectModelFableOnlyWithDeepThink). Outranks Downgraded: an
	// opted-in deep-think call is still HONORED mid-downgrade - the caller
	// separately ledgers derin_during_downgrade (RouteDecision.
	// DerinDuringDowngrade) with its own Turkish spend warning.
	DeepThink bool
	// Downgraded mirrors kahyad/internal/anthproxy.Governor.Downgraded() -
	// today's spend has crossed the cost-governor's 80% rung. Drops the
	// table's base (non-pinned, non-opted-in) model exactly ONE rung per
	// HANDOFF §4's fixed chain: claude-opus-4-8 -> claude-sonnet-5,
	// claude-sonnet-5 -> the local lane. Never touches claude-haiku-4-5 (no
	// rung is defined for it) or an already-Local base decision (already
	// the cheapest rung there is).
	Downgraded bool
	// DefaultModel is cfg.default_model - what the chat/unrecognized row
	// resolves to (HANDOFF §4: "chat/unknown -> cfg.default_model
	// (claude-sonnet-5)"). This is the one field beyond the task spec's own
	// four-field shorthand (RouteInput{Intent, Lane, DeepThink,
	// Downgraded}): cfg.default_model is itself configurable (not always
	// literally "claude-sonnet-5" - kahyad/internal/config lets an operator
	// override it), so SelectModel must take it as an explicit input to
	// stay a pure value->value function with no hidden config/global
	// dependency at all. See this task's own closing report for this
	// deliberate, documented deviation from the literal four-field list.
	DefaultModel string
}

// RouteDecision is SelectModel's pure output.
type RouteDecision struct {
	// Local is true when this task must be answered entirely on-device
	// (kahyad/internal/secretlane.Answerer) - Model is empty whenever this
	// is true. The caller must NEVER fall through to building a cloud
	// envelope/opening an Anthropic proxy listener in that case (see this
	// package's own doc comment).
	Local bool
	// Model is one of the four HANDOFF §9 cloud models - meaningful only
	// when !Local.
	Model string
	// Reason is an internal, English-only diagnostic naming which rule
	// produced this decision (ledger/log payloads only).
	Reason string
	// DerinDuringDowngrade is true iff this decision is claude-fable-5 (the
	// DeepThink opt-in) AND Downgraded was also true - the opt-in is still
	// HONORED (Model stays claude-fable-5; deep-think is never itself
	// downgraded), but the caller must ledger derin_during_downgrade with a
	// Turkish spend warning in its own output (HANDOFF §4 cost governor ⚑).
	// Always false whenever Lane==secret (the secret-lane pin returns
	// before DeepThink is even examined - the pin outranks the opt-in
	// entirely, so there is nothing to warn about).
	DerinDuringDowngrade bool
}

// SelectModel implements the §4 routing table + the ⚑ ordering invariant +
// the ⚑ cost-governor downgrade rung, as one pure function.
func SelectModel(in RouteInput) RouteDecision {
	lane := in.Lane
	if lane == "" {
		lane = LaneNormal
	}
	// Ordering invariant: a secret-lane verdict PINS the local lane and
	// OUTRANKS intent, deep_think, AND downgrade - all of them. Checked
	// first, unconditionally; nothing below this ever runs for a
	// secret-lane task.
	if lane == LaneSecret {
		return RouteDecision{Local: true, Reason: ReasonSecretLanePin}
	}

	// "Derin düşün" opt-in: the ONLY way claude-fable-5 is ever selected.
	// Honored even mid-downgrade (flagged via DerinDuringDowngrade for the
	// caller's own ledger+Turkish-warning side effect) - deep_think is
	// never itself downgraded, only the table's ordinary rows are.
	if in.DeepThink {
		return RouteDecision{Model: ModelFable, Reason: ReasonDeepThinkOptIn, DerinDuringDowngrade: in.Downgraded}
	}

	model, local, reason := baseRowFor(in.Intent, in.DefaultModel)
	if in.Downgraded && !local {
		switch model {
		case ModelOpus:
			return RouteDecision{Model: ModelSonnet, Reason: ReasonDowngradeOpusToSonnet}
		case ModelSonnet:
			return RouteDecision{Local: true, Reason: ReasonDowngradeSonnetToLocal}
		}
	}
	return RouteDecision{Model: model, Local: local, Reason: reason}
}

// baseRowFor resolves the §4 table's ordinary (non-pinned, non-opted-in)
// row for intent, BEFORE any downgrade rung is applied. Only ever reached
// for lane==normal (SelectModel's secret-lane pin above always returns
// first) - so the IntentReader case below only needs to encode the
// Okuyucu row's NON-secret half (secret ⇒ Qwen local is already covered by
// the pin, unconditionally, for every intent including reader).
func baseRowFor(intent, defaultModel string) (model string, local bool, reason string) {
	switch intent {
	case IntentPlan, IntentCodeMultiFile, IntentHardExec:
		return ModelOpus, false, tableReasonPrefix + intent
	case IntentSubagentExec, IntentFanout:
		return ModelSonnet, false, tableReasonPrefix + intent
	case IntentExtract, IntentWriteback:
		return ModelHaiku, false, tableReasonPrefix + intent
	case IntentRoute, IntentClassify:
		// "Yönlendirme / sınıflandırma -> yerel Qwen3-30B-A3B" - the
		// classifier itself, never a cloud model.
		return "", true, ReasonTableRouteClassify
	case IntentReader:
		return ModelHaiku, false, ReasonTableReader
	default:
		return defaultModel, false, ReasonTableDefaultChat
	}
}

// EventRoutingDecision is the ledger/JSONL event LogRoutingDecision emits.
const EventRoutingDecision = "routing_decision"

// Ledger is the narrow append-only-events seam this package needs (mirrors
// every other package's identical interface - kahyad/internal/store's own
// LogEvent already satisfies this with no adapter).
type Ledger interface {
	LogEvent(ctx context.Context, traceID, kind string, payload map[string]any) error
}

// LogRoutingDecision records one SelectModel call's input+output under
// traceID - the "routing decision" step the W4-08 task spec's own ledger-
// ordering acceptance criterion names (intent_classified -> routing
// decision -> model_call, all under one trace_id). ledger may be nil (the
// same "unwired dependency is a no-op" posture every other Ledger consumer
// in this codebase already has).
func LogRoutingDecision(ctx context.Context, ledger Ledger, traceID string, in RouteInput, decision RouteDecision) {
	if ledger == nil {
		return
	}
	model := decision.Model
	if decision.Local {
		model = "local"
	}
	_ = ledger.LogEvent(ctx, traceID, EventRoutingDecision, map[string]any{
		"intent": in.Intent, "lane": in.Lane, "deep_think": in.DeepThink,
		"downgraded": in.Downgraded, "model": model, "local": decision.Local,
		"reason": decision.Reason, "derin_during_downgrade": decision.DerinDuringDowngrade,
	})
}
