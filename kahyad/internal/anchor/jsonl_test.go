package anchor

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kahya/kahyad/internal/logx"
)

// readJSONLLines parses every non-blank line of path as one JSON object -
// mirrors kahyad/internal/logx's own test helper of the same shape.
func readJSONLLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	var lines []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line %q is not valid JSON: %v", line, err)
		}
		lines = append(lines, m)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	return lines
}

// TestPusherRunLogsJSONLLineWithTraceID proves a successful anchor push
// emits at least one JSONL line carrying the SAME trace_id Run was called
// with (HANDOFF §4 ⚑ "her satır trace_id içeren JSONL"; task spec's own
// acceptance criterion: "JSONL log lines for push/verify carry a
// trace_id").
func TestPusherRunLogsJSONLLineWithTraceID(t *testing.T) {
	st := newTestStore(t)
	remote := newBareRemote(t)
	repoDir := filepath.Join(t.TempDir(), "anchor-repo")
	logDir := t.TempDir()
	jsonl, err := logx.New(logDir, "boot0000000000000000000000000000")
	if err != nil {
		t.Fatalf("logx.New: %v", err)
	}
	defer jsonl.Close()

	pusher := newPusher(st.Queries, nil, nil, NewExecGitRunner(), nil, remote, repoDir, "", 6)
	pusher.SetJSONLLogger(jsonl)

	ctx := context.Background()
	if err := st.LogEvent(ctx, "trace-1", "test.one", map[string]any{}); err != nil {
		t.Fatalf("LogEvent: %v", err)
	}
	const wantTraceID = "trace-push-jsonl-test"
	if err := pusher.Run(ctx, wantTraceID); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if err := jsonl.Close(); err != nil {
		t.Fatalf("jsonl.Close(): %v", err)
	}

	lines := readJSONLLines(t, filepath.Join(logDir, "kahyad.jsonl"))
	found := false
	for _, l := range lines {
		if l["event"] == "anchor_pushed" && l["trace_id"] == wantTraceID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no anchor_pushed JSONL line with trace_id=%q found in %+v", wantTraceID, lines)
	}
}

// TestVerifierVerifyLogsJSONLLineWithTraceID mirrors the Pusher test above
// for `kahya ledger verify`'s own JSONL evidence.
func TestVerifierVerifyLogsJSONLLineWithTraceID(t *testing.T) {
	st := newTestStore(t)
	logDir := t.TempDir()
	jsonl, err := logx.New(logDir, "boot0000000000000000000000000000")
	if err != nil {
		t.Fatalf("logx.New: %v", err)
	}
	defer jsonl.Close()

	verifier := newVerifier(st.Queries, nil, nil, NewExecGitRunner(), nil, "", "")
	verifier.SetJSONLLogger(jsonl)

	const wantTraceID = "trace-verify-jsonl-test"
	if _, err := verifier.Verify(context.Background(), wantTraceID); err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if err := jsonl.Close(); err != nil {
		t.Fatalf("jsonl.Close(): %v", err)
	}

	lines := readJSONLLines(t, filepath.Join(logDir, "kahyad.jsonl"))
	found := false
	for _, l := range lines {
		if l["event"] == "ledger_verify_ok" && l["trace_id"] == wantTraceID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no ledger_verify_ok JSONL line with trace_id=%q found in %+v", wantTraceID, lines)
	}
}
