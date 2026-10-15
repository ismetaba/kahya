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
	"net/http"
)

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
// nbytes taken from the request's Content-Length (clamped to 0 when
// absent/chunked, matching Gate.Check's own pre-admission-estimate
// convention) and session keyed on traceID: SessionInfo{SessionID:
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
