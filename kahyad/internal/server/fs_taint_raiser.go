// fs_taint_raiser.go implements mcp/fs.SessionTaintRaiser (project-review
// #12): a secret-lane fs_read raises the OWNING SESSION's taint tier so
// subsequent non-R tool calls from that session are denied (the taint gate
// is R-tools + notify only). It resolves the server-persisted session_id
// from the request's trace_id/task_id correlation (never a caller-supplied
// id — the same rule the policy taint-check hook follows) and calls
// taint.Tracker.Raise. mcp/fs cannot import kahyad/internal/policy or
// kahyad/internal/taint directly (internal-package boundary), so this small
// adapter lives here.
package server

import (
	"context"

	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/taint"
)

// FSSessionTaintRaiser adapts a policy.SessionResolver + taint.Tracker to
// mcp/fs.SessionTaintRaiser.
type FSSessionTaintRaiser struct {
	resolver policy.SessionResolver
	tracker  *taint.Tracker
}

// NewFSSessionTaintRaiser constructs an FSSessionTaintRaiser.
func NewFSSessionTaintRaiser(resolver policy.SessionResolver, tracker *taint.Tracker) *FSSessionTaintRaiser {
	return &FSSessionTaintRaiser{resolver: resolver, tracker: tracker}
}

// RaiseSessionTaint implements mcp/fs.SessionTaintRaiser. An unresolvable
// session (no tasks row / no session_id recorded yet) is NOT an error: the
// taint default already treats an unknown session as tainted, so there is
// nothing to raise onto — see mcp/fs.Server.SessionTaintRaiser's doc
// comment for why this whole path is best-effort.
func (r *FSSessionTaintRaiser) RaiseSessionTaint(ctx context.Context, traceID, taskID, reason string) error {
	if r.resolver == nil || r.tracker == nil {
		return nil
	}
	sessionID, err := r.resolver.ResolveSession(ctx, traceID, taskID)
	if err != nil {
		return err
	}
	if sessionID == "" {
		return nil
	}
	return r.tracker.Raise(ctx, traceID, sessionID, reason)
}
