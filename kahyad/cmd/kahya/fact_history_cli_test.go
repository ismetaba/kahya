package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"kahya/mcp/memory"
)

// fact_history_cli_test.go covers FIX-FINAL fact-history-ended-vs-corrected
// (owner decision 2026-10-16) in the CLI: `kahya fact retract` is a
// correction by default and --sona-erdi records an ending; `kahya fact
// query --as-of` marks an ended fact ", sona erdi".

// retractFake answers POST /v1/fact/retract and records each request body.
type retractFake struct {
	// answerKind is the kind the fake reports as recorded ("" = echo the
	// request's kind).
	answerKind string
	bodies     []map[string]any
}

func (f *retractFake) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/fact/retract", func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{} // a fresh map per request (no merged keys)
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.bodies = append(f.bodies, body)
		kind := f.answerKind
		if kind == "" {
			kind, _ = body["kind"].(string)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "id": 9, "kind": kind})
	})
	return mux
}

func runRetract(t *testing.T, fake *retractFake, args ...string) (int, string, string) {
	t.Helper()
	t.Setenv("KAHYA_SOCKET", startFakeServer(t, fake.handler()))
	var stdout, stderr bytes.Buffer
	code := run(append([]string{"fact", "retract"}, args...), strings.NewReader(""), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// TestFactRetractDefaultIsACorrection: no flag sends kind "corrected" and
// keeps the W5-04 success line byte-exact.
func TestFactRetractDefaultIsACorrection(t *testing.T) {
	fake := &retractFake{}
	code, out, errOut := runRetract(t, fake, "user", "likes", "kahve", "oturum-1")
	if code != 0 || out != "Olgu geri çekildi: 9\n" {
		t.Fatalf("exit %d stdout %q stderr %q", code, out, errOut)
	}
	b := fake.bodies[0]
	if b["kind"] != "corrected" || b["subject"] != "user" || b["object"] != "kahve" || b["session_id"] != "oturum-1" {
		t.Fatalf("request = %v, want kind corrected and the triple + session", b)
	}
}

// TestFactRetractSonaErdiFlagRecordsAnEnding: --sona-erdi anywhere among
// the positional arguments sends kind "ended" and prints the Turkish
// ending line; an object that only starts with "-" stays positional.
func TestFactRetractSonaErdiFlagRecordsAnEnding(t *testing.T) {
	for _, args := range [][]string{
		{"--sona-erdi", "Emre", "works_at", "Acme"},
		{"Emre", "works_at", "Acme", "--sona-erdi"},
		{"Emre", "-sona-erdi", "works_at", "Acme"},
	} {
		fake := &retractFake{}
		code, out, errOut := runRetract(t, fake, args...)
		if code != 0 || out != "Olgu sona erdi olarak kapatıldı: 9\n" {
			t.Fatalf("args %v: exit %d stdout %q stderr %q", args, code, out, errOut)
		}
		if b := fake.bodies[0]; b["kind"] != "ended" || b["subject"] != "Emre" || b["predicate"] != "works_at" || b["object"] != "Acme" || b["session_id"] != "" {
			t.Fatalf("args %v: request = %v", args, b)
		}
	}
	fake := &retractFake{}
	if code, _, _ := runRetract(t, fake, "--sona-erdi", "hesap", "bakiye", "-5"); code != 0 || fake.bodies[0]["object"] != "-5" {
		t.Fatalf("a dash-leading object: exit %d, request %v", code, fake.bodies)
	}
	fake = &retractFake{}
	if code, _, _ := runRetract(t, fake, "--", "user", "likes", "--sona-erdi"); code != 0 || fake.bodies[0]["object"] != "--sona-erdi" || fake.bodies[0]["kind"] != "corrected" {
		t.Fatalf("after --: exit %d, request %v (the flag token is positional there)", code, fake.bodies)
	}
}

// TestFactRetractSonaErdiOnADeniedFactSaysItWasRecordedAsACorrection: when
// kahyad records a correction instead (the user had denied the fact), the
// CLI says so rather than claim the history.
func TestFactRetractSonaErdiOnADeniedFactSaysItWasRecordedAsACorrection(t *testing.T) {
	code, out, _ := runRetract(t, &retractFake{answerKind: "corrected"}, "--sona-erdi", "user", "likes", "kahve")
	if code != 0 || out != "Olgu daha önce reddedildiği için düzeltme olarak kapatıldı (geçmişte gösterilmez): 9\n" {
		t.Fatalf("exit %d stdout %q", code, out)
	}
}

// TestFactRetractUsageAndHelpAreTurkish: wrong arity prints the retract
// usage (exit 2, nothing sent); -h prints it with the --sona-erdi help.
func TestFactRetractUsageAndHelpAreTurkish(t *testing.T) {
	fake := &retractFake{}
	code, out, errOut := runRetract(t, fake, "--sona-erdi", "user", "likes")
	if code != 2 || out != "" || !strings.Contains(errOut, MsgFactRetractUsage) || len(fake.bodies) != 0 {
		t.Fatalf("two positionals: exit %d stdout %q stderr %q requests %d", code, out, errOut, len(fake.bodies))
	}
	help := runHelp(t, "fact", "retract", "-h")
	for _, want := range []string{MsgFactRetractUsage, "-sona-erdi", MsgFactRetractEndedHelp} {
		if !strings.Contains(help, want) {
			t.Errorf("kahya fact retract -h lacks %q:\n%s", want, help)
		}
	}
	if MsgFactRetractEndedHelp != "olgu doğruydu ama artık geçerli değil: sona erdi olarak kapat (geçmiş sorgularında --as-of / --known-as-of ile görünür); bayraksız geri çekme yanlış kaydın düzeltilmesidir ve olgu bir daha hiç gösterilmez" {
		t.Errorf("MsgFactRetractEndedHelp = %q", MsgFactRetractEndedHelp)
	}
}

// TestFactQueryCLIMarksEndedFacts: an ended fact's row ends ", sona erdi"
// (after ", onaysız" when both apply).
func TestFactQueryCLIMarksEndedFacts(t *testing.T) {
	fake := &factQueryFake{facts: []map[string]any{
		{"fact_id": 21, "subject": "Emre", "predicate": "works_at", "object": "Acme", "source_tier": "external_doc",
			"confidence_p": 0.18, "valid_from": "2026-01-01T00:00:00Z", "valid_to": "2026-08-01T00:00:00Z", "ended": true, "retracted_at": "2026-08-01T00:00:00Z"},
		{"fact_id": 22, "subject": "Emre", "predicate": "lives_in", "object": "Moda", "source_tier": "agent_derived",
			"confidence_p": 0.03, "valid_to": "2026-08-01T10:00:00Z", "ended": true, "quarantined": true},
		{"fact_id": 23, "subject": "Emre", "predicate": "works_at", "object": "Beta", "source_tier": "user_asserted",
			"confidence_p": 0.95},
	}}
	t.Setenv("KAHYA_SOCKET", startFakeServer(t, fake.handler()))
	var stdout, stderr bytes.Buffer
	if code := run([]string{"fact", "query", "--subject", "Emre", "--as-of", "2026-06-01T00:00:00Z", "--quarantined"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d (stderr %s)", code, stderr.String())
	}
	want := "Olgular (3):\n" +
		"#21  Emre | works_at | Acme  (p=0.18, external_doc, 2026-01-01→2026-08-01, sona erdi)\n" +
		"#22  Emre | lives_in | Moda  (p=0.03, agent_derived, →2026-08-01T10:00:00Z, onaysız, sona erdi)\n" +
		"#23  Emre | works_at | Beta  (p=0.95, user_asserted)\n"
	if stdout.String() != want {
		t.Fatalf("stdout =\n%q\nwant\n%q", stdout.String(), want)
	}
}

// TestFactEndedSuffixMatchesMCPLabel: the CLI suffix and the fact_query
// tool's label are one text.
func TestFactEndedSuffixMatchesMCPLabel(t *testing.T) {
	got := map[string]string{"cli": MsgFactEndedSuffix, "mcp": ", " + memory.FactEndedLabel}
	for k, v := range got {
		if v != ", sona erdi" {
			t.Fatalf("%s copy = %q, want \", sona erdi\" (cli %q, mcp label %q)", k, v, MsgFactEndedSuffix, memory.FactEndedLabel)
		}
	}
}
