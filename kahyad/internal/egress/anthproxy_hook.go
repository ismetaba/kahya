// anthproxy_hook.go builds the per-task closure kahyad/internal/server's
// SetAnthproxy egressGateFactory calls once per task (kahyad/main.go's
// wiring) to gate a task's own model-call egress through THIS package's
// Gate.Check — kahyad/internal/anthproxy.ProxyConfig.EgressGate's fixed
// func(*http.Request) error shape.
//
// This used to be built inline in main.go with an empty SessionInfo.
// SessionID, so Gate.Check's sensitive-read check could never fire for
// model-call egress no matter what mcp/fs's fs_read seam had marked
// (BLOCKER B/C). Moving it here — a small, exported, directly-testable
// constructor — both fixes that (SessionID: traceID, the SAME key BLOCKER
// A's mcp/fs fix and this package's own ProxySessionRegistry use) and lets
// a unit test build the EXACT closure kahyad actually runs in production,
// rather than reimplementing a look-alike copy of it.
package egress

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
)

// anthproxyMaxBodyBytes mirrors kahyad/internal/anthproxy's
// proxyRequestMaxBytes (this package must not import anthproxy): a body of
// unknown length larger than this is refused before it is ever sized.
const anthproxyMaxBodyBytes = 16 << 20

// TerminalEgressReason is the reason id GateDenial.TerminalEgressReason
// reports for a deterministic denial (FIX-U-4); it equals
// anthproxy.ReasonEgressBlocked (this package must not import anthproxy).
const TerminalEgressReason = "egress_blocked"

// GateDenial is the error NewAnthproxyEgressGateHook returns for a
// Gate.Check deny (FIX-U-4). Error() is the decision's Turkish Reason
// byte-for-byte, so the 403 body the worker sees is unchanged.
// TerminalEgressReason marks the deterministic denials - the allowlist and
// the sensitive-read rule: the model-call target is the FIXED Anthropic
// upstream host:port and the Gate has no reload, so such a denial holds
// for the daemon's lifetime and the task is failed once (egress_blocked)
// instead of re-403ing every resume. A volume-budget denial is daily (and
// already raises the gate's own Alarm), so it stays non-terminal; a
// gate.Check ERROR is returned unwrapped and stays non-terminal too
// (transient).
type GateDenial struct {
	Rule   string
	Reason string
}

func (d *GateDenial) Error() string { return d.Reason }

// TerminalEgressReason returns TerminalEgressReason for an allowlist or
// sensitive-read denial, "" for anything else (budget).
func (d *GateDenial) TerminalEgressReason() string {
	switch d.Rule {
	case EventBlockedAllowlist, EventBlockedSensitive:
		return TerminalEgressReason
	}
	return ""
}

// NewAnthproxyEgressGateHook builds the per-task egress-gate hook
// kahyad/internal/server.Server.SetAnthproxy's egressGateFactory returns:
// gate.Check is consulted against the FIXED (host, port) — the
// Anthropic upstream's own host:port, resolved once at boot
// (kahyad/main.go's hostPortFromURL) — for every request THIS task's
// worker sends through its own per-task anthproxy listener, with
// nbytes taken from the request's Content-Length - or, for a body of
// unknown length (chunked, ContentLength -1), from its REAL byte count:
// the hook buffers it (capped at anthproxyMaxBodyBytes; a read error or an
// oversized body is refused, fail-closed) and restores r.Body/
// ContentLength so the proxy forwards the same bytes. This is the
// anthproxy counterpart of egress/proxy.go's MINOR F counting reader;
// before it a chunked model call was admitted as 0 bytes and the per-host
// daily byte budget (HANDOFF §5 #1 "hacim bütçesi") never moved
// (FIX-FINAL egress-budget-chunked-bypass). Session keyed on traceID: SessionInfo{SessionID:
// traceID, TaskID: taskID, TraceID: traceID}. Keying SessionID on
// traceID (never left empty, never a caller-supplied/model-supplied
// value) is the fix itself — it is the SAME identity mcp/fs's fs_read
// seam marks sensitive under (kahyad/internal/server.
// egressSensitiveMarker, via this package's own MarkSensitiveRead), so a
// secret-lane read earlier in this task's own lifetime hard-blocks this
// task's model-call egress the instant it would otherwise leave the
// allowlist — exactly HANDOFF §5 safety #1's invariant, now reachable for
// the anthproxy path.
func NewAnthproxyEgressGateHook(gate *Gate, host string, port int) func(taskID, traceID string) func(*http.Request) error {
	return func(taskID, traceID string) func(*http.Request) error {
		return func(r *http.Request) error {
			nbytes := r.ContentLength
			if nbytes < 0 {
				nbytes = 0
				if r.Body != nil && r.Body != http.NoBody {
					b, err := io.ReadAll(io.LimitReader(r.Body, anthproxyMaxBodyBytes+1))
					if err != nil {
						return fmt.Errorf("egress: read model-call body of unknown length: %w", err)
					}
					if len(b) > anthproxyMaxBodyBytes {
						return fmt.Errorf("egress: model-call body of unknown length exceeds %d bytes", anthproxyMaxBodyBytes)
					}
					_ = r.Body.Close()
					r.Body = io.NopCloser(bytes.NewReader(b))
					r.ContentLength = int64(len(b))
					nbytes = int64(len(b))
				}
			}
			decision, err := gate.Check(r.Context(), Target{Host: host, Port: port}, nbytes,
				SessionInfo{SessionID: traceID, TaskID: taskID, TraceID: traceID})
			if err != nil {
				// Fail-closed (tasks/README.md global convention): a gate
				// error is treated exactly like a deny, never a permissive
				// fallback.
				return err
			}
			if !decision.Allow {
				return &GateDenial{Rule: decision.Rule, Reason: decision.Reason}
			}
			return nil
		}
	}
}
