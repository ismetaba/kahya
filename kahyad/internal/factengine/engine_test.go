package factengine

import (
	"context"
	"math"
	"net/http"
	"testing"

	"kahya/kahyad/internal/secretlane"
)

const floatTolerance = 1e-9

func almostEqual(a, b float64) bool { return math.Abs(a-b) < floatTolerance }

// TestWriteFactValidatesRequiredFields covers HANDOFF S5 safety#2's
// schema validation step: subject/predicate/object/extractor_ver are all
// required, and evidentiality must be a recognized enum value when set
// at all.
func TestWriteFactValidatesRequiredFields(t *testing.T) {
	e, _ := newTestEngine(t)
	ctx := context.Background()

	base := Candidate{Subject: "a", Predicate: "b", Object: "c", ExtractorVer: "v1"}

	cases := []struct {
		name string
		mut  func(c Candidate) Candidate
	}{
		{"missing_subject", func(c Candidate) Candidate { c.Subject = ""; return c }},
		{"missing_predicate", func(c Candidate) Candidate { c.Predicate = ""; return c }},
		{"missing_object", func(c Candidate) Candidate { c.Object = ""; return c }},
		{"missing_extractor_ver", func(c Candidate) Candidate { c.ExtractorVer = ""; return c }},
		{"invalid_evidentiality", func(c Candidate) Candidate { c.Evidentiality = "sanılan"; return c }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := e.WriteFact(ctx, "trace", tc.mut(base)); err == nil {
				t.Fatalf("WriteFact(%s) error = nil, want a validation error", tc.name)
			}
		})
	}
}

// TestWriteFactRejectsOverlongField covers the S5 safety#2 length cap.
func TestWriteFactRejectsOverlongField(t *testing.T) {
	e, _ := newTestEngine(t)
	ctx := context.Background()

	tooLong := make([]byte, MaxFieldRunes+1)
	for i := range tooLong {
		tooLong[i] = 'a'
	}
	if _, err := e.WriteFact(ctx, "trace", Candidate{
		Subject: string(tooLong), Predicate: "b", Object: "c", ExtractorVer: "v1",
	}); err == nil {
		t.Fatal("WriteFact(overlong subject) error = nil, want a validation error")
	}
}

// TestWriteFactRejectsControlCharacters covers the S5 safety#2 charclass
// cap.
func TestWriteFactRejectsControlCharacters(t *testing.T) {
	e, _ := newTestEngine(t)
	ctx := context.Background()

	if _, err := e.WriteFact(ctx, "trace", Candidate{
		Subject: "a\x00b", Predicate: "b", Object: "c", ExtractorVer: "v1",
	}); err == nil {
		t.Fatal("WriteFact(NUL byte in subject) error = nil, want a validation error")
	}
}

// TestAgentDerivedFactQuarantinedUntilConfirmed is the acceptance
// criterion: an agent_derived fact is absent from injection-eligible
// output; after `kahya fact confirm <id>` it appears; its confidence
// never exceeds the 0.4 tier cap.
func TestAgentDerivedFactQuarantinedUntilConfirmed(t *testing.T) {
	e, _ := newTestEngine(t)
	ctx := context.Background()

	factID, err := e.WriteFact(ctx, "trace", Candidate{
		Subject: "episode:1", Predicate: "contains_number", Object: "1500 TL",
		// No Provenance set at all - defaults to agent_derived, exactly
		// what a hot-window/extractor candidate with no special claim
		// gets.
		Evidentiality: Inferred, ExtractorVer: "hotwindow-v1",
		Evidence: "episode:1,chunk:2",
	})
	if err != nil {
		t.Fatalf("WriteFact error = %v", err)
	}

	fact, err := e.GetFact(ctx, factID)
	if err != nil {
		t.Fatalf("GetFact error = %v", err)
	}
	if fact.SourceTier != TierAgentDerived {
		t.Fatalf("source_tier = %q, want %q", fact.SourceTier, TierAgentDerived)
	}
	if !almostEqual(fact.Confidence, CapLogOddsAgentDerived) {
		t.Errorf("confidence = %v, want %v (the agent_derived cap)", fact.Confidence, CapLogOddsAgentDerived)
	}
	if InjectionEligible(fact) {
		t.Fatal("agent_derived fact must be excluded from injection before confirmation")
	}

	if err := e.ConfirmFact(ctx, "trace", factID); err != nil {
		t.Fatalf("ConfirmFact error = %v", err)
	}
	confirmed, err := e.GetFact(ctx, factID)
	if err != nil {
		t.Fatalf("GetFact(after confirm) error = %v", err)
	}
	if !InjectionEligible(confirmed) {
		t.Fatal("confirmed agent_derived fact must be injection-eligible")
	}
	if confirmed.Confidence > CapLogOddsAgentDerived+floatTolerance {
		t.Errorf("confidence after confirm = %v, exceeds the 0.4 tier cap %v", confirmed.Confidence, CapLogOddsAgentDerived)
	}
	if confirmed.SourceTier != TierAgentDerived {
		t.Errorf("confirm must never change source_tier; got %q", confirmed.SourceTier)
	}
}

