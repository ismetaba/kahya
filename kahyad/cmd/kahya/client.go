// client.go is the kahya CLI's UDS HTTP client (W12-06 deliverable): one
// http.Client whose Transport dials kahyad's control socket instead of TCP,
// plus request builders/response parsers for every route this CLI speaks -
// GET /health, GET /v1/log, POST /v1/reindex, POST /v1/task (SSE) - and the
// SSE frame reader POST /v1/task streams over.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"kahya/kahyad/internal/config"
)

const (
	// connectTimeout bounds only the initial UDS dial (W12-06 step 7).
	connectTimeout = 2 * time.Second

	// idleReadTimeout bounds the gap between bytes on a /v1/task SSE
	// stream. There is deliberately no overall deadline on the stream
	// itself - long tasks may legitimately run for a long time - but 30s
	// with no byte at all means something is wrong (W12-06 step 7).
	idleReadTimeout = 30 * time.Second
)

// resolveSocket returns the UDS path the CLI should dial: the KAHYA_SOCKET
// env override if set, else kahyad's own resolved default (config.Load
// applies the identical defaulting/override rules kahyad itself uses, so
// the CLI and daemon always agree on the socket path without kahya
// hand-rolling a second copy of that logic).
func resolveSocket() (string, error) {
	if v := os.Getenv("KAHYA_SOCKET"); v != "" {
		return v, nil
	}
	cfg, err := config.Load()
	if err != nil {
		return "", err
	}
	return cfg.Socket, nil
}

// unreachableError renders as the exact MsgDaemonUnreachable string (socket
// path substituted) for any failure that means the CLI could not carry out
// a request against kahyad at all: a dial failure, a non-2xx/unexpected
// response, or (until W12-07 lands /v1/task) the 404 that route currently
// answers. Task spec: "ask/REPL must fail GRACEFULLY with the Turkish
// daemon-error string, NOT crash" - every such case is funneled through
// this one type so callers render one Turkish line and exit 2, uniformly.
type unreachableError struct {
	sock string
	err  error
}

func (e *unreachableError) Error() string { return fmt.Sprintf(MsgDaemonUnreachable, e.sock) }
func (e *unreachableError) Unwrap() error { return e.err }

// idleTimeoutError renders as the exact MsgIdleTimeout string (trace_id
// substituted).
type idleTimeoutError struct {
	traceID string
}

func (e *idleTimeoutError) Error() string { return fmt.Sprintf(MsgIdleTimeout, e.traceID) }

// Client talks HTTP-over-UDS to kahyad.
type Client struct {
	sock string
	http *http.Client
}

// newClient builds a Client dialing sock. It sets no blanket client
// Timeout: /v1/task's SSE stream can legitimately run long, so only the
// dial (connectTimeout) and the SSE idle-read gap (idleReadTimeout,
// enforced in readSSE) are bounded.
func newClient(sock string) *Client {
	return &Client{
		sock: sock,
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					d := net.Dialer{Timeout: connectTimeout}
					return d.DialContext(ctx, "unix", sock)
				},
			},
		},
	}
}

// newRequest builds a request against the fake "kahyad" host (only the UDS
// dial matters; the hostname is never resolved) and sets the one header
// every kahyad route expects (W12-06 step 7: "Every request sets
// X-Kahya-Trace-Id").
func (c *Client) newRequest(ctx context.Context, method, path, traceID string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://kahyad"+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Kahya-Trace-Id", traceID)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// do runs req, wrapping any transport-level failure (dial refused, dial
// timeout, connection reset mid-request) as an *unreachableError so every
// caller renders the same Turkish string.
func (c *Client) do(req *http.Request) (*http.Response, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &unreachableError{sock: c.sock, err: err}
	}
	return resp, nil
}

// apiError decodes a kahyad {"error": "..."} body, kahyad's uniform error
// shape (server.writeJSONError). Returns "" if the body doesn't match that
// shape.
func apiError(body io.Reader) string {
	var e struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(body).Decode(&e)
	return e.Error
}

