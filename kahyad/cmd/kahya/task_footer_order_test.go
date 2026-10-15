package main

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestTaskErrorReasonPrintedBeforeTraceFooter (FIX-FINAL TUX-10): on a
// non-ok result execTask printed the "iz:" footer BEFORE the reason, so a
// failed palette run's macOS notification (kahya.lua shows stderr) led
// with "iz: <hex>". The reason now comes first, like the exit-2 path and
// W12-06's "footer after each answer" - for typed and audio questions.
func TestTaskErrorReasonPrintedBeforeTraceFooter(t *testing.T) {
	const reason = "Görev bütçe tavanı nedeniyle duraklatıldı."
	t.Setenv("KAHYA_SOCKET", startFakeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: result\ndata: {\"status\":\"error\",\"task_id\":\"t1\",\"message\":%q}\n\n", reason)
	})))
	for _, audio := range []bool{false, true} {
		var stdout, stderr bytes.Buffer
		var code int
		if audio {
			client := newClient(mustSocket(t))
			code = execTaskAudio(client, "trace-footer-1", "/tmp/kayit.wav", false, nil, false, &stdout, &stderr)
		} else {
			client := newClient(mustSocket(t))
			code = execTask(client, "trace-footer-1", "soru", false, nil, false, &stdout, &stderr)
		}
		if want := reason + "\n" + fmt.Sprintf(MsgTraceFooter, "trace-footer-1") + "\n"; code != 1 || stderr.String() != want {
			t.Fatalf("audio=%v: exit=%d stderr=%q, want 1 and %q", audio, code, stderr.String(), want)
		}
	}
}

func mustSocket(t *testing.T) string {
	t.Helper()
	sock, err := resolveSocket()
	if err != nil || strings.TrimSpace(sock) == "" {
		t.Fatalf("resolveSocket: %q %v", sock, err)
	}
	return sock
}
