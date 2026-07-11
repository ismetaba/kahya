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
// hand-rolling a second copy of that logic). Both the env override and the
// resolved default are passed through config.ExpandHome so a "~/..."
// KAHYA_SOCKET value expands identically to how kahyad's own config.Load
// expands it (BLOCKER 1) - without this, the CLI would dial a literal
// "~/..." path while kahyad listens on the tilde-expanded one.
func resolveSocket() (string, error) {
	if v := os.Getenv("KAHYA_SOCKET"); v != "" {
		return config.ExpandHome(v), nil
	}
	cfg, err := config.Load()
	if err != nil {
		return "", err
	}
	return config.ExpandHome(cfg.Socket), nil
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

// streamIncompleteError renders as the exact MsgStreamIncomplete string
// (trace_id substituted). It is returned (MINOR 6) instead of
// unreachableError when a /v1/task SSE stream ends - via a clean close or a
// low-level read failure, readSSE does not distinguish the two - after at
// least one byte of the stream had already arrived but before a terminal
// "result"/"error" event: at that point the daemon was clearly reachable
// and the task may have progressed or even completed server-side, so
// "kahyad'a ulaşılamıyor" (daemon unreachable) would be misleading.
type streamIncompleteError struct {
	traceID string
}

func (e *streamIncompleteError) Error() string { return fmt.Sprintf(MsgStreamIncomplete, e.traceID) }

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

// Reindex calls POST /v1/reindex {"full": full, "re_embed": reEmbed}
// (W12-11 step 5 adds re_embed: the version-switch trigger - re-embeds
// EVERY chunk under the active embed model_ver and purges chunk_vec rows
// left under any other model_ver).
func (c *Client) Reindex(ctx context.Context, traceID string, full, reEmbed bool) (reindexResponse, error) {
	body, err := json.Marshal(map[string]bool{"full": full, "re_embed": reEmbed})
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

// policyStateRow mirrors kahyad's GET /policy/state row shape
// (kahyad/internal/server.policyStateRow, W3-02).
type policyStateRow struct {
	Tool                 string `json:"tool"`
	Class                string `json:"class"`
	Scope                string `json:"scope"`
	Level                int64  `json:"level"`
	ConsecutiveApprovals int64  `json:"consecutive_approvals"`
	UpdatedAt            string `json:"updated_at"`
}

// PolicyState calls GET /policy/state (`kahya autonomy`'s ladder dump).
func (c *Client) PolicyState(ctx context.Context, traceID string) ([]policyStateRow, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/policy/state", traceID, nil)
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
		return nil, &unreachableError{sock: c.sock, err: fmt.Errorf("policy/state: status %d", resp.StatusCode)}
	}
	var sr struct {
		States []policyStateRow `json:"states"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return nil, &unreachableError{sock: c.sock, err: fmt.Errorf("policy/state: decode response: %w", err)}
	}
	return sr.States, nil
}

// PolicyPromote calls POST /policy/promote {tool,class,scope}
// (`kahya autonomy promote <tool> <class> <scope>` - the ONLY promotion
// path, W3-02). Returns the new (post-promotion) level.
func (c *Client) PolicyPromote(ctx context.Context, traceID, tool, class, scope string) (int, error) {
	body, err := json.Marshal(map[string]string{"trace_id": traceID, "tool": tool, "class": class, "scope": scope})
	if err != nil {
		return 0, err
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/policy/promote", traceID, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	resp, err := c.do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var pr struct {
		Level int    `json:"level"`
		Error string `json:"error,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return 0, &unreachableError{sock: c.sock, err: fmt.Errorf("policy/promote: decode response: %w", err)}
	}
	if resp.StatusCode != http.StatusOK {
		if pr.Error != "" {
			return 0, fmt.Errorf("%s", pr.Error)
		}
		return 0, &unreachableError{sock: c.sock, err: fmt.Errorf("policy/promote: status %d", resp.StatusCode)}
	}
	return pr.Level, nil
}

// PolicyUndo calls POST /policy/undo {trace_id} (`kahya undo --trace
// <id>`, W3-02). Returns the tool name whose undo window was triggered.
func (c *Client) PolicyUndo(ctx context.Context, traceID, targetTraceID string) (string, error) {
	body, err := json.Marshal(map[string]string{"trace_id": targetTraceID})
	if err != nil {
		return "", err
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/policy/undo", traceID, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	resp, err := c.do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var ur struct {
		OK    bool   `json:"ok"`
		Tool  string `json:"tool,omitempty"`
		Error string `json:"error,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ur); err != nil {
		return "", &unreachableError{sock: c.sock, err: fmt.Errorf("policy/undo: decode response: %w", err)}
	}
	if resp.StatusCode != http.StatusOK || !ur.OK {
		if ur.Error != "" {
			return "", fmt.Errorf("%s", ur.Error)
		}
		return "", &unreachableError{sock: c.sock, err: fmt.Errorf("policy/undo: status %d", resp.StatusCode)}
	}
	return ur.Tool, nil
}

// approvalListRow mirrors kahyad's GET /policy/approvals list row shape
// (kahyad/internal/server.policyApprovalsListRow, W3-06).
type approvalListRow struct {
	ID       string `json:"id"`
	Tool     string `json:"tool"`
	Class    string `json:"class"`
	Scope    string `json:"scope"`
	Summary  string `json:"summary"`
	AgeS     int64  `json:"age_s"`
	MintedAt string `json:"minted_at"`
}

// ListApprovals calls GET /policy/approvals (`kahya approvals`).
func (c *Client) ListApprovals(ctx context.Context, traceID string) ([]approvalListRow, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/policy/approvals", traceID, nil)
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
		return nil, &unreachableError{sock: c.sock, err: fmt.Errorf("policy/approvals: status %d", resp.StatusCode)}
	}
	var lr struct {
		Approvals []approvalListRow `json:"approvals"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		return nil, &unreachableError{sock: c.sock, err: fmt.Errorf("policy/approvals: decode response: %w", err)}
	}
	return lr.Approvals, nil
}

// approvalDetail mirrors kahyad's GET /policy/approvals?id=<id> response
// shape (kahyad/internal/server.policyApprovalDetailResponse, W3-06): the
// full rendered WYSIWYE diff text `kahya approve <id>` prints verbatim
// before prompting.
type approvalDetail struct {
	ID       string `json:"id"`
	Tool     string `json:"tool"`
	Class    string `json:"class"`
	Scope    string `json:"scope"`
	Rendered string `json:"rendered"`
	Error    string `json:"error,omitempty"`
}

// GetApproval calls GET /policy/approvals?id=<id> (`kahya approve <id>`'s
// own read-only lookup - does not consume the pending approval).
func (c *Client) GetApproval(ctx context.Context, traceID, id string) (approvalDetail, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/policy/approvals?id="+url.QueryEscape(id), traceID, nil)
	if err != nil {
		return approvalDetail{}, err
	}
	resp, err := c.do(req)
	if err != nil {
		return approvalDetail{}, err
	}
	defer resp.Body.Close()
	var ad approvalDetail
	if err := json.NewDecoder(resp.Body).Decode(&ad); err != nil {
		return approvalDetail{}, &unreachableError{sock: c.sock, err: fmt.Errorf("policy/approvals detail: decode response: %w", err)}
	}
	if resp.StatusCode != http.StatusOK {
		if ad.Error != "" {
			return approvalDetail{}, fmt.Errorf("%s", ad.Error)
		}
		return approvalDetail{}, &unreachableError{sock: c.sock, err: fmt.Errorf("policy/approvals detail: status %d", resp.StatusCode)}
	}
	return ad, nil
}

// PolicyFeedback calls POST /policy/feedback {kind, pending_approval_id,
// surface} (`kahya approve <id>`'s own approve/deny outcome, W3-02/W3-06).
// surface is always "local" for an approve issued from this CLI (HANDOFF
// §5 safety #5: the CLI IS the local surface at W3-W5) - callers pass ""
// for kind="deny", which ignores surface entirely. Returns the minted
// token on a successful approve ("" for deny).
func (c *Client) PolicyFeedback(ctx context.Context, traceID, kind, pendingApprovalID, surface string) (string, error) {
	body, err := json.Marshal(map[string]string{
		"kind": kind, "pending_approval_id": pendingApprovalID, "surface": surface, "trace_id": traceID,
	})
	if err != nil {
		return "", err
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/policy/feedback", traceID, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	resp, err := c.do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var fr struct {
		OK    bool   `json:"ok"`
		Token string `json:"token,omitempty"`
		Error string `json:"error,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&fr); err != nil {
		return "", &unreachableError{sock: c.sock, err: fmt.Errorf("policy/feedback: decode response: %w", err)}
	}
	if !fr.OK {
		if fr.Error != "" {
			return "", fmt.Errorf("%s", fr.Error)
		}
		return "", &unreachableError{sock: c.sock, err: fmt.Errorf("policy/feedback: status %d", resp.StatusCode)}
	}
	return fr.Token, nil
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
	// ProcessedLocally is W3-08's "result" field: true iff this task was
	// answered entirely by the local secret-lane Qwen3-30B-A3B server
	// (kahyad/internal/server.handleSecretLaneTask), never a cloud model -
	// main.go prints the "🔒 yerel işlendi" badge when this is true.
	ProcessedLocally bool
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
	sawAny, err := readSSE(resp.Body, idleReadTimeout, traceID, func(ev sseEvent) (stop bool) {
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
				// Message is an optional explanation the server may attach
				// to a status="error" result (MINOR 5); when absent,
				// MsgTaskFailed is used below so an error result is never
				// silently swallowed with no stderr output.
				Message string `json:"message"`
				// ProcessedLocally is W3-08's CLI-badge field.
				ProcessedLocally bool `json:"processed_locally"`
			}
			if json.Unmarshal(ev.data, &r) == nil {
				result = taskResult{Status: r.Status, TaskID: r.TaskID, SessionID: r.SessionID, ProcessedLocally: r.ProcessedLocally}
				if r.Status == "error" {
					if r.Message != "" {
						result.ErrMsg = r.Message
					} else {
						result.ErrMsg = MsgTaskFailed
					}
				}
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
		// idleTimeoutError already renders its own distinct, correct Turkish
		// string; let it propagate as-is. Any other error from readSSE is a
		// low-level read failure (BLOCKER 4) that must never reach the user
		// as a raw Go error - fold it into the same "reachable but
		// incomplete" vs. "unreachable" distinction as a clean mid-stream
		// close (MINOR 6), based on whether any bytes had already arrived.
		if _, isIdle := err.(*idleTimeoutError); isIdle {
			return taskResult{}, err
		}
		if sawAny {
			return taskResult{}, &streamIncompleteError{traceID: traceID}
		}
		return taskResult{}, &unreachableError{sock: c.sock, err: err}
	}
	if !gotResult {
		// The stream ended cleanly (EOF, no read error) without a
		// result/error event ever arriving. A connection that produced at
		// least one byte was reachable and the task may have progressed or
		// even completed server-side (MINOR 6) - only a stream that never
		// produced anything at all is genuinely "unreachable".
		if sawAny {
			return taskResult{}, &streamIncompleteError{traceID: traceID}
		}
		return taskResult{}, &unreachableError{sock: c.sock, err: fmt.Errorf("task: stream ended without a result/error event")}
	}
	return result, nil
}

// readSSE parses "event: X" / "data: Y" SSE frames from body (blank line =
// dispatch), calling handle for each complete frame until handle returns
// true or the stream ends. It enforces idleTimeout between reads - not an
// overall deadline, since long tasks may legitimately run for a long time -
// by running the blocking line-read on its own goroutine and racing each
// line against a timer; if idleTimeout elapses with no line at all, it
// returns an *idleTimeoutError. The reading goroutine is intentionally not
// joined on that path: closing resp.Body (the caller's defer) unblocks its
// Read shortly after, and kahya is a short-lived CLI process where that is
// a harmless, process-exit-bounded leak, not a long-running server.
//
// Lines are read with a bufio.Reader + ReadString('\n') loop rather than
// bufio.Scanner (BLOCKER 4): Scanner's default 1MB token cap would otherwise
// turn one oversized SSE data line into the raw, English
// "bufio.Scanner: token too long", violating the Turkish-only user-facing
// string policy. ReadString has no such cap, and any other low-level read
// error it does surface is returned as-is here for the caller to translate
// into a Turkish message (StreamTask does so, never printing it raw).
//
// The bool return reports whether at least one line was ever read off body,
// so callers can distinguish "never reachable" from "reachable, but the
// stream ended - cleanly or via a read error - before a terminal event"
// (MINOR 6).
func readSSE(body io.Reader, idleTimeout time.Duration, traceID string, handle func(sseEvent) (stop bool)) (sawAny bool, err error) {
	lines := make(chan string, 16)
	readErrCh := make(chan error, 1)
	go func() {
		r := bufio.NewReader(body)
		for {
			text, err := r.ReadString('\n')
			if len(text) > 0 {
				lines <- strings.TrimSuffix(strings.TrimSuffix(text, "\n"), "\r")
			}
			if err != nil {
				if err != io.EOF {
					readErrCh <- err
				}
				break
			}
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
				case err := <-readErrCh:
					return sawAny, err
				default:
				}
				return sawAny, nil
			}
			sawAny = true
			switch {
			case line == "":
				if dispatch() {
					return sawAny, nil
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
			return sawAny, &idleTimeoutError{traceID: traceID}
		}
	}
}