// TestAgentDerivedConfidenceNeverExceedsCapAcrossMultipleEvidence proves
// the cap holds even after several separate-session agent_derived
// evidence events pile up on the same fact (no ratchet above the tier
// ceiling, HANDOFF S5 memory #1/#3).
func TestAgentDerivedConfidenceNeverExceedsCapAcrossMultipleEvidence(t *testing.T) {
	e, st := newTestEngine(t)
	ctx := context.Background()

	var factID int64
	for i, sess := range []string{"sess-a", "sess-b", "sess-c"} {
		insertCleanSession(t, st, sess+"-unused") // unused clean session per iteration, keeps helper exercised
		id, err := e.WriteFact(ctx, "trace", Candidate{
			Subject: "episode:9", Predicate: "contains_number", Object: "42 TL",
			SessionID: sess, ExtractorVer: "hotwindow-v1",
		})
		if err != nil {
			t.Fatalf("WriteFact iteration %d error = %v", i, err)
		}
		factID = id
		fact, err := e.GetFact(ctx, factID)
		if err != nil {
			t.Fatalf("GetFact iteration %d error = %v", i, err)
		}
		if fact.Confidence > CapLogOddsAgentDerived+floatTolerance {
			t.Fatalf("iteration %d: confidence %v exceeds cap %v", i, fact.Confidence, CapLogOddsAgentDerived)
		}
	}
}

// TestSameSessionDedupeThenDifferentSessionRaisesLogOdds is the
// acceptance criterion: two same-session assertions of one fact produce
// exactly one evidence row; a third from a different session produces a
// second row and raises log-odds (no noisy-OR ratchet - exact expected
// values asserted).
func TestSameSessionDedupeThenDifferentSessionRaisesLogOdds(t *testing.T) {
	e, st := newTestEngine(t)
	ctx := context.Background()
	sessionA := "session-a"
	sessionB := "session-b"
	insertCleanSession(t, st, sessionB)

	base := Candidate{
		Subject: "user", Predicate: "employer", Object: "Acme",
		ExtractorVer: "hotwindow-v1", SessionID: sessionA,
	}

	factID, err := e.WriteFact(ctx, "trace-1", base)
	if err != nil {
		t.Fatalf("WriteFact(1st, session A) error = %v", err)
	}
	if _, err := e.WriteFact(ctx, "trace-2", base); err != nil {
		t.Fatalf("WriteFact(2nd, session A repeat) error = %v", err)
	}
	if got := countEvidenceRows(t, st, factID); got != 1 {
		t.Fatalf("evidence rows after two SAME-session assertions = %d, want 1", got)
	}
	afterDupe, err := e.GetFact(ctx, factID)
	if err != nil {
		t.Fatalf("GetFact error = %v", err)
	}
	if !almostEqual(afterDupe.Confidence, CapLogOddsAgentDerived) {
		t.Fatalf("confidence after same-session dedupe = %v, want %v", afterDupe.Confidence, CapLogOddsAgentDerived)
	}

	third := base
	third.SessionID = sessionB
	third.Provenance = ProvenanceExternalDoc
	if _, err := e.WriteFact(ctx, "trace-3", third); err != nil {
		t.Fatalf("WriteFact(3rd, session B) error = %v", err)
	}
	if got := countEvidenceRows(t, st, factID); got != 2 {
		t.Fatalf("evidence rows after a DIFFERENT-session assertion = %d, want 2", got)
	}

	afterThird, err := e.GetFact(ctx, factID)
	if err != nil {
		t.Fatalf("GetFact error = %v", err)
	}
	wantConfidence := CapLogOddsAgentDerived + CapLogOddsExternalDoc // sum, no noisy-OR
	if !almostEqual(afterThird.Confidence, wantConfidence) {
		t.Errorf("confidence after 3rd (different-session, higher-tier) evidence = %v, want exactly %v", afterThird.Confidence, wantConfidence)
	}
	if afterThird.Confidence <= afterDupe.Confidence {
		t.Error("log-odds must have RISEN after the different-session, higher-tier evidence")
	}
}

