// palette_events_test.go covers W6-01's palette_open/first_token ledger
// events: submitting a task with palette_opened_at (hammerspoon/kahya.lua's
// `kahya ask --palette-opened-at <t>` flag) against the stub-model harness
// (the SAME real-worker-process, fake-upstream fixture task_test.go's own
// TestTaskEndToEndSuccessEchoWorker uses) writes palette_open and
// first_token events sharing the task's trace_id, with first_token.ts >=
// palette_open.ts. A second test proves first_token ALSO fires on the
// OTHER relay point this task names explicitly: the W3-08 secret-lane
// local answer path.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"kahya/kahyad/internal/secretlane"
)

// postTaskWithPalette POSTs /v1/task carrying palette_opened_at - the one
// wire field task_test.go's own postTask/postTaskFull helpers don't set.
func postTaskWithPalette(t *testing.T, client *http.Client, traceID, prompt string, paletteOpenedAt float64) *http.Response {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"prompt": prompt, "trace_id": traceID, "palette_opened_at": paletteOpenedAt,
	})
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, "http://kahyad/v1/task", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Kahya-Trace-Id", traceID)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/task: %v", err)
	}
	return resp
}

// eventTimestamp returns the events.ts column (RFC3339Nano) for the single
// row matching (traceID, kind) - fails the test if there is not exactly
// one.
func eventTimestamp(t *testing.T, f taskTestFixture, traceID, kind string) time.Time {
	t.Helper()
	var raw string
	if err := f.store.DB().QueryRow(
		`SELECT ts FROM events WHERE trace_id = ? AND kind = ?`, traceID, kind,
	).Scan(&raw); err != nil {
		t.Fatalf("query events ts (trace_id=%s kind=%s): %v", traceID, kind, err)
	}
	ts, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t.Fatalf("parse events.ts %q: %v", raw, err)
	}
	return ts
}

// TestPaletteOpenAndFirstTokenEventsCloudLane drives a real worker process
// (the spawn package's echo fake, the SAME stub used by task_test.go's own
// TestTaskEndToEndSuccessEchoWorker) with palette_opened_at set, and
// asserts palette_open + first_token both land under the task's own
// trace_id, in that chronological order.
func TestPaletteOpenAndFirstTokenEventsCloudLane(t *testing.T) {
	script := filepath.Join(spawnTestdataDir(t), "echo_worker.py")
	f := newTaskTestFixture(t, []string{"python3", script}, 5)

	traceID := "trace-palette-cloud-000000000001"
	// paletteOpenedAt is deliberately in the recent past (hotkey press
	// necessarily precedes this HTTP request reaching kahyad at all) -
	// this is what makes first_token.ts >= palette_open.ts a MEANINGFUL
	// assertion (rather than one that would pass by construction even if
	// the two events were logged in the wrong order).
	paletteOpenedAt := float64(time.Now().Add(-2 * time.Second).Unix())

	resp := postTaskWithPalette(t, f.client, traceID, "test sorusu", paletteOpenedAt)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	frames := readAllSSE(t, resp)
	if len(frames) == 0 {
		t.Fatal("no SSE frames received")
	}

	assertLedgerHasKind(t, f.store, traceID, "palette_open")
	assertLedgerHasKind(t, f.store, traceID, "first_token")

	var payload string
	if err := f.store.DB().QueryRow(
		`SELECT payload FROM events WHERE trace_id = ? AND kind = 'palette_open'`, traceID,
	).Scan(&payload); err != nil {
		t.Fatalf("query palette_open payload: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("decode palette_open payload: %v", err)
	}
	if got, _ := decoded["palette_opened_at"].(float64); got != paletteOpenedAt {
		t.Errorf("palette_open payload[palette_opened_at] = %v, want %v", decoded["palette_opened_at"], paletteOpenedAt)
	}

	paletteTS := eventTimestamp(t, f, traceID, "palette_open")
	firstTokenTS := eventTimestamp(t, f, traceID, "first_token")
	if firstTokenTS.Before(paletteTS) {
		t.Errorf("first_token.ts (%s) is before palette_open.ts (%s), want >=", firstTokenTS, paletteTS)
	}
}

// TestPaletteOpenAndFirstTokenEventsSecretLane proves first_token ALSO
// fires on the W3-08 secret-lane LOCAL answer path (handleSecretLaneTask/
// finishSecretLaneTask) - not only the cloud/stub forward-proxy path
// above - so a palette command classified secret-lane never silently
// drops out of the north-star metric. The worker command is a script that
// does not exist at all (mirroring secretlane_task_test.go's own
// convention): a passing test proves the worker was never spawned for
// this task either.
func TestPaletteOpenAndFirstTokenEventsSecretLane(t *testing.T) {
	f := newTaskTestFixture(t, []string{"/nonexistent/worker/binary/should/never/run"}, 5)

	classifier := secretlane.NewClassifier(nil)
	answerer := secretlane.AnswererFunc(func(ctx context.Context, prompt string) (string, error) {
		return "Yerelde işlendi.", nil
	})
	f.srv.SetSecretLane(classifier, answerer, nil)

	traceID := "trace-palette-secretlane-00000001"
	paletteOpenedAt := float64(time.Now().Add(-3 * time.Second).Unix())

	resp := postTaskWithPalette(t, f.client, traceID, "kredi kartı ekstresi ekte, özetler misin", paletteOpenedAt)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	frames := readAllSSE(t, resp)
	if len(frames) == 0 {
		t.Fatal("no SSE frames received")
	}

	assertLedgerHasKind(t, f.store, traceID, "palette_open")
	assertLedgerHasKind(t, f.store, traceID, "first_token")

	paletteTS := eventTimestamp(t, f, traceID, "palette_open")
	firstTokenTS := eventTimestamp(t, f, traceID, "first_token")
	if firstTokenTS.Before(paletteTS) {
		t.Errorf("first_token.ts (%s) is before palette_open.ts (%s), want >=", firstTokenTS, paletteTS)
	}
}

// TestNoPaletteOpenedAtNoPaletteOpenEvent proves an ordinary task (no
// palette_opened_at field at all - `kahya ask`/one-shot/REPL) never writes
// a palette_open event, while first_token is still recorded (the metric's
// OTHER half is useful independent of the palette surface).
func TestNoPaletteOpenedAtNoPaletteOpenEvent(t *testing.T) {
	script := filepath.Join(spawnTestdataDir(t), "echo_worker.py")
	f := newTaskTestFixture(t, []string{"python3", script}, 5)

	traceID := "trace-no-palette-0000000000001"
	resp := postTask(t, f.client, traceID, "test sorusu")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	readAllSSE(t, resp)

	var n int
	if err := f.store.DB().QueryRow(
		`SELECT count(*) FROM events WHERE trace_id = ? AND kind = 'palette_open'`, traceID,
	).Scan(&n); err != nil {
		t.Fatalf("count palette_open events: %v", err)
	}
	if n != 0 {
		t.Errorf("palette_open event count = %d, want 0 (no palette_opened_at was sent)", n)
	}
	assertLedgerHasKind(t, f.store, traceID, "first_token")
}
