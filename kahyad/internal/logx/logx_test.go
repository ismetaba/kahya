package logx

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readLines(t *testing.T, path string) []map[string]any {
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

func TestNewCreatesLogDirWithRestrictedPerms(t *testing.T) {
	base := t.TempDir()
	logDir := filepath.Join(base, "nested", "logs")

	l, err := New(logDir, "boot0000000000000000000000000000")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer l.Close()

	info, err := os.Stat(logDir)
	if err != nil {
		t.Fatalf("stat logDir: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("logDir perm = %o, want 700", perm)
	}
}

func TestLoggerEmitsValidJSONLWithRequiredKeys(t *testing.T) {
	logDir := t.TempDir()
	bootTrace := "boot0000000000000000000000000000"

	l, err := New(logDir, bootTrace)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	l.Info("boot", "pid", 1234)
	l.Warn("something_odd")
	l.Error("boot_failure", "err", "disk full")
	if err := l.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	lines := readLines(t, filepath.Join(logDir, "kahyad.jsonl"))
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3", len(lines))
	}

	for i, m := range lines {
		for _, key := range []string{"ts", "level", "event", "trace_id"} {
			v, ok := m[key]
			if !ok {
				t.Fatalf("line %d missing key %q: %v", i, key, m)
			}
			s, ok := v.(string)
			if !ok || s == "" {
				t.Fatalf("line %d key %q empty or non-string: %v", i, key, v)
			}
		}
		if m["trace_id"] != bootTrace {
			t.Errorf("line %d trace_id = %v, want %s", i, m["trace_id"], bootTrace)
		}
		if _, err := time.Parse(time.RFC3339Nano, m["ts"].(string)); err != nil {
			t.Errorf("line %d ts = %v not RFC3339Nano: %v", i, m["ts"], err)
		}
	}

	if lines[0]["event"] != "boot" {
		t.Errorf("line 0 event = %v, want boot", lines[0]["event"])
	}
}

func TestWithScopesTraceIDWithoutStacking(t *testing.T) {
	logDir := t.TempDir()
	l, err := New(logDir, "boot0000000000000000000000000000")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	child := l.With("request1111111111111111111111111")
	child.Info("http_request", "path", "/health")
	l.Info("still_boot_scoped")
	if err := l.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	lines := readLines(t, filepath.Join(logDir, "kahyad.jsonl"))
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	if lines[0]["trace_id"] != "request1111111111111111111111111" {
		t.Errorf("child trace_id = %v", lines[0]["trace_id"])
	}
	if lines[1]["trace_id"] != "boot0000000000000000000000000000" {
		t.Errorf("parent trace_id after With() = %v, want unchanged boot trace_id", lines[1]["trace_id"])
	}
}

func TestNeverEmptyTraceID(t *testing.T) {
	logDir := t.TempDir()
	// Passing "" simulates a caller mistake (forgetting to mint a boot
	// trace_id); logx must still never emit an empty trace_id.
	l, err := New(logDir, "")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	l.Info("edge_case")
	l.With("").Info("also_edge_case")
	l.Close()

	lines := readLines(t, filepath.Join(logDir, "kahyad.jsonl"))
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	for i, m := range lines {
		v, ok := m["trace_id"].(string)
		if !ok || v == "" {
			t.Fatalf("line %d trace_id empty or missing despite fallback: %v", i, m)
		}
	}
}
