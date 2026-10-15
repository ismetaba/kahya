package fs

import (
	"context"
	"errors"
	"testing"
)

// FIX-FINAL tool-output-secret-lane-unclassified: OutputGate is fs_read's
// FIX-B-6 seam for tools whose OUTPUT can carry file bytes.

func TestOutputGateContentHitMarksThenEscalates(t *testing.T) {
	marker, esc := &fakeSensitiveMarker{}, &fakeLaneEscalator{}
	g := OutputGate{Classifier: fakeContentClassifier{secret: true, category: "finans"}, Marker: marker, Escalator: esc}
	var kinds []string
	secret, err := g.Check(context.Background(), "tr1", "t1", "shell_host", false, func(k string, _ map[string]any) { kinds = append(kinds, k) }, "IBAN TR..", "")
	if err != nil || !secret {
		t.Fatalf("Check = %v %v, want secret, nil", secret, err)
	}
	if marker.callCount() != 1 {
		t.Errorf("mark calls = %d, want 1", marker.callCount())
	}
	if c := esc.snapshot(); len(c) != 1 || c[0] != (escalateCall{"t1", "tr1", "finans"}) {
		t.Errorf("escalations = %+v, want one for t1 with category finans", c)
	}
	if len(kinds) != 1 || kinds[0] != "secret_lane_read" {
		t.Errorf("ledger kinds = %v, want [secret_lane_read]", kinds)
	}
}

func TestOutputGatePathHitEscalatesWithoutClassifying(t *testing.T) {
	esc := &fakeLaneEscalator{}
	g := OutputGate{Classifier: fakeContentClassifier{secret: false}, Escalator: esc}
	if secret, err := g.Check(context.Background(), "tr1", "t1", "shell_docker", true, nil); err != nil || !secret {
		t.Fatalf("Check = %v %v, want secret", secret, err)
	}
	if c := esc.snapshot(); len(c) != 1 || c[0].category != "" {
		t.Errorf("escalations = %+v, want one with empty category", c)
	}
}

func TestOutputGateCleanOutputDoesNothing(t *testing.T) {
	marker, esc := &fakeSensitiveMarker{}, &fakeLaneEscalator{}
	g := OutputGate{Classifier: fakeContentClassifier{secret: false}, Marker: marker, Escalator: esc}
	if secret, err := g.Check(context.Background(), "tr1", "t1", "osascript", false, nil, "temiz"); err != nil || secret {
		t.Fatalf("Check = %v %v, want clean", secret, err)
	}
	if marker.callCount() != 0 || len(esc.snapshot()) != 0 {
		t.Error("a clean output was marked or escalated")
	}
}

// TestOutputGateRecordFailureRefuses: a hit that cannot be marked or
// escalated refuses with MsgSecretLaneOutputRefused (the caller returns
// no bytes) and ledgers the reason.
func TestOutputGateRecordFailureRefuses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		gate   OutputGate
		reason string
	}{
		{"mark", OutputGate{Classifier: fakeContentClassifier{secret: true}, Marker: &fakeSensitiveMarker{err: errors.New("db")}, Escalator: &fakeLaneEscalator{}}, "sensitive_mark_failed"},
		{"escalate", OutputGate{Classifier: fakeContentClassifier{secret: true}, Marker: &fakeSensitiveMarker{}, Escalator: &fakeLaneEscalator{err: errors.New("db")}}, "lane_escalate_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reason any
			_, err := tc.gate.Check(context.Background(), "tr1", "t1", "shell_host", false, func(k string, p map[string]any) {
				if k == "secret_lane_read_refused" {
					reason = p["reason"]
				}
			}, "x")
			if err == nil || err.Error() != MsgSecretLaneOutputRefused {
				t.Fatalf("err = %v, want %q", err, MsgSecretLaneOutputRefused)
			}
			if reason != tc.reason {
				t.Errorf("refused reason = %v, want %s", reason, tc.reason)
			}
		})
	}
}

func TestPathCoversSecretGlob(t *testing.T) {
	home := testHome(t)
	globs := []string{home + "/Documents/saglik/**", home + "/Kahya/memory/finans*.md", home + "/Kahya/memory/gizli/**"}
	for _, tc := range []struct {
		p    string
		want bool
	}{
		{home + "/Documents/saglik/tahlil.md", true}, // under a glob
		{home + "/Documents/saglik", true},           // the root itself
		{home + "/Documents", true},                  // holds the root
		{home + "/Kahya", true},                      // holds the memory dir
		{home + "/Kahya/memory", true},               // root of finans*.md
		{home + "/DOCUMENTS/Saglik/x", true},         // case-folded (APFS)
		{home + "/code/kahya", false},
		{home + "/Documents/is", false},
	} {
		if got := PathCoversSecretGlob(home, tc.p, globs); got != tc.want {
			t.Errorf("PathCoversSecretGlob(%s) = %v, want %v", tc.p, got, tc.want)
		}
	}
}
