package factengine

import (
	"context"
	"testing"
)

func TestDetectRetractionByteExactFixture(t *testing.T) {
	// HANDOFF S5 memory #3 / task spec byte-exact fixture: "Artık
	// sevmiyorum" -> geri-cekme.
	if !DetectRetraction("Kahveyi artık sevmiyorum.") {
		t.Fatal(`DetectRetraction("Kahveyi artık sevmiyorum.") = false, want true`)
	}
}

func TestDetectRetractionGeneralArtikDegilPattern(t *testing.T) {
	if !DetectRetraction("Artık onun arkadaşı değilim.") {
		t.Fatal("DetectRetraction on generic artik...degil pattern = false, want true")
	}
}

func TestDetectRetractionAsciiFoldedSpelling(t *testing.T) {
	if !DetectRetraction("Artik sevmiyorum bu yemegi.") {
		t.Fatal("DetectRetraction on ascii-folded spelling = false, want true")
	}
}

func TestDetectRetractionOrdinaryTextIsNotRetraction(t *testing.T) {
	if DetectRetraction("Kahveyi çok seviyorum.") {
		t.Fatal(`DetectRetraction("Kahveyi çok seviyorum.") = true, want false`)
	}
}

// TestRetractionFixtureClosesFactAndExcludesFromInjection is the
// acceptance-criterion test: seed "Kahveyi çok seviyorum." (fact: user
// likes coffee), then "Kahveyi artık sevmiyorum." => original fact
// status=retracted, valid_to set, negative evidence row, and the
// retracted fact no longer injects.
func TestRetractionFixtureClosesFactAndExcludesFromInjection(t *testing.T) {
	e, st := newTestEngine(t)
	ctx := context.Background()
	sessionID := "session-coffee"
	insertCleanSession(t, st, sessionID)

	factID, err := e.WriteFact(ctx, "trace-1", Candidate{
		Subject: "user", Predicate: "likes", Object: "kahve",
		Provenance: ProvenanceUserAsserted, SessionID: sessionID,
		Evidentiality: Witnessed, ExtractorVer: "user_direct_v1",
		Evidence: "quote:Kahveyi cok seviyorum.",
	})
	if err != nil {
		t.Fatalf("WriteFact(seed) error = %v", err)
	}

	before, err := e.GetFact(ctx, factID)
	if err != nil {
		t.Fatalf("GetFact(before) error = %v", err)
	}
	if !InjectionEligible(before) {
		t.Fatal("seeded fact should be injection-eligible before retraction")
	}

	quote := "Kahveyi artık sevmiyorum."
	if !DetectRetraction(quote) {
		t.Fatalf("DetectRetraction(%q) = false, want true", quote)
	}

	retractedID, err := e.RetractFact(ctx, "trace-2", "user", "likes", "kahve", sessionID)
	if err != nil {
		t.Fatalf("RetractFact error = %v", err)
	}
	if retractedID != factID {
		t.Fatalf("RetractFact returned fact %d, want %d", retractedID, factID)
	}

	after, err := e.GetFact(ctx, factID)
	if err != nil {
		t.Fatalf("GetFact(after) error = %v", err)
	}
	if after.Status != "retracted" {
		t.Errorf("after retraction status = %q, want %q", after.Status, "retracted")
	}
	if !after.ValidTo.Valid || after.ValidTo.String == "" {
		t.Error("after retraction valid_to is not set")
	}
	if InjectionEligible(after) {
		t.Error("retracted fact must not be injection-eligible")
	}

	if got := countEvidenceRows(t, st, factID); got != 2 {
		t.Errorf("evidence rows for fact %d = %d, want 2 (1 positive seed + 1 negative retraction)", factID, got)
	}

	var negativeRows int
	if err := st.DB().QueryRow(`SELECT count(*) FROM evidence WHERE fact_id = ? AND polarity = -1`, factID).Scan(&negativeRows); err != nil {
		t.Fatalf("count negative evidence rows: %v", err)
	}
	if negativeRows != 1 {
		t.Errorf("negative evidence rows for fact %d = %d, want 1", factID, negativeRows)
	}
}

func TestRetractFactNoActiveFactReturnsError(t *testing.T) {
	e, _ := newTestEngine(t)
	if _, err := e.RetractFact(context.Background(), "trace", "nobody", "likes", "nothing", "sess"); err != ErrNoActiveFact {
		t.Fatalf("RetractFact on nonexistent fact error = %v, want ErrNoActiveFact", err)
	}
}