// healthResponse mirrors kahyad's GET /health JSON body
// (kahyad/internal/server.healthResponse).
type healthResponse struct {
	Status        string `json:"status"`
	PID           int    `json:"pid"`
	UptimeS       int64  `json:"uptime_s"`
	Version       string `json:"version"`
	DB            string `json:"db"`
	SchemaVersion int64  `json:"schema_version"`
}

// Health calls GET /health.
func (c *Client) Health(ctx context.Context, traceID string) (healthResponse, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/health", traceID, nil)
	if err != nil {
		return healthResponse{}, err
	}
	resp, err := c.do(req)
	if err != nil {
		return healthResponse{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return healthResponse{}, &unreachableError{sock: c.sock, err: fmt.Errorf("health: status %d", resp.StatusCode)}
	}
	var hr healthResponse
	if err := json.NewDecoder(resp.Body).Decode(&hr); err != nil {
		return healthResponse{}, &unreachableError{sock: c.sock, err: fmt.Errorf("health: decode response: %w", err)}
	}
	return hr, nil
}

// reindexResponse mirrors kahyad's POST /v1/reindex JSON body
// (kahyad/internal/server.reindexResponse, W12-04).
type reindexResponse struct {
	FilesIndexed   int   `json:"files_indexed"`
	FilesUnchanged int   `json:"files_unchanged"`
	FilesRemoved   int   `json:"files_removed"`
	Chunks         int   `json:"chunks"`
	DurationMs     int64 `json:"duration_ms"`
}

// Reindex calls POST /v1/reindex {"full": full}.
func (c *Client) Reindex(ctx context.Context, traceID string, full bool) (reindexResponse, error) {
	body, err := json.Marshal(map[string]bool{"full": full})
	if err != nil {
		return reindexResponse{}, err
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/v1/reindex", traceID, bytes.NewReader(body))
	if err != nil {
		return reindexResponse{}, err
	}
	resp, err := c.do(req)
	if err != nil {
		return reindexResponse{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if msg := apiError(resp.Body); msg != "" {
			return reindexResponse{}, fmt.Errorf("%s", msg)
		}
		return reindexResponse{}, &unreachableError{sock: c.sock, err: fmt.Errorf("reindex: status %d", resp.StatusCode)}
	}
	var rr reindexResponse
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		return reindexResponse{}, &unreachableError{sock: c.sock, err: fmt.Errorf("reindex: decode response: %w", err)}
	}
	return rr, nil
}

// Log calls GET /v1/log?trace_id=queryTraceID and returns the decoded
// "lines" array (kahyad/internal/server.logLineResponse). traceID is the
// X-Kahya-Trace-Id this request itself carries (a freshly minted one - it
// need not match queryTraceID, the trace being looked up).
func (c *Client) Log(ctx context.Context, traceID, queryTraceID string) ([]map[string]any, error) {
	q := url.Values{"trace_id": {queryTraceID}}
	req, err := c.newRequest(ctx, http.MethodGet, "/v1/log?"+q.Encode(), traceID, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if msg := apiError(resp.Body); msg != "" {
			return nil, fmt.Errorf("%s", msg)
		}
		return nil, &unreachableError{sock: c.sock, err: fmt.Errorf("log: status %d", resp.StatusCode)}
	}
	var lr struct {
		Lines []map[string]any `json:"lines"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		return nil, &unreachableError{sock: c.sock, err: fmt.Errorf("log: decode response: %w", err)}
	}
	return lr.Lines, nil
}

// taskResult is POST /v1/task's terminal outcome, assembled from whichever
// of the "result"/"error" SSE events arrives first (W12-06 contract, see
// the task spec's Context section).
type taskResult struct {
	Status    string // "ok" | "error"
	TaskID    string
	SessionID string
	ErrMsg    string // set only when the server sent an "error" event; Turkish, user-facing as-is
}

// sseEvent is one parsed "event: <type>\ndata: <payload>" SSE frame.
type sseEvent struct {
	event string
	data  []byte
}

// StreamTask calls POST /v1/task {"prompt","trace_id":traceID} and reads
// the text/event-stream response, invoking onDelta with each "delta"
// event's text as it arrives (in order), and returning once a "result" or
// "error" event is seen. traceID is minted by the CALLER (main.go), not
// here - per the task spec, the CLI mints its own trace_id specifically so
// it can still print it even when this call fails outright.
func (c *Client) StreamTask(ctx context.Context, traceID, prompt string, onDelta func(string)) (taskResult, error) {
	body, err := json.Marshal(map[string]string{"prompt": prompt, "trace_id": traceID})
	if err != nil {
		return taskResult{}, err
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/v1/task", traceID, bytes.NewReader(body))
	if err != nil {
		return taskResult{}, err
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.do(req)
	if err != nil {
		return taskResult{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		// Covers both a genuine transport problem and (until W12-07 lands
		// the real handler) today's 404 - both fail gracefully as
		// "daemon unreachable" per the task spec, rather than a raw HTTP
		// status leaking to the user.
		return taskResult{}, &unreachableError{sock: c.sock, err: fmt.Errorf("task: status=%d content-type=%q", resp.StatusCode, resp.Header.Get("Content-Type"))}
	}

	var result taskResult
	gotResult := false
	err = readSSE(resp.Body, idleReadTimeout, traceID, func(ev sseEvent) (stop bool) {
		switch ev.event {
		case "delta":
			var d struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(ev.data, &d) == nil && onDelta != nil {
				onDelta(d.Text)
			}
		case "result":
			var r struct {
				Status    string `json:"status"`
				TaskID    string `json:"task_id"`
				SessionID string `json:"session_id"`
			}
			if json.Unmarshal(ev.data, &r) == nil {
				result = taskResult{Status: r.Status, TaskID: r.TaskID, SessionID: r.SessionID}
				gotResult = true
			}
			return true
		case "error":
			var e struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(ev.data, &e) == nil {
				result = taskResult{Status: "error", ErrMsg: e.Message}
				gotResult = true
			}
			return true
		}
		return false
	})
	if err != nil {
		return taskResult{}, err
	}
	if !gotResult {
		return taskResult{}, &unreachableError{sock: c.sock, err: fmt.Errorf("task: stream ended without a result/error event")}
	}
	return result, nil
}

// readSSE parses "event: X" / "data: Y" SSE frames from body (blank line =
// dispatch), calling handle for each complete frame until handle returns
// true or the stream ends. It enforces idleTimeout between reads - not an
// overall deadline, since long tasks may legitimately run for a long time -
// by running the blocking line-scan on its own goroutine and racing each
// line against a timer; if idleTimeout elapses with no line at all, it
// returns an *idleTimeoutError. The scanning goroutine is intentionally not
// joined on that path: closing resp.Body (the caller's defer) unblocks its
// Read/Scan shortly after, and kahya is a short-lived CLI process where
// that is a harmless, process-exit-bounded leak, not a long-running server.
func readSSE(body io.Reader, idleTimeout time.Duration, traceID string, handle func(sseEvent) (stop bool)) error {
	lines := make(chan string, 16)
	scanErr := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(body)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			lines <- sc.Text()
		}
		if err := sc.Err(); err != nil {
			scanErr <- err
		}
		close(lines)
	}()

	var evType string
	var dataLines []string
	dispatch := func() bool {
		if evType == "" && len(dataLines) == 0 {
			return false
		}
		ev := sseEvent{event: evType, data: []byte(strings.Join(dataLines, "\n"))}
		evType, dataLines = "", nil
		return handle(ev)
	}

	for {
		select {
		case line, ok := <-lines:
			if !ok {
				select {
				case err := <-scanErr:
					return err
				default:
				}
				return nil
			}
			switch {
			case line == "":
				if dispatch() {
					return nil
				}
			case strings.HasPrefix(line, "event:"):
				evType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				dataLines = append(dataLines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			// Other SSE fields (id:, retry:) and comment lines (":...") are
			// not part of this protocol; ignore them rather than erroring.
			default:
			}
		case <-time.After(idleTimeout):
			return &idleTimeoutError{traceID: traceID}
		}
	}
}
