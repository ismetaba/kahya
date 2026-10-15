// fact_review_cli_test.go covers the G13 MEM-5 human review commands:
// `kahya fact list`, bulk `kahya fact confirm` and `kahya fact deny`, over a
// fixed-fixture fake kahyad (the factengine_cli_test.go pattern).
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// factReviewFake records what each route was asked and answers fixtures.
type factReviewFake struct {
	listQuery   string
	confirmBody map[string]any
	denyBody    map[string]any
	listRows    []map[string]any
	truncated   bool
}

func (f *factReviewFake) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/fact/list", func(w http.ResponseWriter, r *http.Request) {
		f.listQuery = r.URL.RawQuery
		json.NewEncoder(w).Encode(map[string]any{"facts": f.listRows, "truncated": f.truncated})
	})
	mux.HandleFunc("/v1/fact/confirm", func(w http.ResponseWriter, r *http.Request) {
		f.confirmBody = map[string]any{}
		json.NewDecoder(r.Body).Decode(&f.confirmBody)
		if id, ok := f.confirmBody["fact_id"]; ok {
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "id": id})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "confirmed": []int64{12, 13}, "failed": map[string]string{"14": "factengine: fact not found: 14"}})
	})
	mux.HandleFunc("/v1/fact/deny", func(w http.ResponseWriter, r *http.Request) {
		f.denyBody = map[string]any{}
		json.NewDecoder(r.Body).Decode(&f.denyBody)
		if id, ok := f.denyBody["fact_id"]; ok {
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "id": id})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "denied": []int64{15, 16}, "failed": map[string]string{}})
	})
	return mux
}

func runFactCLI(t *testing.T, fake *factReviewFake, args ...string) (int, string, string) {
	t.Helper()
	t.Setenv("KAHYA_SOCKET", startFakeServer(t, fake.handler()))
	var stdout, stderr bytes.Buffer
	code := run(args, strings.NewReader(""), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestFactListPrintsQuarantinedRows(t *testing.T) {
	fake := &factReviewFake{listRows: []map[string]any{
		{"fact_id": 12, "subject": "Emre", "predicate": "works_at", "object": "Acme", "source_tier": "agent_derived", "confidence_p": 0.4, "quarantined": true, "evidence_path": "inbox/2026-10-08.md"},
		{"fact_id": 9, "subject": "Ayla", "predicate": "lives_in", "object": "Moda", "source_tier": "agent_derived", "confidence_p": 0.4, "quarantined": true},
	}}
	code, out, errOut := runFactCLI(t, fake, "fact", "list", "--quarantined", "--entity", "Emre", "--since", "2026-10-01T00:00:00Z")
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errOut)
	}
	want := "Olgular (2):\n" +
		"#12  Emre | works_at | Acme  (p=0.40, agent_derived, onaysız)  ← inbox/2026-10-08.md\n" +
		"#9  Ayla | lives_in | Moda  (p=0.40, agent_derived, onaysız)  ← -\n"
	if out != want {
		t.Fatalf("stdout =\n%q\nwant\n%q", out, want)
	}
	for _, p := range []string{"quarantined=1", "entity=Emre", "since=2026-10-01T00%3A00%3A00Z"} {
		if !strings.Contains(fake.listQuery, p) {
			t.Errorf("query %q lacks %q", fake.listQuery, p)
		}
	}
	fake.listRows = nil
	if code, out, _ := runFactCLI(t, fake, "fact", "list"); code != 0 || out != MsgFactListNone+"\n" {
		t.Fatalf("empty list = %d %q, want %q", code, out, MsgFactListNone)
	}
}

func TestFactConfirmManyPrintsCount(t *testing.T) {
	fake := &factReviewFake{}
	code, out, errOut := runFactCLI(t, fake, "fact", "confirm", "12", "13", "14")
	if code != 1 || out != "2 olgu onaylandı.\n" {
		t.Fatalf("confirm 12 13 14 = %d %q, want exit 1 (one id failed) and %q", code, out, "2 olgu onaylandı.")
	}
	if errOut != "Olgu #14 işlenemedi: factengine: fact not found: 14\n" {
		t.Fatalf("stderr = %q", errOut)
	}
	ids, _ := fake.confirmBody["fact_ids"].([]any)
	if len(ids) != 3 {
		t.Fatalf("body = %v, want fact_ids [12 13 14]", fake.confirmBody)
	}
	if code, out, _ := runFactCLI(t, fake, "fact", "confirm", "--episode", "7"); code != 1 || out != "2 olgu onaylandı.\n" || fake.confirmBody["episode_id"] != float64(7) {
		t.Fatalf("confirm --episode 7 = %d %q body %v", code, out, fake.confirmBody)
	}
	if code, out, _ := runFactCLI(t, fake, "fact", "confirm", "42"); code != 0 || out != "Olgu onaylandı: 42\n" || fake.confirmBody["fact_id"] != float64(42) {
		t.Fatalf("confirm 42 = %d %q body %v, want the W5-04 single-id line", code, out, fake.confirmBody)
	}
}

