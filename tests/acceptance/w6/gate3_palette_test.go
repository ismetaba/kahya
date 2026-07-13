//go:build acceptance

package w6gate

import (
	"path/filepath"
	"testing"
	"time"
)

// TestW6Gate3PaletteAndFirstTokenLoggedToEvents is HANDOFF §6 W6's third
// acceptance clause: "palet-aç→ilk-token zaman damgaları events tablosuna
// loglanıyor" - the palette-open→first-token timestamps are logged to the
// events table.
//
// This gate proves ONLY that both timestamps EXIST and are correctly ordered
// (first_token.ts >= palette_open.ts) for one trace_id. The p50 <1.5s
// north-star is W78-04/dogfood work, explicitly NOT gated here; the observed
// delta is logged informationally via t.Logf, never asserted.
func TestW6Gate3PaletteAndFirstTokenLoggedToEvents(t *testing.T) {
	pythonBin := findPython3(t)
	// Reuse gate1's fake worker: an ordinary (non-audio) prompt hits its echo
	// branch, emitting one delta -> kahyad's first_token fires (sync.Once) on
	// that first streamed delta.
	workerScript := filepath.Join(fixturesDir(t), "stt_local_worker.py")

	d := bootKahyad(t, daemonOpts{
		workerCmd: []string{pythonBin, workerScript},
	})

	traceID := newTraceID()
	paletteOpenedAt := float64(time.Now().Unix())
	resp := d.postTaskBody(t, map[string]any{
		"trace_id":          traceID,
		"prompt":            "merhaba",
		"palette_opened_at": paletteOpenedAt,
	})
	drainSSEAsync(resp)

	db := d.openDB(t)
	if !waitForEvent(t, db, traceID, "palette_open", 10*time.Second) {
		t.Fatalf("no palette_open event for trace_id=%s\n%s", traceID, dumpLogs(d.dirs.homeDir))
	}
	if !waitForEvent(t, db, traceID, "first_token", 10*time.Second) {
		t.Fatalf("no first_token event for trace_id=%s\n%s", traceID, dumpLogs(d.dirs.homeDir))
	}

	paletteTS := eventTS(t, db, traceID, "palette_open")
	firstTS := eventTS(t, db, traceID, "first_token")

	// Exactly one palette_open, >=1 first_token.
	if len(paletteTS) != 1 {
		t.Fatalf("palette_open row count = %d, want exactly 1 (ts values=%v)", len(paletteTS), paletteTS)
	}
	if len(firstTS) < 1 {
		t.Fatalf("first_token row count = %d, want >=1", len(firstTS))
	}

	paletteT, err := time.Parse(time.RFC3339Nano, paletteTS[0])
	if err != nil {
		t.Fatalf("parse palette_open ts %q: %v", paletteTS[0], err)
	}
	firstT, err := time.Parse(time.RFC3339Nano, firstTS[0])
	if err != nil {
		t.Fatalf("parse first_token ts %q: %v", firstTS[0], err)
	}

	if firstT.Before(paletteT) {
		t.Fatalf("first_token.ts (%s) is BEFORE palette_open.ts (%s) - the palet-aç→ilk-token ordering invariant is violated", firstTS[0], paletteTS[0])
	}

	// Informational only (NOT gated - p50 <1.5s is W78-04/dogfood).
	t.Logf("palet-aç→ilk-token delta (events-table ts, informational): %d ms", firstT.Sub(paletteT).Milliseconds())
}