// TestUserDenialDropsFactBelowInjectionThreshold is the acceptance
// criterion: a user denial drops a p~=0.8 fact below 0.3 => excluded from
// injection; ledger event recorded.
func TestUserDenialDropsFactBelowInjectionThreshold(t *testing.T) {
	e, st := newTestEngine(t)
	ctx := context.Background()
	sessionID := "session-doc"
	insertCleanSession(t, st, sessionID)

	factID, err := e.WriteFact(ctx, "trace", Candidate{
		Subject: "user", Predicate: "lives_in", Object: "Istanbul",
		Provenance: ProvenanceExternalDoc, SessionID: sessionID,
		ExtractorVer: "reader-v1",
	})
	if err != nil {
		t.Fatalf("WriteFact error = %v", err)
	}
	before, err := e.GetFact(ctx, factID)
	if err != nil {
		t.Fatalf("GetFact(before) error = %v", err)
	}
	if !almostEqual(before.Confidence, CapLogOddsExternalDoc) {
		t.Fatalf("seed confidence = %v, want %v (p~=0.8)", before.Confidence, CapLogOddsExternalDoc)
	}
	if !InjectionEligible(before) {
		t.Fatal("seed fact should be injection-eligible")
	}

	beforeEventCount := countEventsOfKind(t, st, EventFactDenied)
	if err := e.DenyFact(ctx, "trace-deny", factID, "session-denier"); err != nil {
		t.Fatalf("DenyFact error = %v", err)
	}

	after, err := e.GetFact(ctx, factID)
	if err != nil {
		t.Fatalf("GetFact(after) error = %v", err)
	}
	wantConfidence := CapLogOddsExternalDoc + DenialLogOdds
	if !almostEqual(after.Confidence, wantConfidence) {
		t.Errorf("confidence after denial = %v, want %v", after.Confidence, wantConfidence)
	}
	if after.Confidence >= InjectionThresholdLogOdds {
		t.Errorf("confidence after denial = %v, want below threshold %v", after.Confidence, InjectionThresholdLogOdds)
	}
	if InjectionEligible(after) {
		t.Fatal("denied fact must be excluded from injection")
	}
	if got := countEventsOfKind(t, st, EventFactDenied); got != beforeEventCount+1 {
		t.Errorf("EventFactDenied ledger count = %d, want %d", got, beforeEventCount+1)
	}
}

