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

// bannerMessages decodes every kahyaNotify payload's "message" field the
// fake `hs` exec received.
func bannerMessages(t *testing.T, mu *sync.Mutex, luaArgs *[]string) []string {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	var out []string
	for _, a := range *luaArgs {
		i := strings.Index(a, `kahyaNotify("`)
		if i < 0 {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSuffix(a[i+len(`kahyaNotify("`):], `")`))
		if err != nil {
			t.Fatal(err)
		}
		var p map[string]string
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p["message"])
	}
	return out
}

// recordingHS returns an HSCli whose fake `hs` exec records its argv.
func recordingHS() (*HSCli, *sync.Mutex, *[]string) {
	var mu sync.Mutex
	var luaArgs []string
	h := NewForTest("/fake/hs", nil, func(_ context.Context, _ string, args ...string) error {
		mu.Lock()
		defer mu.Unlock()
		luaArgs = append(luaArgs, args...)
		return nil
	})
	return h, &mu, &luaArgs
}

// TestFanOutSendNoticeShowsTextAndSpeaksOnlyTheSpokenLine (FIX-FINAL
// tux-resumed-task-answer-never-delivered): a notice carrying a task id
// shows it on the banner and on Telegram, but the Speaker says only the
// caller's short spoken line.
func TestFanOutSendNoticeShowsTextAndSpeaksOnlyTheSpokenLine(t *testing.T) {
	const id = "t_0123456789abcdef0123456789abcdef"
	text := "Devam ettirilen görev tamamlandı (" + id + "). Cevap:\n\nMerhaba, dünya."
	const spoken = "Devam ettirilen bir görev tamamlandı; cevabı bildirimde."
	speaker, argvLog := newTestSpeaker(t, yeldaVoiceList)
	stdinLog := filepath.Join(t.TempDir(), "stdin.log")
	t.Setenv("FAKE_SAY_STDIN_LOG", stdinLog)
	local, mu, luaArgs := recordingHS()
	local.Speaker = speaker
	remote := &capturingRemote{}

	if !(FanOutDelivery{Primary: remote, Local: local}).SendNotice(context.Background(), "trace1", text, spoken) {
		t.Fatal("SendNotice = false")
	}
	if len(remote.text) != 1 || remote.text[0] != text {
		t.Errorf("remote got %q, want the full text", remote.text)
	}
	if msgs := bannerMessages(t, mu, luaArgs); len(msgs) != 1 || msgs[0] != text {
		t.Errorf("banner messages = %q, want the full text", msgs)
	}
	if n := countUtteranceInvocations(t, argvLog); n != 1 {
		t.Fatalf("say utterances = %d, want 1", n)
	}
	if said := strings.TrimSuffix(readFileString(t, stdinLog), "\x00"); said != spoken {
		t.Errorf("spoken = %q, want exactly %q", said, spoken)
	}
}

// TestFanOutSendNoticeFallsBackForAPlainLocalDelivery: a Local that is not
// a NoticeSender still gets the text.
func TestFanOutSendNoticeFallsBackForAPlainLocalDelivery(t *testing.T) {
	remote, local := &capturingRemote{}, &capturingRemote{}
	if !(FanOutDelivery{Primary: remote, Local: local}).SendNotice(context.Background(), "trace1", "metin", "söz") {
		t.Fatal("SendNotice = false")
	}
	if len(local.text) != 1 || local.text[0] != "metin" || len(remote.text) != 1 {
		t.Errorf("local = %q, remote = %q", local.text, remote.text)
	}
	if (FanOutDelivery{}).SendNotice(context.Background(), "trace1", "metin", "söz") {
		t.Error("an empty FanOutDelivery reported a delivery")
	}
}
