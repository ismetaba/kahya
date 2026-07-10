// Package logx implements kahyad's JSONL logger. Every process in the
// Kâhya system logs JSONL with a trace_id on every line (HANDOFF §4 ⚑) —
// this is a W1-2 acceptance criterion, measurable only if it holds for
// every single line, including boot-time lines with no request in flight.
package logx

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"kahya/kahyad/internal/traceid"
)

// requiredKeys documents the four keys every JSONL line must carry:
// ts, level, event, trace_id.
const (
	keyTraceID = "trace_id"
	fileName   = "kahyad.jsonl"
)

// Logger is a JSONL logger scoped to a single trace_id. Every line it
// writes carries ts (RFC3339Nano), level, event, and trace_id.
type Logger struct {
	base    *slog.Logger
	traceID string
	file    *os.File
}

// level is the process-wide minimum log level shared by every Logger
// (current and future - they all point at this same LevelVar via New's
// HandlerOptions). Its zero value is slog.LevelInfo, so Debug() lines are
// silently discarded until SetLevel(slog.LevelDebug) raises the floor
// (MINOR 5: logx previously had no level control at all, so Debug() built
// a well-formed JSONL line and then handed it to a handler whose level
// nothing here ever configured - it just happened to default to Info).
var level slog.LevelVar

// SetLevel sets the process-wide minimum log level for every Logger.
// main.go calls this once at boot, after config.Load and before logx.New,
// using the resolved config.Config.LogLevel.
func SetLevel(l slog.Level) {
	level.Set(l)
}

// New creates the boot logger: it appends JSONL to <logDir>/kahyad.jsonl
// (creating logDir with 0700 if needed) and mirrors every line to stderr.
// bootTraceID is attached to every line logged directly on the returned
// Logger; call With to scope a child logger to a different trace_id (e.g.
// per HTTP request).
func New(logDir, bootTraceID string) (*Logger, error) {
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return nil, fmt.Errorf("logx: create log dir %s: %w", logDir, err)
	}
	// MkdirAll is a no-op on an existing directory's mode; enforce 0700
	// even when the log dir pre-existed with looser permissions.
	if err := os.Chmod(logDir, 0o700); err != nil {
		return nil, fmt.Errorf("logx: chmod log dir %s: %w", logDir, err)
	}
	path := filepath.Join(logDir, fileName)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("logx: open %s: %w", path, err)
	}

	mw := io.MultiWriter(f, os.Stderr)
	opts := &slog.HandlerOptions{
		Level: &level,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			switch a.Key {
			case slog.TimeKey:
				a.Key = "ts" // value stays a time.Time; the JSON handler
				// formats slog.KindTime values with time.RFC3339Nano.
			case slog.MessageKey:
				a.Key = "event"
			}
			return a
		},
	}
	base := slog.New(slog.NewJSONHandler(mw, opts))

	if bootTraceID == "" {
		// Defensive fallback: no line may ever have an empty trace_id, even
		// if a caller forgets to mint one.
		bootTraceID = traceid.New()
	}
	return &Logger{base: base, traceID: bootTraceID, file: f}, nil
}

// With returns a new Logger sharing the same output but scoped to traceID.
// It never mutates the receiver, and never stacks trace_id attributes —
// each call replaces the scope rather than layering onto it. An empty
// traceID is replaced with a freshly minted one so the "never empty
// trace_id" invariant holds regardless of caller input.
func (l *Logger) With(traceID string) *Logger {
	if traceID == "" {
		traceID = traceid.New()
	}
	return &Logger{base: l.base, traceID: traceID, file: l.file}
}

// TraceID returns the trace_id this logger is scoped to.
func (l *Logger) TraceID() string {
	return l.traceID
}

// Close closes the underlying log file.
func (l *Logger) Close() error {
	if l.file == nil {
		return nil
	}
	return l.file.Close()
}

func (l *Logger) log(level slog.Level, event string, args ...any) {
	attrs := make([]any, 0, len(args)+2)
	attrs = append(attrs, keyTraceID, l.traceID)
	attrs = append(attrs, args...)
	l.base.Log(context.Background(), level, event, attrs...)
}

// Debug logs a debug-level JSONL line with event as the "event" field.
func (l *Logger) Debug(event string, args ...any) { l.log(slog.LevelDebug, event, args...) }

// Info logs an info-level JSONL line with event as the "event" field.
func (l *Logger) Info(event string, args ...any) { l.log(slog.LevelInfo, event, args...) }

// Warn logs a warn-level JSONL line with event as the "event" field.
func (l *Logger) Warn(event string, args ...any) { l.log(slog.LevelWarn, event, args...) }

// Error logs an error-level JSONL line with event as the "event" field.
func (l *Logger) Error(event string, args ...any) { l.log(slog.LevelError, event, args...) }
