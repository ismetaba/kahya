// kahya-mcp is a thin stdio<->UDS relay (W12-05): it reads newline-
// delimited JSON-RPC 2.0 messages from stdin (the same wire format real
// MCP stdio clients - including the worker's claude-agent-sdk MCP client,
// W12-09 - use) and forwards each one, byte-for-byte, as an HTTP POST to
// kahyad's POST /v1/mcp endpoint over the UDS control socket (KAHYA_SOCKET
// env, or kahyad's own resolved default), writing kahyad's JSON response
// back to stdout as one line. It propagates KAHYA_TRACE_ID (env) as the
// X-Kahya-Trace-Id header on every request.
//
// Deliberate SDK-usage choice (documented per the task spec's escape
// hatch: "if the SDK's API is unclear, implement a correct minimal
// JSON-RPC 2.0 ... by hand rather than guessing SDK calls - correctness
// over SDK-purity"): this binary does NOT use the official MCP Go SDK's
// Client/Server machinery on its OWN stdio side. kahyad's actual MCP
// protocol implementation - tool registration/schema, initialize/
// tools-list/tools-call dispatch, the binding policy gate - is fully
// implemented with the pinned SDK (github.com/modelcontextprotocol/
// go-sdk/mcp; see kahyad/internal/server/mcp.go and
// mcp/memory/server.go). kahyad's /v1/mcp endpoint is mounted STATELESS
// (mcp.StreamableHTTPOptions.Stateless=true): per the SDK's own
// documented semantics, a stateless server does not require a prior
// initialize handshake on a given HTTP request at all - each POST "uses a
// temporary session with default initialization parameters" - so a
// single self-contained JSON-RPC request (e.g. a bare tools/list with no
// prior initialize) can be forwarded as its own POST and answered
// correctly, with zero session/handshake bookkeeping needed on this
// bridge's side. That is what "the bridge is a dumb pipe; no logic, no
// state" (task spec) means concretely: every incoming line is forwarded
// completely independently of every other line, with no MCP protocol
// awareness in this binary at all - it never parses a tool name, a
// method, or a result; it only frames lines and relays bytes.
//
// No logic/state beyond that framing: this binary performs no tool
// dispatch, no policy decisions, no ledgering. Diagnostics go to stderr
// only, as one hand-rolled JSONL line per event (ts/level/event/trace_id,
// matching every other Kahya process's logging convention -
// kahyad/internal/logx - though this short-lived, stateless process does
// not otherwise warrant pulling in that whole package: it never opens a
// log file, never scopes a request-per-call logger, and exits after a
// single stdin stream).
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
	"os"
	"strings"
	"time"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/traceid"
)

// requestTimeout bounds EVERY POST to kahyad, dial included: a
// dead/stopped daemon must be detected and reported well inside the
// W12-05 acceptance test's "process exits non-zero within 5s" budget,
// never left to hang on a slow/stuck connection.
const requestTimeout = 4 * time.Second

// mcpEndpoint is kahyad's fixed MCP route. The host part is a fake
// hostname (never resolved - the Transport's DialContext always dials the
// UDS socket instead), matching the "http://kahyad/..." convention every
// other Kahya UDS client in this repo already uses (see
// kahyad/internal/server's probeHealth and the kahya CLI's client.go).
const mcpEndpoint = "http://kahyad/v1/mcp"

func main() {
	os.Exit(run(os.Stdin, os.Stdout, os.Stderr))
}

// run is main's testable core.
func run(stdin io.Reader, stdout, stderr io.Writer) int {
	traceID := os.Getenv("KAHYA_TRACE_ID")
	if traceID == "" {
		traceID = traceid.New()
	}

	sock, err := resolveSocket()
	if err != nil {
		logLine(stderr, traceID, "error", "socket_resolve_failed", err.Error())
		return 1
	}
	logLine(stderr, traceID, "info", "bridge_start", "socket="+sock)

	client := &http.Client{
		Timeout: requestTimeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
	}

	reader := bufio.NewReader(stdin)
	writer := bufio.NewWriter(stdout)
	defer writer.Flush()

	for {
		// ReadString (not bufio.Scanner) so one oversized JSON-RPC line -
		// e.g. a memory_write call with a large `content` argument - is
		// never silently truncated/dropped by Scanner's default 64KB/1MB
		// token cap (the same reasoning kahyad/internal/server's
		// readLogLines documents for its own line-at-a-time JSONL scan).
		line, readErr := reader.ReadString('\n')
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			status := relay(client, traceID, trimmed, writer, stderr)
			writer.Flush()
			if status != 0 {
				return status
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return 0
			}
			logLine(stderr, traceID, "error", "stdin_read_failed", readErr.Error())
			return 1
		}
	}
}

