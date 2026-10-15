package telegram

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"kahya/kahyad/internal/policy"
)

// TestMemoryToolSecretGlobCardRedacted (FIX-FINAL telegram-memory-write-
// gizli-card-unredacted): the first redaction gate checked secret-lane
// globs only for fs_write/fs_delete paths, so a reversible memory_write
// card for a note under gizli/** (or finans*.md) went to Telegram in full,
// with an Onayla keyboard, whenever its content missed the lexicon - and
// memory_forget leaked a gizli file name the same way. The memory tools'
// `file` (relative to the memory dir) is now matched against the
// secret-lane globs too, folded like memlane/consolidation match it.
func TestMemoryToolSecretGlobCardRedacted(t *testing.T) {
	const clean = "Persembe 15:30 Kaya Bey ile gorusme"
	cases := []struct {
		name, tool, input string
		wantRedact        bool
		wantReason        string
	}{
		{"write_gizli", "memory_write", `{"content":"` + clean + `","file":"gizli/notes/randevular.md"}`, true, redactReasonGlob},
		{"write_gizli_casefold", "memory_write", `{"content":"` + clean + `","file":"GİZLİ/x.md"}`, true, redactReasonGlob},
		{"write_finans", "memory_write", `{"content":"` + clean + `","file":"finans-2026.md"}`, true, redactReasonGlob},
		{"forget_gizli", "memory_forget", `{"file":"gizli/notes/randevular.md"}`, true, redactReasonGlob},
		{"write_inbox", "memory_write", `{"content":"` + clean + `","file":"inbox/x.md"}`, false, ""},
		{"write_default_file", "memory_write", `{"content":"` + clean + `"}`, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sender := &fakeSender{}
			ledger := &fakeLedger{}
			home := testHome(t)
			b := newTestBot(t, testConfig(), sender, ledger, newAllowGate(t), nil, nil)
			b.home = home
			b.memoryDir = filepath.Join(home, "Kahya", "memory")
			b.secretLaneGlobs = []string{filepath.Join(home, "Kahya/memory/finans*.md"), filepath.Join(home, "Kahya/memory/gizli/**")}
			b.OnPendingApproval(context.Background(), policy.PendingApprovalInfo{
				ID: "6161616161616161616161616161616161616161616161616161616161616161", Tool: tc.tool, Class: policy.ClassW1,
				ToolInput: []byte(tc.input), TraceID: "trace-mw-" + tc.name,
			})
			if len(sender.sent) != 1 {
				t.Fatalf("sent %d messages (%q), want 1", len(sender.sent), sender.allTexts())
			}
			text := sender.sent[0].Text
			if tc.wantRedact {
				if want := "🔒 Yerel onay gerekiyor: " + tc.tool + " (gizli şerit)"; text != want {
					t.Fatalf("sent %q, want only the redacted title %q", text, want)
				}
				if sender.sent[0].Markup != nil {
					t.Fatalf("a redacted notice carried a keyboard")
				}
				if reasons := redactedReasons(ledger); len(reasons) != 1 || reasons[0] != tc.wantReason {
					t.Fatalf("telegram_card_redacted reasons = %q, want [%q]", reasons, tc.wantReason)
				}
				return
			}
			if strings.Contains(text, "gizli şerit") || !strings.Contains(text, "Kaya") {
				t.Fatalf("a non-secret memory card was redacted: %q", text)
			}
			if sender.sent[0].Markup == nil {
				t.Fatalf("a reversible non-secret memory_write card lost its keyboard")
			}
		})
	}
}

// TestMemoryToolCardRedactedWhenMemoryDirUnwired: with no memory dir to
// resolve the `file` against, a memory-tool card that names a file is
// redacted (fail-closed).
func TestMemoryToolCardRedactedWhenMemoryDirUnwired(t *testing.T) {
	sender := &fakeSender{}
	ledger := &fakeLedger{}
	b := newTestBot(t, testConfig(), sender, ledger, newAllowGate(t), nil, nil)
	b.memoryDir = ""
	b.OnPendingApproval(context.Background(), policy.PendingApprovalInfo{
		ID: "6161616161616161616161616161616161616161616161616161616161616161", Tool: "memory_write", Class: policy.ClassW1,
		ToolInput: []byte(`{"content":"x","file":"inbox/a.md"}`), TraceID: "trace-unwired",
	})
	if len(sender.sent) != 1 || strings.Contains(sender.sent[0].Text, "inbox") {
		t.Fatalf("unwired memory dir must redact: %q", sender.allTexts())
	}
	if reasons := redactedReasons(ledger); len(reasons) != 1 || reasons[0] != redactReasonMemoryDirUnwired {
		t.Fatalf("reasons = %q, want [%q]", reasons, redactReasonMemoryDirUnwired)
	}
}
