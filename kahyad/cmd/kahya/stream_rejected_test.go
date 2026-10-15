package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStreamTaskJSONRefusalIsNotUnreachable (FIX-FINAL TUX-7): streamTask
// turned every non-SSE answer into "kahyad'a ulaşılamıyor ... make
// install-agent" - kahyad's own Turkish 403 control-secret refusal, a 503
// shutdown and the 409 halted-before-start answer included - while `kahya
// health` said kahyad was running. A JSON {"error"} refusal is now shown
// in a Turkish frame (exit 2, trace footer after it); a non-JSON answer
// stays "unreachable".
func TestStreamTaskJSONRefusalIsNotUnreachable(t *testing.T) {
	const secretRefused = "Kontrol anahtarı eksik ya da geçersiz (fail-closed). Bu uç nokta yalnızca kahya CLI, kahya-trigger ve Hammerspoon içindir."
	const halted = "⛔ Görev başlamadan durduruldu (user_halted)."
	for _, tc := range []struct {
		name   string
		status int
		msg    string
		want   string
	}{
		{"control_secret_403", http.StatusForbidden, secretRefused, fmt.Sprintf(MsgTaskRejected, secretRefused)},
		{"shutting_down_503", http.StatusServiceUnavailable, "kahyad is shutting down", MsgTaskDaemonRestarting},
		{"halted_before_start_409", http.StatusConflict, halted, fmt.Sprintf(MsgTaskRejected, halted)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KAHYA_SOCKET", startFakeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				json.NewEncoder(w).Encode(map[string]string{"error": tc.msg})
			})))
			var stdout, stderr bytes.Buffer
			code := run([]string{"merhaba"}, strings.NewReader(""), &stdout, &stderr)
			lines := strings.Split(strings.TrimSuffix(stderr.String(), "\n"), "\n")
			if code != 2 || len(lines) != 2 || lines[0] != tc.want || !strings.HasPrefix(lines[1], "iz: ") {
				t.Fatalf("exit=%d stderr=%q, want 2, %q then the iz: footer", code, stderr.String(), tc.want)
			}
			if strings.Contains(stderr.String(), "ulaşılamıyor") {
				t.Fatal("a kahyad refusal was reported as unreachable")
			}
		})
	}
}

// TestHaltReachesDaemonDespiteUnrelatedConfigError (FIX-FINAL ops-config-
// load-gates-halt-and-jobs): `kahya halt` and Hammerspoon's ⌥⎋ (no
// KAHYA_SOCKET) resolved the socket through config.Load, so a config.yaml
// timezone typo written while kahyad ran made the emergency stop exit 2
// without dialing the live daemon. Halt now resolves only the socket.
func TestHaltReachesDaemonDespiteUnrelatedConfigError(t *testing.T) {
	var halts int
	sock := startFakeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/halt" && r.Method == http.MethodPost {
			halts++
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "halted": 0})
	}))
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KAHYA_SOCKET", "")
	t.Setenv("KAHYA_ENV", "")
	dir := filepath.Join(home, "Library", "Application Support", "Kahya")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("socket: "+sock+"\ntimezone: Europe/Istanbull\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"halt"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || halts != 1 {
		t.Fatalf("kahya halt = %d (halts seen %d), stderr=%q; want 0 and one POST /halt", code, halts, stderr.String())
	}
}
