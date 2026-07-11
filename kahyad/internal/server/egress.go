// egress.go wires the W3-05 egress gate into this package: POST
// /session/sensitive-read (UDS) — the wire endpoint this task's spec
// names, and the one its own manual-verification acceptance criterion
// curls (`curl --unix-socket ... /session/sensitive-read`) — and the
// mcp/fs.SensitiveReadMarker in-process adapter fs.go's SetFSTool wiring
// hands to mcp/fs.New, so fs_read's secret_lane_read seam reaches the
// SAME *egress.Gate every other in-process caller (the anthproxy
// egress-gate factory in task.go, this package's own egress proxy) uses.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"kahya/kahyad/internal/egress"
	mcpfs "kahya/mcp/fs"
	mcpshell "kahya/mcp/shell"
)

// SetEgressGate wires the W3-05 gate (kahya/internal/egress.NewGate) into
// this package. Call before Prepare(); POST /session/sensitive-read
// answers 503 until this is set (the same "unwired dependency" posture as
// SetSearcher/SetReindexer/SetTaskStore elsewhere in this package).
func (s *Server) SetEgressGate(gate *egress.Gate) {
	s.egressGate = gate
}

// EgressGate returns the wired *egress.Gate (nil if SetEgressGate was
// never called) — task.go's per-task anthproxy egress-gate factory and
// main.go's egress.Proxy wiring both consult this.
func (s *Server) EgressGate() *egress.Gate {
	return s.egressGate
}

// egressSensitiveMarker adapts *egress.Gate to mcp/fs.SensitiveReadMarker
// — a direct in-process call (mcp/fs's own doc comment: "the same
// in-process today, a real HTTP client later" seam PolicyClient already
// established), since both live in this same process today exactly like
// enginePolicyClient (fs.go) does for policy decisions.
type egressSensitiveMarker struct {
	gate *egress.Gate
}

func (a egressSensitiveMarker) MarkSensitiveRead(ctx context.Context, sessionID, traceID string) error {
	return a.gate.MarkSensitiveRead(ctx, sessionID, traceID)
}

// NewEgressSensitiveMarker constructs the mcp/fs.SensitiveReadMarker
// adapter main.go wires mcp/fs.New with. gate must not be nil.
func NewEgressSensitiveMarker(gate *egress.Gate) mcpfs.SensitiveReadMarker {
	return egressSensitiveMarker{gate: gate}
}

// egressTokenRegistrar adapts *egress.ProxySessionRegistry to
// mcp/shell.EgressTokenRegistrar (BLOCKER B/C) — a direct in-process call,
// exactly like egressSensitiveMarker above: mcp/shell cannot import
// kahyad/internal/egress directly (Go's internal-package import
// boundary), so this thin adapter is what main.go wires
// mcp/shell.Runner.SetEgressTokenRegistrar with. SessionID is set to
// traceID (not left empty) — the SAME key BLOCKER A's mcp/fs fix and
// egress.NewAnthproxyEgressGateHook both use, so a needs_network:true
// container's egress lands in the identical taint bucket a secret-lane
// read under the same trace_id already marked.
type egressTokenRegistrar struct {
	reg *egress.ProxySessionRegistry
}

func (a egressTokenRegistrar) Register(token, traceID, taskID string) {
	a.reg.Register(token, egress.SessionInfo{SessionID: traceID, TraceID: traceID, TaskID: taskID})
}

func (a egressTokenRegistrar) Release(token string) {
	a.reg.Release(token)
}

// NewEgressTokenRegistrar constructs the mcp/shell.EgressTokenRegistrar
// adapter main.go wires mcp/shell.Runner.SetEgressTokenRegistrar with.
// reg must not be nil — it must be the SAME *egress.ProxySessionRegistry
// instance the egress.Proxy listener's own Tokens field is set to,
// otherwise a container's registered token would never be visible to the
// proxy that actually needs to look it up.
func NewEgressTokenRegistrar(reg *egress.ProxySessionRegistry) mcpshell.EgressTokenRegistrar {
	return egressTokenRegistrar{reg: reg}
}

// sensitiveReadRequest is POST /session/sensitive-read's request body.
type sensitiveReadRequest struct {
	SessionID string `json:"session_id"`
	TraceID   string `json:"trace_id"`
}

// handleSensitiveRead implements POST /session/sensitive-read (this
// task's spec step 3): sets session_id's sensitive_read flag (rises
// only, never clears within the session) and ledgers
// sensitive_read_marked. This is the SAME operation mcp/fs's own
// in-process egressSensitiveMarker adapter performs for fs_read's
// secret_lane_read seam — this HTTP endpoint exists so any OTHER
// (present or future, in- or out-of-process) tool can raise the same
// taint flag over the wire, and so this task's manual-verification
// acceptance criterion has a literal endpoint to curl.
func (s *Server) handleSensitiveRead(w http.ResponseWriter, r *http.Request) {
	if s.egressGate == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "egress gate not available")
		return
	}
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}

	var req sensitiveReadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(req.SessionID) == "" {
		writeJSONError(w, http.StatusBadRequest, "session_id must not be empty")
		return
	}

	traceID := req.TraceID
	if traceID == "" {
		traceID = traceIDFromContext(r)
	}

	if err := s.egressGate.MarkSensitiveRead(r.Context(), req.SessionID, traceID); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "session_id": req.SessionID})
}
