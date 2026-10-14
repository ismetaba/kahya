// approval_wait.go is the server half of the B-1 approval loop
// (needs_approval -> human decision -> execution of exactly the approved
// bytes; owner decision G01-1 option 1, "tool-side wait-and-resume"):
//
// Every in-process binding gate - policyGateMiddleware (mcp.go: memory_*,
// the dev stub) and enginePolicyClient (fs.go: fs_*, shell_*, osascript_*,
// via Server.FSPolicyClient) - calls checkWithApprovalWait instead of a
// bare Engine.Check. A needs_approval decision PARKS the in-flight MCP
// tools/call inside kahyad (approval_wait_seconds) until the human answers
// on any approval surface (`kahya approve`, Hammerspoon, Telegram for a
// reversible tool); the same call then re-checks, Engine.Check redeems the
// single-use grant Engine.Approve persisted for THAT call's own pending
// approval (never a sibling's, never a ladder auto-allow; a denial or halt
// of it is final), and the tool executes exactly the bytes the human saw -
// they never pass back through the model. The
// worker's can_use_tool is only a preflight (HANDOFF S5: not a security
// boundary): it defers every needs_approval for a kahyad MCP tool to this
// gate.
//
// The waiter holds no database transaction while parked (brain.db runs on
// one connection - kahyad/internal/store); a parked call whose client went
// away (worker killed, bridge closed) never redeems a later approval
// (requestDoneKey), and kahyad shutdown releases every parked call.
package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"kahya/kahyad/internal/policy"
)

// Turkish tool-facing texts (CLAUDE.md language policy): the model reads
// these as the tool's error text, so they also carry its instruction.
// Byte-exact, never paraphrased.
const (
	// MsgApprovalPendingFmt answers a needs_approval immediately when
	// waiting is disabled (approval_wait_seconds: 0). %s = pending id.
	MsgApprovalPendingFmt = "⏳ Yerel onay bekleniyor (onay kimliği: %s). Kullanıcı onaylarsa aynı çağrıyı aynı argümanlarla tekrar et; reddederse eylemi yapma."
	// MsgApprovalWaitTimeoutFmt: nobody decided within the wait budget; the
	// id is still live, so an identical retry after a later approval
	// redeems it. %s = pending id.
	MsgApprovalWaitTimeoutFmt = "⏳ Onay süresi içinde karar verilmedi (onay kimliği: %s). Kullanıcı onaylarsa aynı çağrıyı aynı argümanlarla tekrar et."
	// MsgApprovalDenied: the human denied (or a halt invalidated) the call.
	MsgApprovalDenied = "❌ Kullanıcı bu eylemi reddetti; eylemi yapma, gerekiyorsa alternatif öner."
	// MsgApprovalNotRedeemableFmt: approved, but the grant could not be
	// redeemed (expired, revoked by a halt). %s = pending id.
	MsgApprovalNotRedeemableFmt = "⛔ Onay (%s) kullanılamadı (süresi doldu ya da görev durduruldu); eylem yürütülmedi."
	// MsgApprovalPendingNoticeFmt is the `kahya ask` SSE line telling the
	// user why the answer paused and how to approve. Both %s = pending id.
	// Never the tool input (a secret-lane payload must not be echoed).
	MsgApprovalPendingNoticeFmt = "⏳ Onay bekleniyor (onay kimliği: %s) — başka bir terminalde 'kahya approve %s' çalıştır.\n"
)

// approvalWaitProbeInterval is the belt-and-braces DB probe cadence while
// waiting: the Engine hook (SetApprovalDecidedHook -> ApprovalDecided) is
// the fast path; the probe covers a decision that landed between Check
// minting the id and Wait registering for it.
const approvalWaitProbeInterval = 2 * time.Second

// approvalKeepaliveInterval is how often a parked call keeps its
// `kahya ask` SSE stream alive (an SSE comment line): the CLI aborts a
// stream after 30s with no byte at all (kahyad/cmd/kahya idleReadTimeout),
// which would cancel the very task the user is approving.
const approvalKeepaliveInterval = 10 * time.Second

// waitOutcome is how one approval wait ended (ledgered as
// approval_wait_ended.outcome).
type waitOutcome string

const (
	waitApproved waitOutcome = "approved"
	waitDenied   waitOutcome = "denied"
	waitTimeout  waitOutcome = "timeout"
	waitCanceled waitOutcome = "canceled"
)