// TestExtractorClaimedUserAssertedStoredAsAgentDerivedAndLedgered is the
// acceptance criterion: an extractor candidate struct claiming
// source_tier=user_asserted is stored as agent_derived (quarantined,
// excluded from injection) and the clamping is ledgered - the model
// cannot mint trust.
func TestExtractorClaimedUserAssertedStoredAsAgentDerivedAndLedgered(t *testing.T) {
	e, st := newTestEngine(t)
	ctx := context.Background()

	before := countEventsOfKind(t, st, EventTierClamped)

	factID, err := e.WriteFact(ctx, "trace", Candidate{
		Subject: "user", Predicate: "password_hint", Object: "kırmızı balık",
		// The extractor's OWN struct claims user_asserted - and provides
		// NO Provenance/SessionID at all, i.e. exactly what a prompt-
		// injected extractor free to set any field it likes would try.
		ClaimedSourceTier: TierUserAsserted,
		ExtractorVer:      "malicious-extractor-v1",
	})
	if err != nil {
		t.Fatalf("WriteFact error = %v", err)
	}

	fact, err := e.GetFact(ctx, factID)
	if err != nil {
		t.Fatalf("GetFact error = %v", err)
	}
	if fact.SourceTier != TierAgentDerived {
		t.Fatalf("source_tier = %q, want %q (the model cannot mint trust)", fact.SourceTier, TierAgentDerived)
	}
	if InjectionEligible(fact) {
		t.Fatal("clamped fact must stay quarantined (agent_derived, unconfirmed)")
	}
	if got := countEventsOfKind(t, st, EventTierClamped); got != before+1 {
		t.Errorf("EventTierClamped ledger count = %d, want %d", got, before+1)
	}
}

// TestUserAssertedRequiresCleanTaintSession is the acceptance criterion:
// a candidate marked direct-user-utterance from a session with taint
// tier untrusted (or no W4-03 taint record at all) is NOT stored as
// user_asserted (fail-closed).
func TestUserAssertedRequiresCleanTaintSession(t *testing.T) {
	e, st := newTestEngine(t)
	ctx := context.Background()

	taintedSession := "tainted-session"
	insertTaintedSession(t, st, taintedSession, "untrusted_output:web_fetch")

	noRecordSession := "no-record-session" // never inserted anywhere at all

	cases := []struct {
		name      string
		sessionID string
	}{
		{"tainted_session", taintedSession},
		{"no_taint_record_at_all", noRecordSession},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			factID, err := e.WriteFact(ctx, "trace", Candidate{
				Subject: "user-" + tc.name, Predicate: "claims", Object: "direct utterance",
				Provenance: ProvenanceUserAsserted, SessionID: tc.sessionID,
				ExtractorVer: "reader-v1",
			})
			if err != nil {
				t.Fatalf("WriteFact error = %v", err)
			}
			fact, err := e.GetFact(ctx, factID)
			if err != nil {
				t.Fatalf("GetFact error = %v", err)
			}
			if fact.SourceTier != TierAgentDerived {
				t.Errorf("source_tier = %q, want %q (fail-closed)", fact.SourceTier, TierAgentDerived)
			}
		})
	}
}

// TestUserAssertedSucceedsWithCleanSession is the positive-path
// counterpart: a clean session's direct-user-utterance candidate DOES
// become user_asserted.
func TestUserAssertedSucceedsWithCleanSession(t *testing.T) {
	e, st := newTestEngine(t)
	ctx := context.Background()
	sessionID := "clean-session"
	insertCleanSession(t, st, sessionID)

	factID, err := e.WriteFact(ctx, "trace", Candidate{
		Subject: "user", Predicate: "birthday", Object: "1990-05-01",
		Provenance: ProvenanceUserAsserted, SessionID: sessionID,
		ExtractorVer: "user_direct_v1",
	})
	if err != nil {
		t.Fatalf("WriteFact error = %v", err)
	}
	fact, err := e.GetFact(ctx, factID)
	if err != nil {
		t.Fatalf("GetFact error = %v", err)
	}
	if fact.SourceTier != TierUserAsserted {
		t.Errorf("source_tier = %q, want %q", fact.SourceTier, TierUserAsserted)
	}
	if !almostEqual(fact.Confidence, CapLogOddsUserAsserted) {
		t.Errorf("confidence = %v, want %v", fact.Confidence, CapLogOddsUserAsserted)
	}
}

