package router

import "testing"

// TestSelectModelMatrixEveryTableRow proves every §4 table row (+ the
// chat/unknown default fallback) resolves to the documented model, in the
// ordinary (no lane pin, no deep-think, no downgrade) case.
func TestSelectModelMatrixEveryTableRow(t *testing.T) {
	cases := []struct {
		intent    string
		wantLocal bool
		wantModel string
	}{
		{IntentPlan, false, ModelOpus},
		{IntentCodeMultiFile, false, ModelOpus},
		{IntentHardExec, false, ModelOpus},
		{IntentSubagentExec, false, ModelSonnet},
		{IntentFanout, false, ModelSonnet},
		{IntentExtract, false, ModelHaiku},
		{IntentWriteback, false, ModelHaiku},
		{IntentRoute, true, ""},
		{IntentClassify, true, ""},
		{IntentReader, false, ModelHaiku},
		{IntentChat, false, ModelSonnet},
		{"", false, ModelSonnet},                       // absent intent -> default row
		{"totally_unknown_intent", false, ModelSonnet}, // unrecognized -> default row
	}
	for _, tc := range cases {
		t.Run(tc.intent, func(t *testing.T) {
			got := SelectModel(RouteInput{Intent: tc.intent, Lane: LaneNormal, DefaultModel: ModelSonnet})
			if got.Local != tc.wantLocal {
				t.Errorf("SelectModel(intent=%q).Local = %v, want %v", tc.intent, got.Local, tc.wantLocal)
			}
			if got.Model != tc.wantModel {
				t.Errorf("SelectModel(intent=%q).Model = %q, want %q", tc.intent, got.Model, tc.wantModel)
			}
		})
	}
}

// TestSelectModelUsesConfiguredDefaultModel proves the chat/unknown row
// reads RouteInput.DefaultModel rather than a hardcoded literal.
func TestSelectModelUsesConfiguredDefaultModel(t *testing.T) {
	got := SelectModel(RouteInput{Intent: IntentChat, Lane: LaneNormal, DefaultModel: ModelOpus})
	if got.Model != ModelOpus || got.Local {
		t.Errorf("SelectModel() = %+v, want Model=%q Local=false", got, ModelOpus)
	}
}

// TestSelectModelFableOnlyWithDeepThink proves claude-fable-5 is NEVER
// reachable through any table row/intent alone - only DeepThink==true ever
// produces it, and every intent produces it identically once DeepThink is
// set.
func TestSelectModelFableOnlyWithDeepThink(t *testing.T) {
	allIntents := []string{
		IntentPlan, IntentCodeMultiFile, IntentHardExec, IntentSubagentExec, IntentFanout,
		IntentExtract, IntentWriteback, IntentRoute, IntentClassify, IntentReader, IntentChat, "",
	}
	for _, intent := range allIntents {
		for _, downgraded := range []bool{false, true} {
			got := SelectModel(RouteInput{Intent: intent, Lane: LaneNormal, DeepThink: false, Downgraded: downgraded, DefaultModel: ModelSonnet})
			if got.Model == ModelFable {
				t.Errorf("SelectModel(intent=%q, deep_think=false, downgraded=%v) = %+v, want claude-fable-5 NEVER selected without deep_think", intent, downgraded, got)
			}
		}
		got := SelectModel(RouteInput{Intent: intent, Lane: LaneNormal, DeepThink: true, DefaultModel: ModelSonnet})
		if got.Local || got.Model != ModelFable {
			t.Errorf("SelectModel(intent=%q, deep_think=true) = %+v, want Model=claude-fable-5 Local=false", intent, got)
		}
	}
}

// TestSelectModelSecretLanePinsLocalForEveryIntent proves Lane==secret pins
// the local lane for EVERY intent, and OUTRANKS deep_think and downgrade
// alike (HANDOFF §4 ⚑ ordering invariant) - the exact assertion the W4-08
// task spec calls out by name.
func TestSelectModelSecretLanePinsLocalForEveryIntent(t *testing.T) {
	allIntents := []string{
		IntentPlan, IntentCodeMultiFile, IntentHardExec, IntentSubagentExec, IntentFanout,
		IntentExtract, IntentWriteback, IntentRoute, IntentClassify, IntentReader, IntentChat, "",
	}
	for _, intent := range allIntents {
		for _, deepThink := range []bool{false, true} {
			for _, downgraded := range []bool{false, true} {
				got := SelectModel(RouteInput{
					Intent: intent, Lane: LaneSecret, DeepThink: deepThink, Downgraded: downgraded, DefaultModel: ModelSonnet,
				})
				if !got.Local || got.Model != "" {
					t.Errorf("SelectModel(intent=%q, lane=secret, deep_think=%v, downgraded=%v) = %+v, want Local=true Model=\"\" (secret lane must outrank everything)",
						intent, deepThink, downgraded, got)
				}
				if got.DerinDuringDowngrade {
					t.Errorf("SelectModel(intent=%q, lane=secret, deep_think=%v, downgraded=%v).DerinDuringDowngrade = true, want false (the pin returns before deep_think is even examined)",
						intent, deepThink, downgraded)
				}
			}
		}
	}
}

