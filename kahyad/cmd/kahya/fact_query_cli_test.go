package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// factQueryFake answers GET /v1/fact/query with fixed rows (or a 400) and
// records the query string it got.
type factQueryFake struct {
	got    url.Values
	facts  []map[string]any
	status int
	err    string
}

func (f *factQueryFake) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/fact/query", func(w http.ResponseWriter, r *http.Request) {
		f.got = r.URL.Query()
		if f.status != 0 {
			w.WriteHeader(f.status)
			json.NewEncoder(w).Encode(map[string]any{"error": f.err})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"facts": f.facts, "trace_id": "t"})
	})
	return mux
}

// TestFactQueryCLIAsOfPrintsRows (FIX-MEM-4 phase 4): `kahya fact query`
// sends every flag as a query parameter and prints the Turkish rows - the
// validity part and the ", onaysız" mark exactly as the handbook shows.
func TestFactQueryCLIAsOfPrintsRows(t *testing.T) {
	fake := &factQueryFake{facts: []map[string]any{
		{"fact_id": 12, "subject": "Emre", "predicate": "works_at", "object": "Acme", "source_tier": "user_asserted",
			"confidence_p": 0.8, "valid_from": "2026-05-01T00:00:00Z", "quarantined": true},
		{"fact_id": 13, "subject": "Emre", "predicate": "lives_in", "object": "Kadıköy", "source_tier": "external_doc",
			"confidence_p": 0.6666, "valid_to": "2026-06-01T12:00:00Z"},
		{"fact_id": 14, "subject": "Emre", "predicate": "likes", "object": "çay", "source_tier": "user_asserted",
			"confidence_p": 0.95},
	}}
	t.Setenv("KAHYA_SOCKET", startFakeServer(t, fake.handler()))

	var stdout, stderr bytes.Buffer
	code := run([]string{"fact", "query", "--subject", "Emre", "--as-of", "2026-05-15T00:00:00Z", "--known-as-of", "2026-05-01T00:00:00Z",
		"--entity", "Emre", "--predicate", "works_at", "--quarantined", "--limit", "5"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d (stderr %s)", code, stderr.String())
	}
	want := "Olgular (3):\n" +
		"#12  Emre | works_at | Acme  (p=0.80, user_asserted, 2026-05-01→, onaysız)\n" +
		"#13  Emre | lives_in | Kadıköy  (p=0.67, external_doc, →2026-06-01T12:00:00Z)\n" +
		"#14  Emre | likes | çay  (p=0.95, user_asserted)\n"
	if stdout.String() != want {
		t.Fatalf("stdout =\n%q\nwant\n%q", stdout.String(), want)
	}
	for key, val := range map[string]string{
		"subject": "Emre", "as_of": "2026-05-15T00:00:00Z", "known_as_of": "2026-05-01T00:00:00Z",
		"entity": "Emre", "predicate": "works_at", "quarantined": "1", "limit": "5",
	} {
		if fake.got.Get(key) != val {
			t.Errorf("query %s = %q, want %q (all: %v)", key, fake.got.Get(key), val, fake.got)
		}
	}
	if fake.got.Has("object") || fake.got.Has("since") {
		t.Errorf("unset flags were sent: %v", fake.got)
	}
}

func TestFactQueryCLINoneAndServerError(t *testing.T) {
	fake := &factQueryFake{}
	t.Setenv("KAHYA_SOCKET", startFakeServer(t, fake.handler()))
	var stdout, stderr bytes.Buffer
	if code := run([]string{"fact", "query", "--predicate", "owns"}, strings.NewReader(""), &stdout, &stderr); code != 0 || stdout.String() != "Eşleşen olgu yok.\n" {
		t.Fatalf("no rows: exit %d stdout %q", code, stdout.String())
	}

	fake.status, fake.err = http.StatusBadRequest, "as_of must be RFC3339"
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"fact", "query", "--as-of", "dün"}, strings.NewReader(""), &stdout, &stderr); code != 2 {
		t.Fatalf("server 400: exit %d, want 2", code)
	}
	if got := strings.TrimSpace(stderr.String()); got != "Olgu sorgusu başarısız: as_of must be RFC3339" {
		t.Fatalf("stderr = %q", got)
	}
	stderr.Reset()
	if code := run([]string{"fact", "query", "stray"}, strings.NewReader(""), &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), MsgFactUsage) {
		t.Fatalf("a positional argument: exit %d stderr %q", code, stderr.String())
	}
}