// approvalWaiter parks callers on a pending_approval_id until
// OnDecided(id) fires, the probe observes a decision, the budget elapses,
// the caller's ctx ends, or Close is called (kahyad shutdown).
type approvalWaiter struct {
	mu        sync.Mutex
	waiters   map[string][]chan bool
	probe     func(ctx context.Context, id string) (decided, approved bool)
	interval  time.Duration
	closed    chan struct{}
	closeOnce sync.Once
}

func newApprovalWaiter(probe func(ctx context.Context, id string) (decided, approved bool)) *approvalWaiter {
	return &approvalWaiter{
		waiters: map[string][]chan bool{}, probe: probe,
		interval: approvalWaitProbeInterval, closed: make(chan struct{}),
	}
}

// OnDecided is the Engine hook target. Non-blocking: every registered
// channel is buffered(1) and receives at most one value.
func (w *approvalWaiter) OnDecided(id string, approved bool) {
	w.mu.Lock()
	chans := w.waiters[id]
	delete(w.waiters, id)
	w.mu.Unlock()
	for _, ch := range chans {
		ch <- approved
	}
}

// Close releases every current and future Wait with waitCanceled
// (Server.Shutdown) - nothing parked ever redeems after kahyad started
// shutting down.
func (w *approvalWaiter) Close() {
	w.closeOnce.Do(func() { close(w.closed) })
}

// Wait blocks until id is decided, the budget elapses, ctx ends, or the
// waiter is closed.
func (w *approvalWaiter) Wait(ctx context.Context, id string, budget time.Duration) waitOutcome {
	ch := make(chan bool, 1)
	w.mu.Lock()
	w.waiters[id] = append(w.waiters[id], ch)
	w.mu.Unlock()
	defer w.unregister(id, ch)

	decided := func(approved bool) waitOutcome {
		if approved {
			return waitApproved
		}
		return waitDenied
	}
	select {
	case <-w.closed:
		return waitCanceled
	default:
	}
	if d, a := w.probe(ctx, id); d { // decided before we registered
		return decided(a)
	}
	deadline := time.NewTimer(budget)
	defer deadline.Stop()
	tick := time.NewTicker(w.interval)
	defer tick.Stop()
	for {
		select {
		case a := <-ch:
			return decided(a)
		case <-tick.C:
			if d, a := w.probe(ctx, id); d {
				return decided(a)
			}
		case <-deadline.C:
			return waitTimeout
		case <-ctx.Done():
			return waitCanceled
		case <-w.closed:
			return waitCanceled
		}
	}
}

func (w *approvalWaiter) unregister(id string, ch chan bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	chans := w.waiters[id]
	for i, c := range chans {
		if c == ch {
			w.waiters[id] = append(chans[:i:i], chans[i+1:]...)
			break
		}
	}
	if len(w.waiters[id]) == 0 {
		delete(w.waiters, id)
	}
}

// SetApprovalWait sets how long a tool-side binding gate parks a tool call
// on needs_approval (config key approval_wait_seconds; 0 disables waiting -
// the call fails at once with MsgApprovalPendingFmt). Call before Prepare.
func (s *Server) SetApprovalWait(d time.Duration) { s.approvalWait = d }

// ApprovalDecided is the policy.Engine.SetApprovalDecidedHook target
// (main.go): it wakes every tool call parked on id.
func (s *Server) ApprovalDecided(id string, approved bool) {
	if s.approvalWaiter != nil {
		s.approvalWaiter.OnDecided(id, approved)
	}
}

func (s *Server) probeApprovalDecision(ctx context.Context, id string) (decided, approved bool) {
	if s.policyEngine == nil {
		return false, false
	}
	d, decision, err := s.policyEngine.PendingApprovalDecision(ctx, id)
	if err != nil || !d {
		return false, false
	}
	return true, decision == policy.PendingDecisionApproved
}

// ledgerRaw appends one best-effort ledger event (the deny-all
// policy_decision pattern handlePolicyCheck uses by hand).
func (s *Server) ledgerRaw(ctx context.Context, traceID, kind string, payload map[string]any) {
	if s.eventLogger == nil {
		return
	}
	if err := s.eventLogger.LogEvent(ctx, traceID, kind, payload); err != nil {
		s.log.With(traceID).Warn(kind+"_ledger_error", "err", err.Error())
	}
}