// TestSelectModelEmptyLaneTreatedAsNormal proves "" behaves identically to
// LaneNormal (matches spawn.Envelope.Lane's own documented empty
// convention).
func TestSelectModelEmptyLaneTreatedAsNormal(t *testing.T) {
	got := SelectModel(RouteInput{Intent: IntentPlan, Lane: "", DefaultModel: ModelSonnet})
	if got.Local || got.Model != ModelOpus {
		t.Errorf("SelectModel(lane=\"\") = %+v, want the same as lane=normal (Model=%q)", got, ModelOpus)
	}
}

// TestSelectModelDowngradeChain proves the fixed Opus->Sonnet->yerel chain:
// an Opus-class task drops to Sonnet; a Sonnet-class task drops to the
// local lane; Haiku-class and already-local rows are UNAFFECTED (no rung
// is defined for them).
func TestSelectModelDowngradeChain(t *testing.T) {
	cases := []struct {
		name      string
		intent    string
		wantLocal bool
		wantModel string
	}{
		{"plan(opus)->sonnet", IntentPlan, false, ModelSonnet},
		{"code_multi_file(opus)->sonnet", IntentCodeMultiFile, false, ModelSonnet},
		{"hard_exec(opus)->sonnet", IntentHardExec, false, ModelSonnet},
		{"subagent_exec(sonnet)->local", IntentSubagentExec, true, ""},
		{"fanout(sonnet)->local", IntentFanout, true, ""},
		{"chat(default=sonnet)->local", IntentChat, true, ""},
		{"extract(haiku) unaffected", IntentExtract, false, ModelHaiku},
		{"writeback(haiku) unaffected", IntentWriteback, false, ModelHaiku},
		{"reader(haiku) unaffected", IntentReader, false, ModelHaiku},
		{"route(local) unaffected", IntentRoute, true, ""},
		{"classify(local) unaffected", IntentClassify, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SelectModel(RouteInput{Intent: tc.intent, Lane: LaneNormal, Downgraded: true, DefaultModel: ModelSonnet})
			if got.Local != tc.wantLocal || got.Model != tc.wantModel {
				t.Errorf("SelectModel(intent=%q, downgraded=true) = %+v, want Local=%v Model=%q", tc.intent, got, tc.wantLocal, tc.wantModel)
			}
		})
	}
}

// TestSelectModelDeepThinkHonoredUnderDowngradeButFlagged proves an
// explicit derin opt-in is NOT itself a router choice subject to
// downgrade: it is still honored (Model stays claude-fable-5) even when
// Downgraded is true, but the decision is flagged (DerinDuringDowngrade)
// so the caller can ledger derin_during_downgrade + its own Turkish spend
// warning.
func TestSelectModelDeepThinkHonoredUnderDowngradeButFlagged(t *testing.T) {
	got := SelectModel(RouteInput{Intent: IntentChat, Lane: LaneNormal, DeepThink: true, Downgraded: true, DefaultModel: ModelSonnet})
	if got.Local || got.Model != ModelFable {
		t.Errorf("SelectModel(deep_think=true, downgraded=true) = %+v, want Model=claude-fable-5 (honored despite downgrade)", got)
	}
	if !got.DerinDuringDowngrade {
		t.Error("SelectModel(deep_think=true, downgraded=true).DerinDuringDowngrade = false, want true")
	}

	notDowngraded := SelectModel(RouteInput{Intent: IntentChat, Lane: LaneNormal, DeepThink: true, Downgraded: false, DefaultModel: ModelSonnet})
	if notDowngraded.DerinDuringDowngrade {
		t.Error("SelectModel(deep_think=true, downgraded=false).DerinDuringDowngrade = true, want false")
	}
}

// TestLogRoutingDecisionNilLedgerNoop proves a nil Ledger is a safe no-op
// (matches every other optional-dependency Ledger consumer's posture).
func TestLogRoutingDecisionNilLedgerNoop(t *testing.T) {
	LogRoutingDecision(nil, nil, "trace1", RouteInput{Intent: IntentChat}, RouteDecision{Model: ModelSonnet})
}
