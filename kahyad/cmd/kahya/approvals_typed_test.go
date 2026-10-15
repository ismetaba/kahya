package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestApprovalsDecideW3TypedMismatchIsTurkishExit3 (FIX-FINAL TUX-4): a W3
// approve whose typed word was not exactly "onayla" printed the English
// policy error and exited 2, which Hammerspoon could not tell from a
// transport failure ("Onay kararı iletilemedi"). The daemon's stable code
// now maps to exit 3 and a Turkish line naming the next step; any other
// refusal keeps exit 2.
func TestApprovalsDecideW3TypedMismatchIsTurkishExit3(t *testing.T) {
	code := "w3_typed_onayla_required"
	t.Setenv("KAHYA_SOCKET", startFakeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": `policy: W3 approval requires the typed confirmation word "onayla"`, "code": code})
	})))
	var stdout, stderr bytes.Buffer
	exit := run([]string{"approvals", "decide", "pa-1", "--approve", "--typed", "Onayla"}, strings.NewReader(""), &stdout, &stderr)
	if want := fmt.Sprintf(MsgApprovalW3TypedMismatchFmt, "pa-1") + "\n"; exit != 3 || stdout.Len() != 0 || stderr.String() != want {
		t.Fatalf("exit=%d stdout=%q stderr=%q, want 3 and %q", exit, stdout.String(), stderr.String(), want)
	}
	if strings.Contains(stderr.String(), "policy:") {
		t.Fatal("the English policy error reached the user")
	}

	code = ""
	stdout.Reset()
	stderr.Reset()
	if exit := run([]string{"approvals", "decide", "pa-1", "--approve", "--typed", "onayla"}, strings.NewReader(""), &stdout, &stderr); exit != 2 {
		t.Fatalf("another refusal: exit=%d, want 2", exit)
	}
}
