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
	"errors"
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

// jobTriggerResponse mirrors kahyad's POST /jobs/trigger/{name} 202 body
// (kahyad/internal/server.jobTriggerResponse, W4-01).
type jobTriggerResponse struct {
	TraceID string `json:"trace_id"`
}

// TriggerJob calls POST /jobs/trigger/{name} (W4-01's ONE job-dispatch
// route - kahyad-trigger, launchd, and `kahya job run <name>` all reach
// the exact same handler, so a manual trigger can never behave
// differently than a scheduled one). Returns the freshly-minted trace_id
// kahyad's response carries, so the caller can point `kahya log --trace
// <id>` at this exact run.
func (c *Client) TriggerJob(ctx context.Context, traceID, name string) (string, error) {
	req, err := c.newRequest(ctx, http.MethodPost, "/jobs/trigger/"+url.PathEscape(name), traceID, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		if msg := apiError(resp.Body); msg != "" {
			return "", fmt.Errorf("%s", msg)
		}
		return "", &unreachableError{sock: c.sock, err: fmt.Errorf("job trigger: status %d", resp.StatusCode)}
	}
	var jr jobTriggerResponse
	if err := json.NewDecoder(resp.Body).Decode(&jr); err != nil {
		return "", &unreachableError{sock: c.sock, err: fmt.Errorf("job trigger: decode response: %w", err)}
	}
	return jr.TraceID, nil
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
// surface, typed} (`kahya approve <id>`'s own approve/deny outcome,
// W3-02/W3-06/W6-01). surface is always "local" for an approve issued
// from this CLI (HANDOFF §5 safety #5: the CLI IS the local surface at
// W3-W5) - callers pass "" for kind="deny", which ignores surface
// entirely. typed is W6-01's addition: the exact text the human typed for
// a W3 approval's confirmation prompt, forwarded so kahyad can verify it
// server-side (byte-exact "onayla" after NFC normalization) - the
// authoritative check; ignored server-side for a non-W3 approval, so
// callers may always pass "" for W1/W2/deny. Returns the minted token on
// a successful approve ("" for deny).
func (c *Client) PolicyFeedback(ctx context.Context, traceID, kind, pendingApprovalID, surface, typed string) (string, error) {
	body, err := json.Marshal(map[string]string{
		"kind": kind, "pending_approval_id": pendingApprovalID, "surface": surface, "typed": typed, "trace_id": traceID,
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

// ApprovalDecision calls POST /approvals/{id}/decision {approve, typed}
// (W6-01) - the non-interactive counterpart to PolicyFeedback used by
// `kahya approvals decide` and (indirectly, via that subcommand)
// hammerspoon/kahya.lua's approval cards. No surface field is ever sent -
// kahyad itself stamps surface="local" from the UDS channel (see
// kahyad/internal/server's handleApprovalsDecision doc comment); there is
// no wire field on this route for a caller to forge one through. Returns
// the minted token on a successful approve ("" on reject).
func (c *Client) ApprovalDecision(ctx context.Context, traceID, id string, approve bool, typed string) (string, error) {
	body, err := json.Marshal(map[string]any{"approve": approve, "typed": typed})
	if err != nil {
		return "", err
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/approvals/"+url.PathEscape(id)+"/decision", traceID, bytes.NewReader(body))
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
		return "", &unreachableError{sock: c.sock, err: fmt.Errorf("approvals/decision: decode response: %w", err)}
	}
	if !fr.OK {
		if fr.Error != "" {
			return "", fmt.Errorf("%s", fr.Error)
		}
		return "", &unreachableError{sock: c.sock, err: fmt.Errorf("approvals/decision: status %d", resp.StatusCode)}
	}
	return fr.Token, nil
}

// DebugEmitApproval calls POST /debug/emit-approval {class} (W6-01):
// `kahya debug emit-approval --class W2|W3`'s own server-side call.
// kahyad itself refuses (403) unless it is running under KAHYA_ENV=dev,
// regardless of what this CLI's own client-side env check already did.
// Returns the freshly minted pending_approval_id.
func (c *Client) DebugEmitApproval(ctx context.Context, traceID, class string) (string, error) {
	body, err := json.Marshal(map[string]string{"class": class})
	if err != nil {
		return "", err
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/debug/emit-approval", traceID, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	resp, err := c.do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var dr struct {
		ID    string `json:"id,omitempty"`
		Error string `json:"error,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&dr); err != nil {
		return "", &unreachableError{sock: c.sock, err: fmt.Errorf("debug/emit-approval: decode response: %w", err)}
	}
	if resp.StatusCode != http.StatusOK {
		if dr.Error != "" {
			return "", fmt.Errorf("%s", dr.Error)
		}
		return "", &unreachableError{sock: c.sock, err: fmt.Errorf("debug/emit-approval: status %d", resp.StatusCode)}
	}
	return dr.ID, nil
}

// taskStatusToolCall mirrors kahyad's GET /v1/task/status response's
// tool_calls row shape (kahyad/internal/server.taskStatusToolCallView,
// W4-02).
type taskStatusToolCall struct {
	Seq      int64  `json:"seq"`
	Tool     string `json:"tool"`
	Class    string `json:"class"`
	Status   string `json:"status"`
	ArgsHash string `json:"args_hash"`
}

// taskStatus mirrors kahyad's GET /v1/task/status response body
// (kahyad/internal/server.taskStatusResponse, W4-02).
type taskStatus struct {
	ID        string               `json:"id"`
	Status    string               `json:"status"`
	SessionID string               `json:"session_id,omitempty"`
	Attempts  int64                `json:"attempts"`
	PID       int                  `json:"pid,omitempty"`
	ToolCalls []taskStatusToolCall `json:"tool_calls"`
}

// TaskStatus calls GET /v1/task/status?id=<id> (`kahya task show <id>`).
func (c *Client) TaskStatus(ctx context.Context, traceID, taskID string) (taskStatus, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/v1/task/status?id="+url.QueryEscape(taskID), traceID, nil)
	if err != nil {
		return taskStatus{}, err
	}
	resp, err := c.do(req)
	if err != nil {
		return taskStatus{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if msg := apiError(resp.Body); msg != "" {
			return taskStatus{}, fmt.Errorf("%s", msg)
		}
		return taskStatus{}, &unreachableError{sock: c.sock, err: fmt.Errorf("task/status: status %d", resp.StatusCode)}
	}
	var ts taskStatus
	if err := json.NewDecoder(resp.Body).Decode(&ts); err != nil {
		return taskStatus{}, &unreachableError{sock: c.sock, err: fmt.Errorf("task/status: decode response: %w", err)}
	}
	return ts, nil
}

// TaskResolve calls POST /v1/task/resolve {task_id, action, trace_id}
// (`kahya task resolve <id> --retry|--abort`). action is "retry" or
// "abort".
func (c *Client) TaskResolve(ctx context.Context, traceID, taskID, action string) error {
	body, err := json.Marshal(map[string]string{"task_id": taskID, "action": action, "trace_id": traceID})
	if err != nil {
		return err
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/v1/task/resolve", traceID, bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var rr struct {
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		return &unreachableError{sock: c.sock, err: fmt.Errorf("task/resolve: decode response: %w", err)}
	}
	if !rr.OK {
		if rr.Error != "" {
			return fmt.Errorf("%s", rr.Error)
		}
		return &unreachableError{sock: c.sock, err: fmt.Errorf("task/resolve: status %d", resp.StatusCode)}
	}
	return nil
}

// ledgerVerifyResult mirrors kahyad's POST /v1/ledger/verify JSON body
// (kahyad/internal/server.ledgerVerifyResponse, W4-05). Message (only set
// when OK is false) is already the exact Turkish AlarmMismatch string -
// `kahya ledger verify` prints it verbatim, never re-wrapped.
type ledgerVerifyResult struct {
	OK              bool   `json:"ok"`
	MismatchEventID int64  `json:"mismatch_event_id,omitempty"`
	Message         string `json:"message,omitempty"`
}

// LedgerVerify calls POST /v1/ledger/verify (`kahya ledger verify`, W4-05):
// recomputes the ledger digest from event 1 forward and compares it
// against every anchored checkpoint (kahyad itself performs the recompute
// and any resulting anchor.mismatch ledgering - kahya is brain.db's only
// writer).
func (c *Client) LedgerVerify(ctx context.Context, traceID string) (ledgerVerifyResult, error) {
	req, err := c.newRequest(ctx, http.MethodPost, "/v1/ledger/verify", traceID, bytes.NewReader([]byte("{}")))
	if err != nil {
		return ledgerVerifyResult{}, err
	}
	resp, err := c.do(req)
	if err != nil {
		return ledgerVerifyResult{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if msg := apiError(resp.Body); msg != "" {
			return ledgerVerifyResult{}, fmt.Errorf("%s", msg)
		}
		return ledgerVerifyResult{}, &unreachableError{sock: c.sock, err: fmt.Errorf("ledger/verify: status %d", resp.StatusCode)}
	}
	var lr ledgerVerifyResult
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		return ledgerVerifyResult{}, &unreachableError{sock: c.sock, err: fmt.Errorf("ledger/verify: decode response: %w", err)}
	}
	return lr, nil
}

// consolidationShowResponse mirrors kahyad's GET /v1/consolidation JSON
// body (kahyad/internal/server.consolidationShowResponse, W5-02).
type consolidationShowResponse struct {
	Found bool   `json:"found"`
	Diff  string `json:"diff,omitempty"`
}

// ShowConsolidation calls GET /v1/consolidation (`kahya consolidation
// show`).
func (c *Client) ShowConsolidation(ctx context.Context, traceID string) (found bool, diff string, err error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/v1/consolidation", traceID, nil)
	if err != nil {
		return false, "", err
	}
	resp, err := c.do(req)
	if err != nil {
		return false, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if msg := apiError(resp.Body); msg != "" {
			return false, "", fmt.Errorf("%s", msg)
		}
		return false, "", &unreachableError{sock: c.sock, err: fmt.Errorf("consolidation: status %d", resp.StatusCode)}
	}
	var sr consolidationShowResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return false, "", &unreachableError{sock: c.sock, err: fmt.Errorf("consolidation: decode response: %w", err)}
	}
	return sr.Found, sr.Diff, nil
}

// consolidationActionResponse mirrors kahyad's POST /v1/consolidation/
// approve|reject JSON body (kahyad/internal/server.consolidationActionResponse).
type consolidationActionResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// errConsolidationNoPending is returned by ApproveConsolidation/
// RejectConsolidation when kahyad answers 404 (no pending suggestion,
// kahyad/internal/consolidation.ErrNoPending) - main.go checks this via
// errors.Is to print the Turkish MsgConsolidationEmpty line rather than a
// raw (English) server error string.
var errConsolidationNoPending = errors.New("consolidation: no pending suggestion")

// ApproveConsolidation calls POST /v1/consolidation/approve (`kahya
// consolidation approve`, after the local 'onayla' confirm).
func (c *Client) ApproveConsolidation(ctx context.Context, traceID string) error {
	return c.consolidationAction(ctx, traceID, "/v1/consolidation/approve")
}

// RejectConsolidation calls POST /v1/consolidation/reject (`kahya
// consolidation reject`).
func (c *Client) RejectConsolidation(ctx context.Context, traceID string) error {
	return c.consolidationAction(ctx, traceID, "/v1/consolidation/reject")
}

func (c *Client) consolidationAction(ctx context.Context, traceID, path string) error {
	req, err := c.newRequest(ctx, http.MethodPost, path, traceID, bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return errConsolidationNoPending
	}
	var ar consolidationActionResponse
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		return &unreachableError{sock: c.sock, err: fmt.Errorf("%s: decode response: %w", path, err)}
	}
	if !ar.OK {
		if ar.Error != "" {
			return fmt.Errorf("%s", ar.Error)
		}
		return &unreachableError{sock: c.sock, err: fmt.Errorf("%s: status %d", path, resp.StatusCode)}
	}
	return nil
}

// factActionResponse mirrors kahyad's POST /v1/fact/*, /v1/entity/* JSON
// body (kahyad/internal/server.factActionResponse, W5-04).
type factActionResponse struct {
	OK    bool   `json:"ok"`
	ID    int64  `json:"id,omitempty"`
	Error string `json:"error,omitempty"`
}

// factAction POSTs body (already-marshaled JSON) to path and decodes a
// factActionResponse - the shared plumbing every one of the four W5-04
// fact/entity routes below uses, mirroring consolidationAction's identical
// shape.
func (c *Client) factAction(ctx context.Context, traceID, path string, body any) (int64, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return 0, fmt.Errorf("%s: marshal request: %w", path, err)
	}
	req, err := c.newRequest(ctx, http.MethodPost, path, traceID, bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	resp, err := c.do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	var ar factActionResponse
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		return 0, &unreachableError{sock: c.sock, err: fmt.Errorf("%s: decode response: %w", path, err)}
	}
	if !ar.OK {
		if ar.Error != "" {
			return 0, fmt.Errorf("%s", ar.Error)
		}
		return 0, &unreachableError{sock: c.sock, err: fmt.Errorf("%s: status %d", path, resp.StatusCode)}
	}
	return ar.ID, nil
}

// ConfirmFact calls POST /v1/fact/confirm (`kahya fact confirm <id>`).
func (c *Client) ConfirmFact(ctx context.Context, traceID string, factID int64) error {
	_, err := c.factAction(ctx, traceID, "/v1/fact/confirm", map[string]any{"fact_id": factID})
	return err
}

// RetractFact calls POST /v1/fact/retract (`kahya fact retract <özne>
// <yüklem> <nesne> [oturum_id]`), returning the retracted fact's id.
func (c *Client) RetractFact(ctx context.Context, traceID, subject, predicate, object, sessionID string) (int64, error) {
	return c.factAction(ctx, traceID, "/v1/fact/retract", map[string]any{
		"subject": subject, "predicate": predicate, "object": object, "session_id": sessionID,
	})
}

// MergeEntities calls POST /v1/entity/merge (`kahya entity merge <a> <b>
// --evidence <fact_id>`: b merges INTO a), returning the new
// merge_ledger row's id.
func (c *Client) MergeEntities(ctx context.Context, traceID string, dstEntityID, srcEntityID, evidenceFactID int64, actor string) (int64, error) {
	return c.factAction(ctx, traceID, "/v1/entity/merge", map[string]any{
		"dst_entity_id": dstEntityID, "src_entity_id": srcEntityID, "evidence_fact_id": evidenceFactID, "actor": actor,
	})
}

// SplitEntities calls POST /v1/entity/split (`kahya entity split
// <merge_ledger_id>`), returning the new split merge_ledger row's id.
func (c *Client) SplitEntities(ctx context.Context, traceID string, mergeLedgerID int64, actor string) (int64, error) {
	return c.factAction(ctx, traceID, "/v1/entity/split", map[string]any{
		"merge_ledger_id": mergeLedgerID, "actor": actor,
	})
}

// rememberedResponse mirrors kahyad's POST /v1/remembered JSON body
// (kahyad/internal/server.rememberedResponse, W5-03).
type rememberedResponse struct {
	OK        bool   `json:"ok"`
	Duplicate bool   `json:"duplicate,omitempty"`
	Error     string `json:"error,omitempty"`
}

// MarkRemembered calls POST /v1/remembered {trace_id, channel} (`kahya
// remembered --trace <id>`, channel="local" - the CLI IS the local
// surface, HANDOFF §5 safety #5's "local"/"remote" split). Returns
// duplicate=true when targetTraceID was already marked (still not an
// error - re-marking is idempotent by design, W5-03 task spec).
func (c *Client) MarkRemembered(ctx context.Context, traceID, targetTraceID string) (duplicate bool, err error) {
	body, err := json.Marshal(map[string]string{"trace_id": targetTraceID, "channel": "local"})
	if err != nil {
		return false, err
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/v1/remembered", traceID, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	resp, err := c.do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	var rr rememberedResponse
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		return false, &unreachableError{sock: c.sock, err: fmt.Errorf("remembered: decode response: %w", err)}
	}
	if !rr.OK {
		if rr.Error != "" {
			return false, fmt.Errorf("%s", rr.Error)
		}
		return false, &unreachableError{sock: c.sock, err: fmt.Errorf("remembered: status %d", resp.StatusCode)}
	}
	return rr.Duplicate, nil
}

// evalMiniQuestionResult mirrors kahyad's POST /v1/eval/mini/run JSON
// body's "results" row shape (kahyad/internal/server.evalMiniQuestionResult,
// W5-05).
type evalMiniQuestionResult struct {
	Q         string `json:"q"`
	Pass      bool   `json:"pass"`
	Abstained bool   `json:"abstained,omitempty"`
	Err       string `json:"err,omitempty"`
}

// evalMiniRunResult mirrors kahyad's POST /v1/eval/mini/run JSON body
// (kahyad/internal/server.evalMiniRunResponse, W5-05).
type evalMiniRunResult struct {
	Total         int                      `json:"total"`
	PassCount     int                      `json:"pass_count"`
	Results       []evalMiniQuestionResult `json:"results"`
	PreviousFound bool                     `json:"previous_found"`
	Regressed     bool                     `json:"regressed"`
	Reasons       []string                 `json:"reasons,omitempty"`
	Error         string                   `json:"error,omitempty"`
}

// EvalMiniRun calls POST /v1/eval/mini/run (`kahya eval mini`): kahyad runs
// the W5-05 retrieval mini-baseline against its own memory_search searcher
// and ledgers exactly one eval.mini.run event - this CLI process never
// opens brain.db itself, only ever this one UDS round-trip.
func (c *Client) EvalMiniRun(ctx context.Context, traceID string) (evalMiniRunResult, error) {
	req, err := c.newRequest(ctx, http.MethodPost, "/v1/eval/mini/run", traceID, bytes.NewReader([]byte("{}")))
	if err != nil {
		return evalMiniRunResult{}, err
	}
	resp, err := c.do(req)
	if err != nil {
		return evalMiniRunResult{}, err
	}
	defer resp.Body.Close()

	var rr evalMiniRunResult
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		return evalMiniRunResult{}, &unreachableError{sock: c.sock, err: fmt.Errorf("eval/mini/run: decode response: %w", err)}
	}
	if resp.StatusCode != http.StatusOK {
		if rr.Error != "" {
			return evalMiniRunResult{}, fmt.Errorf("%s", rr.Error)
		}
		return evalMiniRunResult{}, &unreachableError{sock: c.sock, err: fmt.Errorf("eval/mini/run: status %d", resp.StatusCode)}
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

// streamTaskRequest is POST /v1/task's request body (matches
// kahyad/internal/server.taskRequest exactly - W4-08 adds deep_think;
// W6-01 adds palette_opened_at; W6-02 adds input_audio_path).
type streamTaskRequest struct {
	Prompt          string   `json:"prompt"`
	TraceID         string   `json:"trace_id"`
	DeepThink       bool     `json:"deep_think,omitempty"`
	PaletteOpenedAt *float64 `json:"palette_opened_at,omitempty"`
	// InputAudioPath is `kahya ask --audio <path>`'s own field (W6-02):
	// an absolute, canonicalized path to a mono 16kHz wav. When set,
	// kahyad transcribes it ENTIRELY LOCALLY before doing anything else
	// with this task (kahyad/internal/server's own stt.go doc comment) -
	// Prompt is typically empty on the wire in this case (StreamTaskAudio
	// never sets it); the resulting transcript becomes the task's prompt
	// server-side.
	InputAudioPath string `json:"input_audio_path,omitempty"`
}

// StreamTask calls POST /v1/task {"prompt","trace_id":traceID,
// "deep_think":deepThink} and reads the text/event-stream response,
// invoking onDelta with each "delta" event's text as it arrives (in
// order), and returning once a "result" or "error" event is seen. traceID
// is minted by the CALLER (main.go), not here - per the task spec, the CLI
// mints its own trace_id specifically so it can still print it even when
// this call fails outright. deepThink is W4-08's `kahya ask --derin`
// opt-in (the OTHER opt-in form - the "derin düşün:" Turkish prompt prefix
// - is detected server-side, so it needs no client-side plumbing at all).
// This is StreamTaskFull's own paletteOpenedAt=nil shorthand - every
// pre-W6-01 caller (runOneShot, the REPL, `kahya ask` without
// --palette-opened-at) keeps using this exact signature unchanged.
func (c *Client) StreamTask(ctx context.Context, traceID, prompt string, deepThink bool, onDelta func(string)) (taskResult, error) {
	return c.StreamTaskFull(ctx, traceID, prompt, deepThink, nil, onDelta)
}

// StreamTaskFull is StreamTask's own general form (W6-01): paletteOpenedAt,
// when non-nil, is `kahya ask --palette-opened-at <unix-seconds-float>`'s
// own value (hammerspoon/kahya.lua's hs.timer.secondsSinceEpoch(),
// captured at hotkey press) - forwarded verbatim onto the wire so kahyad
// can ledger a palette_open event under this task's own trace_id (see
// kahyad/internal/server's logPaletteOpen doc comment).
func (c *Client) StreamTaskFull(ctx context.Context, traceID, prompt string, deepThink bool, paletteOpenedAt *float64, onDelta func(string)) (taskResult, error) {
	return c.streamTask(ctx, traceID, streamTaskRequest{
		Prompt: prompt, TraceID: traceID, DeepThink: deepThink, PaletteOpenedAt: paletteOpenedAt,
	}, onDelta)
}

// StreamTaskAudio is StreamTaskFull's own W6-02 sibling: `kahya ask
// --audio <path>` sends input_audio_path instead of a typed prompt -
// kahyad transcribes it locally before this task otherwise proceeds
// exactly like StreamTaskFull's own request (same deep_think/
// palette_opened_at wiring, same SSE contract/result mapping).
func (c *Client) StreamTaskAudio(ctx context.Context, traceID, audioPath string, deepThink bool, paletteOpenedAt *float64, onDelta func(string)) (taskResult, error) {
	return c.streamTask(ctx, traceID, streamTaskRequest{
		TraceID: traceID, DeepThink: deepThink, PaletteOpenedAt: paletteOpenedAt, InputAudioPath: audioPath,
	}, onDelta)
}

// streamTask is StreamTaskFull/StreamTaskAudio's shared core: marshal
// reqBody, POST /v1/task, and read the resulting SSE stream to a terminal
// taskResult - the one place this mapping is implemented, so the two
// public entry points can never drift apart.
func (c *Client) streamTask(ctx context.Context, traceID string, reqBody streamTaskRequest, onDelta func(string)) (taskResult, error) {
	body, err := json.Marshal(reqBody)
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
