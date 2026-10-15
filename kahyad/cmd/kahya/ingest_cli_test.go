package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kahya/kahyad/internal/ingest"
)

// ingestFake records the POST /v1/ingest body and answers a fixture (or a
// refusal).
type ingestFake struct {
	body     map[string]any
	lane     string
	status   int
	errorRes map[string]any
}

func (f *ingestFake) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/ingest", func(w http.ResponseWriter, r *http.Request) {
		f.body = map[string]any{}
		json.NewDecoder(r.Body).Decode(&f.body)
		if f.status != 0 {
			w.WriteHeader(f.status)
			json.NewEncoder(w).Encode(f.errorRes)
			return
		}
		file := "ingest/2026-10-08-not.md"
		if f.lane == "secret" {
			file = "gizli/ingest/2026-10-08-not.md"
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "file": file, "episode_id": 3, "lane": f.lane,
			"chunks": 1, "facts": 2, "entities": 3, "source_tier": "external_doc"})
	})
	return mux
}

func runIngestCLI(t *testing.T, fake *ingestFake, stdin string, args ...string) (int, string, string) {
	t.Helper()
	t.Setenv("KAHYA_SOCKET", startFakeServer(t, fake.handler()))
	var stdout, stderr bytes.Buffer
	code := run(append([]string{"ingest"}, args...), strings.NewReader(stdin), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// TestIngestCLIReadsStdinAndPrintsTurkish: `echo ... | kahya ingest -
// --source not` sends the stdin bytes and the label, and prints the
// handbook's byte-exact lines: the done line and the quarantine hint.
func TestIngestCLIReadsStdinAndPrintsTurkish(t *testing.T) {
	fake := &ingestFake{lane: "normal"}
	doc := "Acme Ltd. 2026-05-01 tarihinde Kadıköy'e taşındı.\n"
	code, out, errOut := runIngestCLI(t, fake, doc, "-", "--source", "not")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, errOut)
	}
	want := "Belge alındı: ingest/2026-10-08-not.md (1 parça, 2 olgu, 3 varlık; katman: external_doc, şerit: normal)\n" +
		"Not ve olgular onaysız (external_doc); otomatik hatırlatmaya girmesi için: kahya memory confirm ingest/2026-10-08-not.md\n"
	if out != want {
		t.Fatalf("stdout =\n%q\nwant\n%q", out, want)
	}
	raw, _ := base64.StdEncoding.DecodeString(fake.body["content_b64"].(string))
	if string(raw) != doc || fake.body["source_ref"] != "not" || fake.body["lane_hint"] != nil || fake.body["no_raw"] != false {
		t.Fatalf("body = %v", fake.body)
	}

	// A file path: its absolute path is the source, --title / --no-raw pass
	// through, flags may come first.
	dir := t.TempDir()
	path := filepath.Join(dir, "rapor.md")
	if err := os.WriteFile(path, []byte("# Rapor\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := runIngestCLI(t, fake, "", "--title", "Rapor", "--no-raw", path); code != 0 {
		t.Fatalf("file ingest exit = %d %q", code, errOut)
	}
	if fake.body["source_ref"] != path || fake.body["title"] != "Rapor" || fake.body["no_raw"] != true {
		t.Fatalf("file body = %v", fake.body)
	}
}

// TestIngestCLISecretBadge: --secret asks for the local lane, and a
// secret-lane result prints the lock badge naming the note.
func TestIngestCLISecretBadge(t *testing.T) {
	fake := &ingestFake{lane: "secret"}
	code, out, _ := runIngestCLI(t, fake, "tahlil sonuçları\n", "-", "--secret")
	if code != 0 || fake.body["lane_hint"] != "secret" {
		t.Fatalf("exit = %d body = %v", code, fake.body)
	}
	for _, want := range []string{
		"Belge alındı: gizli/ingest/2026-10-08-not.md (1 parça, 2 olgu, 3 varlık; katman: external_doc, şerit: gizli)\n",
		"🔒 Belge gizli şerit olarak işlendi (yalnız yerel model); not: gizli/ingest/2026-10-08-not.md\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout lacks %q:\n%s", want, out)
		}
	}
}

// TestIngestCLIRefusals: the daemon's refusals print the handbook texts (a
// 413 with the cap, the no-local-model refusal) or the daemon's own Turkish
// text; a missing file and a missing path are local errors.
func TestIngestCLIRefusals(t *testing.T) {
	fake := &ingestFake{status: http.StatusRequestEntityTooLarge, errorRes: map[string]any{"error": "x", "reason": "too_large", "max_bytes": 2097152}}
	if code, _, errOut := runIngestCLI(t, fake, "x", "-"); code != 2 || errOut != "Dosya çok büyük (en fazla 2097152 bayt).\n" {
		t.Fatalf("too large = %d %q", code, errOut)
	}
	fake = &ingestFake{status: http.StatusServiceUnavailable, errorRes: map[string]any{"error": "x", "reason": "local_unavailable"}}
	if code, _, errOut := runIngestCLI(t, fake, "x", "-"); code != 2 || errOut != MsgIngestLocalUnavailable+"\n" {
		t.Fatalf("local unavailable = %d %q", code, errOut)
	}
	fake = &ingestFake{status: http.StatusConflict, errorRes: map[string]any{"error": "ingest: bu belge zaten alınmış: ingest/a.md", "reason": "duplicate"}}
	if code, _, errOut := runIngestCLI(t, fake, "x", "-"); code != 2 || errOut != "ingest: bu belge zaten alınmış: ingest/a.md\n" {
		t.Fatalf("duplicate = %d %q", code, errOut)
	}
	fake = &ingestFake{status: http.StatusServiceUnavailable, errorRes: map[string]any{"error": "ingest not available"}}
	if code, _, errOut := runIngestCLI(t, fake, "x", "-"); code != 2 || errOut != "Belge alınamadı: ingest not available\n" {
		t.Fatalf("unwired = %d %q", code, errOut)
	}
	if code, _, errOut := runIngestCLI(t, &ingestFake{}, "", filepath.Join(t.TempDir(), "yok.txt")); code != 2 || !strings.HasPrefix(errOut, "Belge okunamadı: ") {
		t.Fatalf("missing file = %d %q", code, errOut)
	}
	for _, args := range [][]string{{}, {"a.txt", "b.txt"}} {
		if code, _, errOut := runIngestCLI(t, &ingestFake{}, "", args...); code != 2 || errOut != MsgIngestUsage+"\n" {
			t.Fatalf("%v = %d %q, want usage", args, code, errOut)
		}
	}
}

// TestIngestCLIStringsMatchIngestPackage: the CLI's copies of the texts the
// daemon also produces stay byte-identical, and the usage line is the
// handbook's.
func TestIngestCLIStringsMatchIngestPackage(t *testing.T) {
	if MsgIngestLocalUnavailable != ingest.MsgIngestLocalUnavailable || MsgIngestTooLarge != ingest.MsgIngestTooLargeFmt {
		t.Fatal("kahya ingest texts drifted from kahyad/internal/ingest")
	}
	if MsgIngestUsage != "kullanım: kahya ingest <dosya|-> [--title <başlık>] [--source <url|etiket>] [--secret]" {
		t.Fatalf("MsgIngestUsage = %q", MsgIngestUsage)
	}
}