// requestDoneKey carries the HTTP request's OWN Done() channel into the MCP
// handler context (ensureTraceHeader, mcp.go). go-sdk v1.0.0's jsonrpc2
// layer hands every tools/call handler a notDone{}-wrapped context
// (internal/jsonrpc2/conn.go: Done() is nil and never fires on a client
// disconnect; only Value() passes through), so this is the ONLY way
// checkWithApprovalWait learns the bridge went away. Both tool paths see
// it: policyGateMiddleware's ctx and the ctx mcp/fs, mcp/shell and
// mcp/osascript handlers pass to enginePolicyClient.Check are the same
// handler context.
type requestDoneKey struct{}

// checkWithApprovalWait (B-1) is THE tool-side binding check every
// in-process gate uses (see this file's package comment). needs_approval
// parks the call (SetApprovalWait > 0). A denial or halt of the call's own
// pending approval ends it as a deny with no re-check at all; an approval
// (or a timeout) re-checks bound to that pending id
// (CheckInput.RedeemPendingApprovalID + NoMintPending): only the call's own
// grant is redeemed (policy.Engine redeemApprovedGrant), nothing mints a
// second pending row, and a call whose client disconnected (or a kahyad
// shutdown) never redeems at all.
func (s *Server) checkWithApprovalWait(ctx context.Context, in policy.CheckInput) (policy.Decision, error) {
	if s.policyEngine == nil {
		// Fail-closed: never rely on wiring order.
		return policy.Decision{Result: policy.ResultDeny, Reason: policy.ReasonPolicyStateError, Rule: policy.RuleLadderV1}, nil
	}
	d, err := s.policyEngine.Check(ctx, in)
	if d.Result != policy.ResultNeedsApproval || d.PendingApprovalID == "" {
		return d, err
	}
	id := d.PendingApprovalID
	if s.approvalWait <= 0 || s.approvalWaiter == nil {
		d.Reason = fmt.Sprintf(MsgApprovalPendingFmt, id)
		return d, nil
	}
	tool := policy.Canonicalize(in.Tool)

	// The wait is bound to the HTTP request's own lifetime (requestDoneKey):
	// a worker killed by task_timeout_min or a halt (process group killed ->
	// bridge UDS connection closed -> net/http cancels r.Context()) ends
	// the wait as canceled instead of leaving a parked call that would
	// redeem a later approval and EXECUTE with no live task behind it.
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if done, ok := ctx.Value(requestDoneKey{}).(<-chan struct{}); ok && done != nil {
		go func() {
			select {
			case <-done:
				cancel()
			case <-waitCtx.Done():
			}
		}()
	}

	s.ledgerRaw(ctx, in.TraceID, "approval_wait_started", map[string]any{
		"event": "approval_wait_started", "pending_approval_id": id, "task_id": in.TaskID, "tool": tool,
		"budget_s": int(s.approvalWait.Seconds()),
	})
	stopNotice := s.startApprovalNotice(in.TaskID, id)
	outcome := s.approvalWaiter.Wait(waitCtx, id, s.approvalWait)
	stopNotice()
	if waitCtx.Err() != nil {
		outcome = waitCanceled
	}
	s.ledgerRaw(ctx, in.TraceID, "approval_wait_ended", map[string]any{
		"event": "approval_wait_ended", "pending_approval_id": id, "task_id": in.TaskID, "tool": tool,
		"outcome": string(outcome),
	})

	if outcome == waitCanceled {
		// Deliberately NOT redeemed: the caller is gone (or kahyad is
		// shutting down). The pending id stays live for its TTL; a later
		// approval mints a grant only an identical retry by a live task can
		// redeem (approval_grant_expired otherwise) - never an execution
		// without an in-flight, client-attached call. nil error on purpose:
		// on a shutdown the still-connected model reads the Turkish reason
		// (an error would surface as English "context canceled").
		return policy.Decision{
			Result: policy.ResultDeny, Reason: fmt.Sprintf(MsgApprovalWaitTimeoutFmt, id),
			Rule: policy.RuleLadderV1, Class: d.Class, Scope: d.Scope, PendingApprovalID: id,
		}, nil
	}

	if outcome == waitDenied {
		// The human denied - or a halt invalidated - THIS call's own pending
		// approval: final, so nothing re-checks. A re-check could still
		// answer ALLOW (the ladder level raised while the call waited - Deny
		// demotes by only one step and a halt not at all - or another
		// approved grant for the same task and bytes) and run what the human
		// just refused.
		return policy.Decision{
			Result: policy.ResultDeny, Reason: MsgApprovalDenied, Rule: policy.RuleLadderV1,
			Class: d.Class, Scope: d.Scope, Level: d.Level, PendingApprovalID: id,
		}, nil
	}

	// approved, or timeout (an approval may have landed right at the
	// deadline): re-check bound to this call's own pending id. The engine
	// redeems that id's grant and nothing else - never a sibling's grant,
	// never a ladder auto-allow - and mints no second pending row.
	in.NoMintPending = true
	in.RedeemPendingApprovalID = id
	d2, err := s.policyEngine.Check(ctx, in)
	if d2.Result == policy.ResultAllow {
		if d2.RedeemedPendingApprovalID != id {
			// Engine contract broken (an allow that is not this call's own
			// approval): fail-closed.
			return policy.Decision{
				Result: policy.ResultDeny, Reason: policy.ReasonPolicyStateError, Rule: policy.RuleLadderV1,
				Class: d.Class, Scope: d.Scope, PendingApprovalID: id,
			}, err
		}
		return d2, err
	}
	if d2.Result == policy.ResultDeny {
		// A deny of the engine's own (the session turned tainted while the
		// human decided, a policy state error) is the truer answer.
		return d2, err
	}
	switch outcome {
	case waitApproved:
		d2.Result, d2.Reason = policy.ResultDeny, fmt.Sprintf(MsgApprovalNotRedeemableFmt, id)
	default: // timeout: the id is still live, so the result stays needs_approval
		d2.Reason = fmt.Sprintf(MsgApprovalWaitTimeoutFmt, id)
	}
	d2.PendingApprovalID = id
	return d2, err
}

