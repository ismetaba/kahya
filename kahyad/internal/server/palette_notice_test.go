package server

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTaskSSEQuietNoticeStillKeepsAlive (FIX-FINAL TUX-6): a quiet stream
// writes no approval-wait notice but still writes keepalives.
func TestTaskSSEQuietNoticeStillKeepsAlive(t *testing.T) {
	var buf strings.Builder
	sse := newTaskSSE(&buf, nil, func(event string, payload any) { fmt.Fprintf(&buf, "event: %s\n\n", event) })
	sse.quietNotice = true
	sse.notice("abc")
	sse.keepalive()
	if got := buf.String(); got != ": keepalive\n\n" {
		t.Fatalf("quiet stream = %q, want only the keepalive comment", got)
	}
}

// TestPaletteTaskStreamOmitsApprovalNotice: the B-1 approval-wait notice
// was written into every /v1/task answer stream as a "delta", so the
// ⌥Space completion notification (kahya.lua reads `kahya ask` stdout when
// the task ends) opened with "⏳ Onay bekleniyor ... başka bir terminalde
// 'kahya approve <64-hex>' çalıştır." for a decision already made on the
// Hammerspoon card. A palette run (palette_opened_at) now gets keepalives
// and no notice delta; a terminal `kahya ask` still gets the byte-exact
// notice.
func TestPaletteTaskStreamOmitsApprovalNotice(t *testing.T) {
	worker := filepath.Join(t.TempDir(), "slow_worker.py")
	if err := os.WriteFile(worker, []byte("import json, sys, time\nsys.stdin.buffer.read()\ntime.sleep(1.5)\nprint(json.dumps({\"type\": \"result\", \"status\": \"ok\"}), flush=True)\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, palette := range []bool{true, false} {
		f := newTaskTestFixture(t, []string{"python3", worker}, 5)
		f.srv.approvalKeepalive = 20 * time.Millisecond
		traceID := fmt.Sprintf("trace-palette-notice-%v-0000000000", palette)
		go func() {
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				var taskID string
				f.srv.approvalNotices.Range(func(k, _ any) bool { taskID = k.(string); return false })
				if taskID != "" {
					stop := f.srv.startApprovalNotice(taskID, "abc123")
					time.Sleep(150 * time.Millisecond)
					stop()
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}()
		var resp *http.Response
		if palette {
			resp = postTaskWithPalette(t, f.client, traceID, "dosyayı yaz", float64(time.Now().Unix()))
		} else {
			resp = postTask(t, f.client, traceID, "dosyayı yaz")
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		body := string(raw)
		if !strings.Contains(body, ": keepalive") {
			t.Fatalf("palette=%v: no keepalive while parked: %q", palette, body)
		}
		hasNotice := strings.Contains(body, "Onay bekleniyor")
		if palette && hasNotice {
			t.Fatalf("palette run's answer stream carries the approval notice: %q", body)
		}
		if !palette && !hasNotice {
			t.Fatalf("terminal run lost the approval notice: %q", body)
		}
	}
}