// TestSecretLaneCandidateExtractionRejectedFailClosed is the acceptance
// criterion: a secret-lane candidate whose extraction would require the
// cloud model is rejected fail-closed with the Turkish notice, never
// proxied to Anthropic (asserted here via the SAME forward-proxy backstop
// ledger event kahyad/internal/secretlane's own proxy chokepoint writes).
func TestSecretLaneCandidateExtractionRejectedFailClosed(t *testing.T) {
	ctx := context.Background()
	classifier := secretlane.NewClassifier(nil) // deterministic pre-pass alone is enough for this fixture

	secretText := "IBAN'ım TR330006100519786457841326, lütfen bu hesaba gönderin."
	err := GuardCloudExtraction(ctx, classifier, secretText)
	if err == nil {
		t.Fatal("GuardCloudExtraction(secret-lane text) = nil, want ErrSecretLaneCloudExtraction")
	}
	if err.Error() != secretlane.MsgSecretLaneCloudBlocked {
		t.Errorf("GuardCloudExtraction error = %q, want the Turkish notice %q", err.Error(), secretlane.MsgSecretLaneCloudBlocked)
	}

	// A caller that respects this guard never even builds a cloud
	// request - simulate that contract directly: cloudCalls stays 0.
	cloudCalls := 0
	extractViaCloud := func(text string) {
		if guardErr := GuardCloudExtraction(ctx, classifier, text); guardErr != nil {
			return // MUST refuse, never call the cloud model
		}
		cloudCalls++
	}
	extractViaCloud(secretText)
	if cloudCalls != 0 {
		t.Fatal("secret-lane text must never reach a cloud extraction call")
	}

	// Independently, prove the SAME backstop kahyad/internal/secretlane's
	// real forward-proxy chokepoint uses would ALSO 403+ledger a task
	// whose lane is already secret, regardless of this guard - a second,
	// unbypassable line of defense at the actual egress point.
	fakeLedger := &fakeSecretLaneLedger{}
	fakeLookup := fakeLaneLookup{lane: secretlane.LaneSecret}
	hookFactory := secretlane.NewProxyBackstopHook(fakeLookup, fakeLedger)
	hook := hookFactory("task-1", "trace-1")
	req, err := http.NewRequest(http.MethodPost, "http://127.0.0.1/v1/messages", nil)
	if err != nil {
		t.Fatalf("build fake proxy request: %v", err)
	}
	if err := hook(req); err == nil {
		t.Fatal("proxy backstop hook did not block a secret-lane task")
	}
	if len(fakeLedger.events) != 1 || fakeLedger.events[0] != secretlane.EventSecretLaneCloudBlocked {
		t.Errorf("proxy backstop ledger events = %v, want exactly one %q", fakeLedger.events, secretlane.EventSecretLaneCloudBlocked)
	}

	// A non-secret candidate must NOT be blocked - once classification
	// actually completes (here, a fake local Qwen standing in for the
	// real one; classifier above intentionally has NO Qwen wired at all,
	// which correctly fails closed on anything the deterministic pre-pass
	// doesn't itself resolve - see Classifier.Classify's own doc comment
	// - so this assertion needs its own classifier with the local model
	// leg present).
	ordinaryQwen := secretlane.QwenClassifierFunc(func(context.Context, string) (secretlane.Verdict, error) {
		return secretlane.Verdict{SecretLane: false, Category: secretlane.CategoryNone}, nil
	})
	withLocalModel := secretlane.NewClassifier(ordinaryQwen)
	if err := GuardCloudExtraction(ctx, withLocalModel, "Yarın toplantı saat kaçta?"); err != nil {
		t.Errorf("GuardCloudExtraction(ordinary text) error = %v, want nil", err)
	}
}

type fakeSecretLaneLedger struct {
	events []string
}

func (f *fakeSecretLaneLedger) LogEvent(_ context.Context, _ string, kind string, _ map[string]any) error {
	f.events = append(f.events, kind)
	return nil
}

type fakeLaneLookup struct {
	lane string
}

func (f fakeLaneLookup) GetTaskLane(_ context.Context, _ string) (lane, category string, found bool, err error) {
	return f.lane, "finans", true, nil
}