// approvalNoticeSink is one live `kahya ask` SSE stream (taskSSE) a parked
// call of that task reports to.
type approvalNoticeSink interface {
	notice(pendingApprovalID string)
	keepalive()
}

// registerApprovalNotice routes approval-wait notices for taskID to sink
// until the returned func is called (handleTask, for its stream's life).
func (s *Server) registerApprovalNotice(taskID string, sink approvalNoticeSink) (unregister func()) {
	if taskID == "" {
		return func() {}
	}
	s.approvalNotices.Store(taskID, sink)
	return func() { s.approvalNotices.CompareAndDelete(taskID, sink) }
}

// startApprovalNotice tells taskID's live SSE stream (if any) that the
// task is waiting for approval id, and keeps that stream alive until the
// returned stop func is called.
func (s *Server) startApprovalNotice(taskID, id string) (stop func()) {
	v, ok := s.approvalNotices.Load(taskID)
	if !ok || taskID == "" {
		return func() {}
	}
	sink := v.(approvalNoticeSink)
	sink.notice(id)
	interval := s.approvalKeepalive
	if interval <= 0 {
		interval = approvalKeepaliveInterval
	}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				sink.keepalive()
			}
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}

// taskSSE serializes one /v1/task SSE stream's writes: since B-1 a parked
// tool call (another goroutine) writes approval notices/keepalives next to
// spawn.Run's own delta stream, and nothing may be written once the
// handler has returned (close).
type taskSSE struct {
	mu      sync.Mutex
	closed  bool
	w       io.Writer
	flusher http.Flusher
	inner   func(event string, payload any)
}

func newTaskSSE(w io.Writer, flusher http.Flusher, inner func(event string, payload any)) *taskSSE {
	return &taskSSE{w: w, flusher: flusher, inner: inner}
}

func (t *taskSSE) write(event string, payload any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.inner(event, payload)
}

func (t *taskSSE) notice(id string) {
	t.write("delta", map[string]string{"text": fmt.Sprintf(MsgApprovalPendingNoticeFmt, id, id)})
}

// keepalive writes an SSE comment line: every SSE reader (the kahya CLI's
// readSSE included) skips it, but it resets the reader's idle timer.
func (t *taskSSE) keepalive() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	fmt.Fprint(t.w, ": keepalive\n\n")
	if t.flusher != nil {
		t.flusher.Flush()
	}
}

func (t *taskSSE) close() {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
}
