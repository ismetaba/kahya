package ui

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// FIX-FINAL tux-tts-speaks-task-ids-in-delivered-notices (with
// adv2-task-notices-spoken-with-hex-ids), orchestrator decision (a): with
// tts.enabled the Speaker read every delivered notice aloud as shown - the
// blocked_user notices carry the 34-character task id twice plus a `kahya
// task resolve` command, the consolidation notices a CLI command. Speech
// now never reads a task id, a trace id, a hex run or a CLI command; the
// banner and Telegram keep the full text.

// idOrCommandRe is what speech must never contain.
var idOrCommandRe = regexp.MustCompile(`t_[0-9a-f]{8,}|[0-9a-fA-F]{16,}|kahya [a-z]|git -C`)

// speakVia runs one SendNotification or SendNotice through a real Speaker
// (fake `say`) and returns what was said and what the banner showed.
func speakVia(t *testing.T, send func(h *HSCli) bool) (said, banner string) {
	t.Helper()
	speaker, argvLog := newTestSpeaker(t, yeldaVoiceList)
	stdinLog := filepath.Join(t.TempDir(), "stdin.log")
	t.Setenv("FAKE_SAY_STDIN_LOG", stdinLog)
	h, mu, luaArgs := recordingHS()
	h.Speaker = speaker
	if !send(h) {
		t.Fatal("delivery = false")
	}
	if n := countUtteranceInvocations(t, argvLog); n != 1 {
		t.Fatalf("say utterances = %d, want 1", n)
	}
	msgs := bannerMessages(t, mu, luaArgs)
	if len(msgs) != 1 {
		t.Fatalf("banners = %q, want 1", msgs)
	}
	return strings.TrimSuffix(readFileString(t, stdinLog), "\x00"), msgs[0]
}

func TestSendNotificationNeverSpeaksIDsOrCommands(t *testing.T) {
	const id = "t_0123456789abcdef0123456789abcdef"
	const trace = "0123456789abcdef0123456789abcdef"
	for i, text := range []string{
		// task.MsgResumeCapExceededFmt, as the resume scan sends it.
		fmt.Sprintf("Görev %s: işçi süreci %d kez yarıda kesildi, otomatik devam limiti doldu — 'kahya task resolve %s' ile karar ver.", id, 3, id),
		// consolidation.MsgSuggestionReady / MsgRepoStateUnsafe.
		"Konsolidasyon önerisi hazır — kahya consolidation show",
		"Hafıza deposu (~/Kahya) beklenmedik git durumunda — konsolidasyon atlandı; 'git -C ~/Kahya status' ile kontrol et",
		// a bare trace id that is not the stripped "trace_id:" line.
		"Bir şey oldu (iz " + trace + ").",
	} {
		t.Run(fmt.Sprintf("notice%d", i), func(t *testing.T) {
			said, banner := speakVia(t, func(h *HSCli) bool { return h.SendNotification(context.Background(), trace, text) })
			if said != MsgSpokenNoticeFallback {
				t.Errorf("spoken = %q, want the id-free fallback %q", said, MsgSpokenNoticeFallback)
			}
			if idOrCommandRe.MatchString(said) {
				t.Errorf("speech reads an id or a command: %q", said)
			}
			if banner != text {
				t.Errorf("banner = %q, want the full text %q", banner, text)
			}
		})
	}
	if MsgSpokenNoticeFallback != "Kâhya'dan yeni bir bildirim var; ayrıntı ekranda." {
		t.Errorf("MsgSpokenNoticeFallback = %q", MsgSpokenNoticeFallback)
	}
}

// TestSendNotificationStillSpeaksPlainText: a notice with no id or command
// (the briefing's bullet lines) is still read as shown (W6-05).
func TestSendNotificationStillSpeaksPlainText(t *testing.T) {
	text := "Günaydın — sabah brifingi\n- Bugün 2 görev var\n- PR #42 incelemede"
	said, _ := speakVia(t, func(h *HSCli) bool { return h.SendNotification(context.Background(), "tr", text) })
	if said != text {
		t.Errorf("spoken = %q, want the text itself", said)
	}
}

// TestSendNoticeSpokenLineIsAlsoGuarded: a caller's spoken line is guarded
// by the same rule (a spoken line carrying an id falls back too).
func TestSendNoticeSpokenLineIsAlsoGuarded(t *testing.T) {
	said, _ := speakVia(t, func(h *HSCli) bool {
		return h.SendNotice(context.Background(), "tr", "metin", "görev t_0123456789abcdef bekliyor")
	})
	if said != MsgSpokenNoticeFallback {
		t.Errorf("spoken = %q, want the fallback", said)
	}
}