// relay POSTs one raw JSON-RPC message (line) to kahyad's /v1/mcp and
// writes its response as one line to out. It returns a NONZERO exit
// status only when kahyad could not be reached AT ALL (a transport-level
// failure: dial refused, timeout, ...) - the fail-fast case the W12-05
// acceptance test exercises by stopping the daemon first. In that case it
// ALSO writes a best-effort JSON-RPC error response (carrying the
// original request's id when parseable) to out before returning, so a
// caller reading stdout still gets a well-formed JSON-RPC message instead
// of silence. Any OTHER response (including a non-2xx HTTP status from
// kahyad) is relayed to out verbatim - this binary has no opinion on MCP
// semantics, per the package doc.
func relay(client *http.Client, traceID, line string, out io.Writer, stderr io.Writer) int {
	req, err := http.NewRequest(http.MethodPost, mcpEndpoint, bytes.NewBufferString(line))
	if err != nil {
		writeTransportError(out, line, err)
		logLine(stderr, traceID, "error", "request_build_failed", err.Error())
		return 1
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("X-Kahya-Trace-Id", traceID)

	resp, err := client.Do(req)
	if err != nil {
		writeTransportError(out, line, err)
		logLine(stderr, traceID, "error", "kahyad_unreachable", err.Error())
		return 1
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNoContent {
		// A JSON-RPC notification (e.g. "notifications/initialized"): the
		// MCP streamable-HTTP spec requires no response body for these, so
		// relay correctly writes nothing back to the caller.
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		return 0
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		writeTransportError(out, line, err)
		logLine(stderr, traceID, "error", "response_read_failed", err.Error())
		return 1
	}
	fmt.Fprintln(out, string(body))
	return 0
}

// writeTransportError writes a synthesized JSON-RPC error response for a
// request that never got a real answer from kahyad at all.
func writeTransportError(out io.Writer, requestLine string, transportErr error) {
	env := map[string]any{
		"jsonrpc": "2.0",
		"id":      extractID(requestLine),
		"error": map[string]any{
			"code":    -32000,
			"message": fmt.Sprintf("kahyad unreachable: %s", transportErr.Error()),
		},
	}
	b, err := json.Marshal(env)
	if err != nil {
		// json.Marshal on this fixed shape (strings/ints/a nested map)
		// cannot actually fail; guarded anyway so a future field addition
		// can never panic this path.
		b = []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32000,"message":"kahyad unreachable"}}`)
	}
	fmt.Fprintln(out, string(b))
}

// extractID best-effort parses the "id" field out of a raw JSON-RPC
// request line, returning nil if the line isn't valid JSON, has no id
// field, or id itself doesn't parse (any of which just means the
// synthesized error response's id is null - still valid JSON-RPC).
func extractID(line string) any {
	var probe struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal([]byte(line), &probe); err != nil || len(probe.ID) == 0 {
		return nil
	}
	var id any
	if err := json.Unmarshal(probe.ID, &id); err != nil {
		return nil
	}
	return id
}

// resolveSocket returns the UDS path to dial: KAHYA_SOCKET if set, else
// kahyad's own resolved default - mirroring the kahya CLI's
// resolveSocket (kahyad/cmd/kahya/client.go) exactly, so every Kahya
// client agrees on the same socket path without a second copy of
// config.Load's defaulting/override rules.
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

// logLine writes one hand-rolled JSONL diagnostic line to w (stderr) -
// see the package doc for why this bridge does not pull in
// kahyad/internal/logx for such a small, short-lived process.
func logLine(w io.Writer, traceID, level, event, detail string) {
	line, err := json.Marshal(map[string]string{
		"ts":       time.Now().UTC().Format(time.RFC3339Nano),
		"level":    level,
		"event":    event,
		"trace_id": traceID,
		"detail":   detail,
	})
	if err != nil {
		return
	}
	fmt.Fprintln(w, string(line))
}
