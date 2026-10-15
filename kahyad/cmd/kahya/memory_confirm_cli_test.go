package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// runCLI runs the CLI against whatever KAHYA_SOCKET the test set.
func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, strings.NewReader(""), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// TestMemoryConfirmPrintsTurkish: `kahya memory confirm <yol>` posts the
// path and prints the handbook's byte-exact success line; a refusal is
// wrapped in the Turkish frame; no path is a usage error.
func TestMemoryConfirmPrintsTurkish(t *testing.T) {
	var got map[string]string
	refuse := false
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/memory/confirm", func(w http.ResponseWriter, r *http.Request) {
		got = map[string]string{}
		json.NewDecoder(r.Body).Decode(&got)
		if refuse {
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]any{"error": "memory_confirm: not zaten onaylı (user_asserted)"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "file": got["file"], "episode_id": 7, "commit_sha": "abc", "previous_tier": "agent_derived"})
	})
	t.Setenv("KAHYA_SOCKET", startFakeServer(t, mux))
	code, out, errOut := runCLI(t, "memory", "confirm", "inbox/2026-10-08.md")
	if code != 0 || out != "Not onaylandı (user_asserted katmanına yükseltildi): inbox/2026-10-08.md\n" || got["file"] != "inbox/2026-10-08.md" {
		t.Fatalf("confirm = %d %q %q body %v", code, out, errOut, got)
	}
	refuse = true
	if code, _, errOut := runCLI(t, "memory", "confirm", "inbox/2026-10-08.md"); code != 2 || errOut != "Not onaylanamadı: memory_confirm: not zaten onaylı (user_asserted)\n" {
		t.Fatalf("refused confirm = %d %q", code, errOut)
	}
	if code, _, errOut := runCLI(t, "memory", "confirm"); code != 2 || errOut != MsgMemoryUsage+"\n" {
		t.Fatalf("no path = %d %q", code, errOut)
	}
}
