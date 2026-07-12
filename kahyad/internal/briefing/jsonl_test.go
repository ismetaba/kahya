package briefing

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kahya/kahyad/internal/logx"
)

// TestRunEmitsCollectorWorkerAndDeliveryJSONLLinesUnderOneTraceID is this
// task's own acceptance criterion: "`kahya log --trace <id>` shows
// collector, worker, and delivery lines all under one trace_id" -
// kahya log reads *.jsonl files directly (kahyad/internal/server's own
// readLogLines), so this asserts against a REAL logx.Logger writing to a
// temp log dir, then re-reads that file exactly the way readLogLines
// does (grep each line's own trace_id field).
func TestRunEmitsCollectorWorkerAndDeliveryJSONLLinesUnderOneTraceID(t *testing.T) {
	logDir := t.TempDir()
	log, err := logx.New(logDir, "boot-trace")
	if err != nil {
		t.Fatalf("logx.New: %v", err)
	}
	t.Cleanup(func() { log.Close() })

	classifier := permissiveClassifier()
	spawner := &fakeWorkerSpawner{RawJSON: `{"lines":["ozet"]}`}
	delivery := &fakeDelivery{Sent: true}

	o := &Orchestrator{
		Classifier: classifier,
		GH:         GHCollector{Runner: &fakeGHRunner{PRJSON: []byte(`[{"number":1,"title":"bump deps"}]`)}, Repos: []string{"kahya/x"}},
		Spawner:    spawner,
		Delivery:   delivery,
		Log:        log,
		Now:        fixedNow(time.Date(2026, 7, 12, 8, 30, 0, 0, time.UTC)),
	}

	const traceID = "trace-jsonl"
	result, err := o.Run(context.Background(), traceID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Delivered {
		t.Fatal("Delivered = false, want true")
	}

	lines := readJSONLLinesForTrace(t, logDir, traceID)
	var sawCollector, sawWorker, sawDelivery bool
	for _, l := range lines {
		switch l["event"] {
		case "briefing_collected":
			sawCollector = true
		case "briefing_worker_spawn", "briefing_worker_done":
			sawWorker = true
		case "briefing_delivered":
			sawDelivery = true
		}
		if l["trace_id"] != traceID {
			t.Errorf("line %+v has trace_id %v, want %q", l, l["trace_id"], traceID)
		}
	}
	if !sawCollector {
		t.Error("no collector JSONL line found")
	}
	if !sawWorker {
		t.Error("no worker JSONL line found")
	}
	if !sawDelivery {
		t.Error("no delivery JSONL line found")
	}
}

// readJSONLLinesForTrace reads every *.jsonl file under logDir and
// returns every decoded line whose trace_id equals traceID - mirrors
// kahyad/internal/server.readLogLines' own scan, minus the HTTP/sorting
// layer this test does not need.
func readJSONLLinesForTrace(t *testing.T, logDir, traceID string) []map[string]any {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(logDir, "*.jsonl"))
	if err != nil {
		t.Fatalf("glob %s: %v", logDir, err)
	}
	var out []map[string]any
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				continue
			}
			if m["trace_id"] == traceID {
				out = append(out, m)
			}
		}
	}
	return out
}
