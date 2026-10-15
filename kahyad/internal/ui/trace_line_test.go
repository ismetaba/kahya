package ui

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type capturingRemote struct {
	mu   sync.Mutex
	text []string
}

func (c *capturingRemote) SendNotification(_ context.Context, _, text string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.text = append(c.text, text)
	return true
}

// TestLocalNotificationDropsBriefingTraceLine (FIX-FINAL TUX-10): the
// briefing text carries "trace_id: <id>" for Telegram (W5-01); locally
// kahyaNotify already appends "iz: <id>", so the banner showed the id twice
// and Yelda read 32 hex characters aloud every morning. The local banner
// and the speech drop that line; Telegram still gets the text unchanged.
func TestLocalNotificationDropsBriefingTraceLine(t *testing.T) {
	const trace = "0123456789abcdef0123456789abcdef"
	text := "Günaydın — sabah brifingi\ntrace_id: " + trace + "\n\n- Bugün 2 görev var\n"
	speaker, _ := newTestSpeaker(t, yeldaVoiceList)
	stdinLog := filepath.Join(t.TempDir(), "stdin.log")
	t.Setenv("FAKE_SAY_STDIN_LOG", stdinLog)
	var mu sync.Mutex
	var luaArgs []string
	local := NewForTest("/fake/hs", nil, func(_ context.Context, _ string, args ...string) error {
		mu.Lock()
		defer mu.Unlock()
		luaArgs = append(luaArgs, args...)
		return nil
	})
	local.Speaker = speaker
	remote := &capturingRemote{}
	if !(FanOutDelivery{Primary: remote, Local: local}).SendNotification(context.Background(), trace, text) {
		t.Fatal("SendNotification = false")
	}
	if len(remote.text) != 1 || remote.text[0] != text {
		t.Fatalf("remote got %q, want the text unchanged", remote.text)
	}
	mu.Lock()
	var message string
	for _, a := range luaArgs {
		if i := strings.Index(a, `kahyaNotify("`); i >= 0 {
			b64 := strings.TrimSuffix(a[i+len(`kahyaNotify("`):], `")`)
			raw, err := base64.StdEncoding.DecodeString(b64)
			if err != nil {
				t.Fatal(err)
			}
			var p map[string]string
			if err := json.Unmarshal(raw, &p); err != nil {
				t.Fatal(err)
			}
			message = p["message"]
		}
	}
	mu.Unlock()
	if message == "" || strings.Contains(message, "trace_id:") || !strings.Contains(message, "Bugün 2 görev var") {
		t.Fatalf("local banner message = %q, want the briefing without its trace_id line", message)
	}
	if spoken := readFileString(t, stdinLog); strings.Contains(spoken, trace) || !strings.Contains(spoken, "sabah brifingi") {
		t.Fatalf("spoken text = %q, want the briefing without the trace id", spoken)
	}
}

func TestStripTraceLine(t *testing.T) {
	for _, tc := range []struct{ in, trace, want string }{
		{"a\ntrace_id: T\n\nb\n", "T", "a\nb\n"},
		{"a\ntrace_id: T\nb", "T", "a\nb"},
		{"a\ntrace_id: OTHER\nb", "T", "a\ntrace_id: OTHER\nb"},
		{"trace_id: T", "T", ""},
		{"x", "", "x"},
	} {
		if got := stripTraceLine(tc.in, tc.trace); got != tc.want {
			t.Errorf("stripTraceLine(%q, %q) = %q, want %q", tc.in, tc.trace, got, tc.want)
		}
	}
}
