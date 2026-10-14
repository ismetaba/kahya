//go:build acceptance

package w6gate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSensitiveReadMarkSurvivesDaemonRestart is FIX-B-7 over the real
// bin/kahyad: a sensitive-read mark (POST /session/sensitive-read, the same
// egress.Gate.MarkSensitiveRead mcp/fs's fs_read seam calls) is written to
// brain.db's sensitive_reads table, and the next kahyad boot on the same
// brain.db loads it (boot JSONL sensitive_reads_loaded count=1) - the
// production tracker is the durable one, not the old in-memory map. A
// repeat mark after the restart adds no second row (rises only).
func TestSensitiveReadMarkSurvivesDaemonRestart(t *testing.T) {
	d := bootKahyad(t, daemonOpts{})
	db := d.openDB(t)
	traceID := newTraceID()

	mark := func() {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"session_id": traceID, "trace_id": traceID})
		req, err := http.NewRequest(http.MethodPost, "http://kahyad/session/sensitive-read", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Kahya-Trace-Id", traceID)
		resp, err := d.client.Do(req)
		if err != nil {
			t.Fatalf("POST /session/sensitive-read: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST /session/sensitive-read status = %d, want 200", resp.StatusCode)
		}
	}
	rows := func() int {
		t.Helper()
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM sensitive_reads WHERE session_key = ?`, traceID).Scan(&n); err != nil {
			t.Fatalf("count sensitive_reads: %v", err)
		}
		return n
	}

	mark()
	if n := rows(); n != 1 {
		t.Fatalf("sensitive_reads rows for the marked session = %d, want 1", n)
	}
	var reason string
	if err := db.QueryRow(`SELECT reason FROM sensitive_reads WHERE session_key = ? AND trace_id = ?`, traceID, traceID).Scan(&reason); err != nil {
		t.Fatalf("read sensitive_reads row: %v", err)
	}
	if reason != "secret_lane_read" {
		t.Errorf("sensitive_reads.reason = %q, want secret_lane_read", reason)
	}

	d.stop()
	d.start()

	if got := loadedCounts(t, d.dirs.homeDir); len(got) != 2 || got[0] != 0 || got[1] != 1 {
		t.Fatalf("boot sensitive_reads_loaded counts = %v, want [0 1] (first boot empty, second boot loads the persisted mark)\n%s", got, dumpLogs(d.dirs.homeDir))
	}

	mark()
	if n := rows(); n != 1 {
		t.Fatalf("sensitive_reads rows after a restart + second mark = %d, want still 1", n)
	}
	if n := countEventsMatching(t, db, traceID, "sensitive_read_marked"); n != 2 {
		t.Errorf("sensitive_read_marked ledger rows = %d, want 2 (one per mark)", n)
	}
}

// loadedCounts returns every boot's sensitive_reads_loaded count from the
// kahyad stderr JSONL logs, oldest boot first. Each line must carry a
// trace_id (CLAUDE.md: JSONL with trace_id on every line).
func loadedCounts(t *testing.T, homeDir string) []int64 {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(homeDir, "kahyad.stderr.*.log"))
	if err != nil {
		t.Fatal(err)
	}
	// start() names each boot's file by time.Now().UnixNano(), and the
	// glob is sorted lexically; every name has the same digit count.
	var out []int64
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if !strings.Contains(line, `"sensitive_reads_loaded"`) {
				continue
			}
			var ev struct {
				Event   string `json:"event"`
				TraceID string `json:"trace_id"`
				Count   int64  `json:"count"`
			}
			if err := json.Unmarshal([]byte(line), &ev); err != nil || ev.Event != "sensitive_reads_loaded" {
				continue
			}
			if ev.TraceID == "" {
				t.Errorf("sensitive_reads_loaded line without trace_id: %s", line)
			}
			out = append(out, ev.Count)
		}
		f.Close()
	}
	return out
}
