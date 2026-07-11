// Package mockanthropic implements a small, reusable mock of Anthropic's
// POST /v1/messages endpoint for kahyad's W1-2 hermetic acceptance gate
// (tests/e2e/w12_gate_test.go, W12-10) and for future record-replay work
// (HANDOFF §6 W7-8). It:
//
//   - records the full request (method, path, headers, raw body bytes) for
//     every call it receives, so a test can assert on exactly what the
//     real worker -> claude-agent-sdk -> claude CLI chain actually sent
//     upstream (in particular, that a <hafiza> injection block reached the
//     model call itself, not just kahyad's own UserPromptSubmit hook); and
//   - streams back a FIXED Turkish SSE answer (AnswerText) shaped exactly
//     like a real Anthropic streaming response - message_start (with
//     input/cache usage fields), content_block_start, a few
//     content_block_delta text_delta events whose concatenation
//     reconstructs AnswerText exactly, content_block_stop, message_delta
//     (with output_tokens usage), message_stop - so
//     kahyad/internal/anthproxy/usage.go's SSEUsageAccumulator parses real
//     usage out of a real HTTP response, exercising the cost governor's
//     model_call ledger event on the real code path rather than a
//     hand-rolled shortcut.
//
// This is a mock of the WIRE PROTOCOL only: it does not attempt to
// interpret the request (routing, tool use, multi-turn history) - every
// call to POST /v1/messages* gets the exact same canned answer, which is
// sufficient for the W1-2 gate (the gate gates plumbing - injection,
// trace, ledger - not answer quality; HANDOFF §6 out-of-scope note).
package mockanthropic

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// AnswerText is the fixed Turkish answer this mock streams back for every
// /v1/messages call (W12-10 task spec step 2, verbatim).
const AnswerText = "Kadıköy öne çıkmıştı."

// AnswerChunks is AnswerText split into a few content_block_delta pieces
// purely to exercise incremental SSE delivery; concatenating every chunk,
// in order, reconstructs AnswerText exactly - byte for byte, no added or
// missing whitespace.
var AnswerChunks = []string{"Kadıköy", " öne", " çıkmıştı."}

// RecordedRequest is one POST this mock observed, captured before any
// response is written.
type RecordedRequest struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

// Server is the mock Anthropic server. Construct with New; it is listening
// immediately on return (httptest.NewServer semantics).
type Server struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []RecordedRequest
}

// New starts a new mock server bound to an ephemeral 127.0.0.1 port and
// returns it, ready to use as cfg.anthropic_upstream_url. Call Close when
// done.
func New() *Server {
	s := &Server{}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// URL is this mock's base URL (e.g. "http://127.0.0.1:54321"), suitable
// for kahyad's config.yaml anthropic_upstream_url key.
func (s *Server) URL() string { return s.srv.URL }

// Close shuts the mock server down. Safe to call once, typically via
// defer/t.Cleanup right after New.
func (s *Server) Close() { s.srv.Close() }

// Requests returns a snapshot of every request recorded so far, in arrival
// order. Safe to call concurrently with in-flight requests.
func (s *Server) Requests() []RecordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]RecordedRequest, len(s.requests))
	copy(out, s.requests)
	return out
}

// handle records the request, then - for anything under /v1/messages
// (Anthropic's SDK/CLI append a "?beta=true"-style query string on some
// calls; matching by path PREFIX, not exact-equality, is deliberate) -
// streams the fixed SSE answer. Any other path gets a plain 404: this mock
// exists to stand in for Anthropic's one endpoint the worker ever calls,
// nothing else.
func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	rec := RecordedRequest{
		Method: r.Method,
		Path:   r.URL.Path,
		Header: r.Header.Clone(),
		Body:   body,
	}
	s.mu.Lock()
	s.requests = append(s.requests, rec)
	s.mu.Unlock()

	if !strings.HasPrefix(r.URL.Path, "/v1/messages") {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, canFlush := w.(http.Flusher)

	writeSSE(w, "message_start", messageStartEvent())
	flushIf(flusher, canFlush)
	writeSSE(w, "content_block_start", map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
	flushIf(flusher, canFlush)
	for _, chunk := range AnswerChunks {
		writeSSE(w, "content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": chunk},
		})
		flushIf(flusher, canFlush)
	}
	writeSSE(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	flushIf(flusher, canFlush)
	writeSSE(w, "message_delta", messageDeltaEvent())
	flushIf(flusher, canFlush)
	writeSSE(w, "message_stop", map[string]any{"type": "message_stop"})
	flushIf(flusher, canFlush)
}

func flushIf(f http.Flusher, ok bool) {
	if ok {
		f.Flush()
	}
}

// messageStartEvent carries plausible input/cache usage numbers on
// message.usage - kahyad/internal/anthproxy/usage.go's
// sseMessageStart.Message.Usage reads exactly these three fields.
func messageStartEvent() map[string]any {
	return map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": "msg_mock_kahya_w12", "type": "message", "role": "assistant",
			"model":         "claude-sonnet-5",
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":                1500,
				"cache_creation_input_tokens": 0,
				"cache_read_input_tokens":     0,
				"output_tokens":               1,
			},
		},
	}
}

// messageDeltaEvent carries the cumulative output_tokens count
// usage.go's sseMessageDelta reads (Anthropic's own convention: this
// value is cumulative, not incremental - see SSEUsageAccumulator.Feed's
// doc comment).
func messageDeltaEvent() map[string]any {
	return map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": 12},
	}
}

// writeSSE writes one "event: <event>\ndata: <json>\n\n" frame - the exact
// framing kahyad/internal/anthproxy/usage.go's SSEUsageAccumulator.Feed
// expects ("data: " prefix, one JSON object per line, blank line between
// frames).
func writeSSE(w http.ResponseWriter, event string, data any) {
	b, err := json.Marshal(data)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
}

// FindHafizaBlock searches body (a JSON /v1/messages request) for the
// first string value anywhere in it that contains a complete
// "<hafiza>...</hafiza>" block, and returns that block VERBATIM as it
// exists in the DECODED JSON string (i.e. after JSON-unescaping - so the
// returned bytes are byte-identical to whatever produced them, not the
// wire-escaped form with literal "\n" sequences). This is what the W1-2
// gate's ledger-forensics subtest hashes and compares against the
// hafiza_injected ledger event's own block_sha256: the hook
// (worker/kahya_worker/hooks.py) returns kahyad's <hafiza> block
// byte-exact and unmodified as additionalContext, so if the CLI/SDK also
// carries it through unmodified into the model-call request body, sha256
// of what this function returns must equal sha256 of the ledgered block.
func FindHafizaBlock(body []byte) (string, bool) {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return "", false
	}
	return findHafizaIn(v)
}

const (
	hafizaOpen  = "<hafiza>"
	hafizaClose = "</hafiza>"
)

func findHafizaIn(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		start := strings.Index(t, hafizaOpen)
		if start < 0 {
			return "", false
		}
		rest := t[start:]
		end := strings.Index(rest, hafizaClose)
		if end < 0 {
			return "", false
		}
		return rest[:end+len(hafizaClose)], true
	case map[string]any:
		for _, vv := range t {
			if b, ok := findHafizaIn(vv); ok {
				return b, true
			}
		}
	case []any:
		for _, vv := range t {
			if b, ok := findHafizaIn(vv); ok {
				return b, true
			}
		}
	}
	return "", false
}