func TestFactConfirmAllQuarantinedWithoutSinceIsUsageError(t *testing.T) {
	fake := &factReviewFake{}
	code, out, errOut := runFactCLI(t, fake, "fact", "confirm", "--all-quarantined")
	if code != 2 || out != "" || errOut != MsgFactConfirmAllNeedsSince+"\n" {
		t.Fatalf("--all-quarantined = %d %q %q, want exit 2 and %q", code, out, errOut, MsgFactConfirmAllNeedsSince)
	}
	if fake.confirmBody != nil || fake.listQuery != "" {
		t.Fatal("nothing may reach kahyad without --since")
	}
	if MsgFactConfirmAllNeedsSince != "--all-quarantined için --since <RFC3339> zorunlu (yanlışlıkla her şeyi onaylamayı önler)." {
		t.Fatalf("MsgFactConfirmAllNeedsSince drifted from the handbook text: %q", MsgFactConfirmAllNeedsSince)
	}
}

// TestFactConfirmAllQuarantinedListsThenNeedsYes: without --yes the window
// is only listed with the prompt (nothing confirmed); with --yes the same
// bound is confirmed.
func TestFactConfirmAllQuarantinedListsThenNeedsYes(t *testing.T) {
	fake := &factReviewFake{listRows: []map[string]any{
		{"fact_id": 12, "subject": "Emre", "predicate": "works_at", "object": "Acme", "source_tier": "agent_derived", "confidence_p": 0.4, "quarantined": true, "evidence_path": "inbox/a.md"},
	}}
	code, out, _ := runFactCLI(t, fake, "fact", "confirm", "--all-quarantined", "--since", "2026-10-01T00:00:00Z")
	if code != 0 || !strings.HasSuffix(out, "1 olgu onaylanacak; devam için komutu --yes ile tekrarla.\n") || !strings.HasPrefix(out, "Olgular (1):\n#12  Emre") {
		t.Fatalf("dry run = %d %q", code, out)
	}
	if fake.confirmBody != nil {
		t.Fatalf("dry run confirmed %v", fake.confirmBody)
	}
	code, out, _ = runFactCLI(t, fake, "fact", "confirm", "--all-quarantined", "--since", "2026-10-01T00:00:00Z", "--yes")
	if code != 1 || out != "2 olgu onaylandı.\n" || fake.confirmBody["all_quarantined_since"] != "2026-10-01T00:00:00Z" {
		t.Fatalf("--yes = %d %q body %v", code, out, fake.confirmBody)
	}
	fake.truncated = true
	fake.confirmBody = nil
	if code, _, errOut := runFactCLI(t, fake, "fact", "confirm", "--all-quarantined", "--since", "2000-01-01T00:00:00Z"); code != 2 || errOut != "Olgu onayı başarısız: en fazla 500 olgu\n" || fake.confirmBody != nil {
		t.Fatalf("over the cap = %d %q", code, errOut)
	}
}

func TestFactDenyPrintsTurkish(t *testing.T) {
	fake := &factReviewFake{}
	if code, out, _ := runFactCLI(t, fake, "fact", "deny", "15"); code != 0 || out != "Olgu reddedildi (güven düşürüldü): 15\n" {
		t.Fatalf("deny 15 = %d %q", code, out)
	}
	if code, out, _ := runFactCLI(t, fake, "fact", "deny", "15", "16"); code != 0 || out != "2 olgu reddedildi.\n" {
		t.Fatalf("deny 15 16 = %d %q", code, out)
	}
	for _, args := range [][]string{{"fact", "deny"}, {"fact", "deny", "x"}, {"fact", "confirm", "12", "--episode", "7"}, {"fact", "confirm", "12", "--yes"}} {
		if code, _, errOut := runFactCLI(t, fake, args...); code != 2 || !strings.Contains(errOut, MsgFactUsage) {
			t.Errorf("%v = %d %q, want usage", args, code, errOut)
		}
	}
}
