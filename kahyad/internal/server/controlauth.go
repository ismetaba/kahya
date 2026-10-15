// controlauth.go is kahyad's UDS caller-identity gate (G03 / B-3).
//
// kahyad's single UDS listener serves two very different callers on the
// same socket and the same uid: the human surfaces (the kahya CLI,
// kahya-trigger under launchd, Hammerspoon via the CLI) and every worker
// kahyad spawns (KAHYA_SOCKET is in the worker env so its can_use_tool and
// UserPromptSubmit hooks can reach /policy/check and /v1/memory/search).
// Socket mode 0600 separates uids only, so without a second identity a
// worker could promote its own autonomy ladder (/policy/promote) or
// approve its own W3 pending approval as surface="local"
// (/approvals/{id}/decision, /policy/feedback).
//
// The gate: kahyad mints a fresh 32-byte secret on every boot and writes
// it (hex, 0600) to config.ControlSecretPath(socket) = "<socket>.secret".
// Routes fall in three classes (classifyRoute):
//
//   - open: /health only.
//   - worker: /policy/check, /v1/memory/search, /v1/mcp,
//     /policy/consume-token - the only routes a worker (and its kahya-mcp
//     bridge child) ever needs.
//   - control: everything else, INCLUDING any path not listed (a route
//     added later without a classification, or a path no handler
//     serves), so the default is fail-closed.
//
// Control routes require config.ControlSecretHeader to carry the exact
// per-boot secret (constant-time compare); a missing or wrong value is
// answered 403 + MsgControlSecretRejected and ledgered as
// kind=control_auth_rejected. The secret is never placed in a worker's
// environment (spawn.secretEnvDenylist) and the kahya-mcp bridge never
// reads or sends it.
//
// Read-only control routes (/metrics, /readiness, /v1/log, /policy/state)
// stay control-only on purpose (owner decision G03-4 / G00 H-4): a tainted
// worker must not read other tasks' log lines or ledger summaries.
// /session/sensitive-read stays control too (G00 C-16): it has no caller
// outside kahyad's own process today.
//
// Same-uid limit (documented, owner decisions G03-1/2/3 = no sandbox-exec,
// no Keychain ACL, no separate uid): a worker compromised to the point of
// reading arbitrary files as the user can read <socket>.secret. This gate
// closes the path for everything that only uses what the worker is GIVEN
// (its env, its tools, a prompt-injected model); B-14's peer attribution
// (below) narrows it further.
//
// Caller attribution (G03 / B-14). The correlation ids a worker-route
// caller DECLARES (task_id/trace_id in the JSON body, X-Kahya-Task-Id /
// X-Kahya-Trace-Id headers on /v1/mcp) used to be trusted as-is, so a
// tainted worker could name a clean task's ids and have Engine.Check
// resolve the clean task's session (taint laundering), with tokens, undo
// windows and hafiza_injected rows written against the wrong task. Now:
//
//   - connContext records the OS-level peer pid of every accepted UDS
//     connection (peerPIDFromConn: LOCAL_PEERPID on macOS, SO_PEERCRED on
//     Linux) and that process's SESSION id (getsid; its process group is
//     recorded too, for the ledger only).
//   - Per REQUEST (keep-alive connections outlive registry changes),
//     attributeCaller maps that session to a live worker kahyad itself
//     spawned (task.LiveRegistry.BySID): spawn.Run starts every worker as
//     the leader of its own new session (Setsid), and every process the
//     worker's tree spawns - the kahya-mcp bridge included - inherits it.
//     The key is the session, NOT the process group: setpgid(2) lets any
//     process join another group of its own session (with Setpgid-only
//     workers all sharing kahyad's session, a compromised worker's child
//     could join a clean worker's group and borrow its identity) or leave
//     its group with setpgid(0, 0); it can never join another session.
//     The only way out of a worker's session is setsid(2) in a forked
//     child, which yields a fresh session no worker is registered under.
//   - A connection whose peer cannot be resolved to a session (the peer
//     exited before kahyad accepted it - the fd may live on in a forked
//     child - or the conn is not a UDS conn) is refused on every non-open
//     route, even with the secret (reason unresolved_peer): kahyad cannot
//     tell a live worker from a CLI there, so it fails closed.
//   - Worker routes: an attributable caller has its ids PINNED to that
//     worker's own task_id/trace_id (headers overwritten here, body ids
//     overridden in the handlers via pinBodyIdentity); a conflicting
//     declaration is ledgered as worker_identity_override, never trusted.
//     A caller NOT attributable to a live worker (a shell script, a test
//     harness, the CLI) is served only with the control secret - else 403
//     reason unattributable_worker_route.
//   - Control routes: a caller attributable to a live worker is refused
//     even WITH the secret (reason live_worker_on_control_route) - while a
//     worker's task is running, a child that read <socket>.secret cannot
//     use it from anywhere in that worker's session, however it rearranges
//     its process groups. That refusal depends on the worker still being
//     registered: handleTask drops the registration (deferred Unregister)
//     the moment spawn.Run returns, after which the worker's session no
//     longer maps to a live worker. So spawn.Run SIGKILLs the worker's
//     process group when the task ends - on the normal-exit path as well as
//     on a timeout - and no descendant still in that group outlives the
//     task. Residuals (same-uid limit, owner decisions G03-1/2/3; the
//     sanctioned worker does neither), each needing a read of
//     <socket>.secret to matter:
//     (a) a forked child that calls setsid() gets a fresh session no
//     worker is registered under, so it is unattributable AT ONCE - also
//     while its worker's task is still running. Without the secret it is
//     refused everywhere but /health; WITH the secret it reaches the
//     control routes and has its declared ids honored on worker routes.
//     (b) a forked child that calls setpgid(0, 0) stays in the worker's
//     session, so while the task runs it is pinned on worker routes and
//     refused on control routes; but it escapes the group reap, and once
//     it outlives the task (the worker is unregistered) it is
//     unattributable, with the same consequences as (a).
package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"

	"kahya/kahyad/internal/config"
)

// MsgControlSecretRejected is the byte-exact Turkish body every control
// route answers (HTTP 403) when X-Kahya-Control-Secret is missing or
// wrong (G03 / B-3).
const MsgControlSecretRejected = "Kontrol anahtarı eksik ya da geçersiz (fail-closed). Bu uç nokta yalnızca kahya CLI, kahya-trigger ve Hammerspoon içindir."

// Control-auth rejection reasons, ledgered as control_auth_rejected's
// payload.reason.
const (
	controlRejectMissingSecret = "missing_secret"
	controlRejectBadSecret     = "bad_secret"
	// controlRejectUnattributable: a worker route called by a peer that is
	// not a live worker kahyad spawned, without the control secret (B-14).
	controlRejectUnattributable = "unattributable_worker_route"
	// controlRejectWorkerOnControl: a control route called from inside a
	// live worker's session - refused even with the secret (B-14).
	controlRejectWorkerOnControl = "live_worker_on_control_route"
	// controlRejectUnresolvedPeer: the connection's peer could not be
	// resolved to a session (gone before accept, or not a UDS peer), so a
	// live worker cannot be ruled out - refused on every non-open route,
	// even with the secret (B-14, fail-closed).
	controlRejectUnresolvedPeer = "unresolved_peer"
	// controlRejectNoTrace: an attributable live worker registered without
	// a trace_id cannot be pinned completely - fail closed (B-14).
	controlRejectNoTrace = "attributed_worker_without_trace"
	// controlRejectWorkerRawSearch: an attributed live worker asked
	// /v1/memory/search for the raw for_injection:false surface, which is
	// the local CLI's only (FIX-FINAL worker-raw-memory-search-for-
	// injection-false).
	controlRejectWorkerRawSearch = "worker_raw_memory_search"
)

// workerLookup is the narrow reverse lookup caller attribution needs (G03
// / B-14) - *kahyad/internal/task.LiveRegistry satisfies it (BySID).
// SetTaskLiveRegistry (task_durability.go) wires it from the same
// registry handleTask and the outbox dispatcher register workers into.
type workerLookup interface {
	BySID(sid int) (taskID, traceID string, ok bool)
}

// peerKey/peerInfo: what connContext records once per accepted UDS
// connection - the peer's pid, session id (the attribution key) and
// process group (ledger-only diagnostics: a worker child that moved
// groups shows pgid != sid). Each is 0 when unavailable; SID == 0 means
// the peer is unresolved.
type peerKey struct{}

type peerInfo struct{ PID, PGID, SID int }

// callerAttrKey/callerAttribution: what withControlAuth resolves PER
// REQUEST from peerInfo + the live worker registry, and hands to the
// worker-plane handlers (handlePolicyCheck, handleMemorySearch,
// handlePolicyConsumeToken) via the request context.
type callerAttrKey struct{}

type callerAttribution struct {
	PID     int    // peer pid, 0 if unavailable
	PGID    int    // peer process group, 0 if unavailable (diagnostics only)
	SID     int    // peer session id, 0 if unresolved (fail-closed)
	TaskID  string // live worker's task_id if SID maps to one, else ""
	TraceID string // that worker's trace_id, else ""
}

func callerAttributionFromContext(ctx context.Context) (callerAttribution, bool) {
	a, ok := ctx.Value(callerAttrKey{}).(callerAttribution)
	return a, ok
}

// connContext runs once per accepted UDS connection (http.Server.
// ConnContext, wired in Prepare) and records ONLY the OS-level peer pid,
// session id and process group - never a task id: live-registry
// membership can change over the life of a kept-alive connection, so that
// lookup is done per request in attributeCaller. The session is read HERE,
// at accept time, and kept for the connection's life: a peer that forks
// and exits cannot later make its connection look like someone else's.
// Any failure leaves SID == 0 - "unresolved", which withControlAuth
// refuses on every non-open route (fail-closed).
func (s *Server) connContext(ctx context.Context, c net.Conn) context.Context {
	info := peerInfo{}
	if pid, err := peerPIDFromConn(c); err == nil && pid > 0 {
		info.PID = pid
		if sid, err := peerSessionID(pid); err == nil && sid > 0 {
			info.SID = sid
		}
		if pgid, err := syscall.Getpgid(pid); err == nil {
			info.PGID = pgid
		}
	}
	return context.WithValue(ctx, peerKey{}, info)
}

// attributeCaller resolves the request's caller: peerInfo from the
// connection plus, when s.workerLookup is wired and the peer's session
// belongs to a worker kahyad itself spawned, that worker's task_id/
// trace_id.
func (s *Server) attributeCaller(r *http.Request) callerAttribution {
	info, _ := r.Context().Value(peerKey{}).(peerInfo)
	att := callerAttribution{PID: info.PID, PGID: info.PGID, SID: info.SID}
	if att.SID > 0 && s.workerLookup != nil {
		if tid, trace, ok := s.workerLookup.BySID(att.SID); ok {
			att.TaskID, att.TraceID = tid, trace
		}
	}
	return att
}

// ledgerIdentityOverride records kind=worker_identity_override under the
// ATTRIBUTED trace_id whenever a live worker named ids other than its own
// (source: "header" for the X-Kahya-* headers, "body" for the JSON
// routes). Never fatal: the request proceeds with the pinned ids.
func (s *Server) ledgerIdentityOverride(r *http.Request, att callerAttribution, source, claimedTask, claimedTrace string) {
	payload := map[string]any{
		"event": "worker_identity_override", "path": r.URL.Path, "source": source,
		"peer_pid": att.PID, "peer_pgid": att.PGID, "peer_sid": att.SID,
		"task_id": att.TaskID, "claimed_task_id": claimedTask, "claimed_trace_id": claimedTrace,
	}
	if s.eventLogger != nil {
		if err := s.eventLogger.LogEvent(r.Context(), att.TraceID, "worker_identity_override", payload); err != nil {
			s.log.With(att.TraceID).Warn("worker_identity_override_ledger_error", "err", err.Error())
		}
	}
	s.log.With(att.TraceID).Warn("worker_identity_override", "path", r.URL.Path, "source", source, "claimed_task_id", claimedTask)
}

// pinBodyIdentity returns the (taskID, traceID) a body-based worker-route
// handler must use. For a caller attributable to a live worker that is
// the worker's OWN ids (a conflicting body claim is ledgered via
// ledgerIdentityOverride and ignored; an omitted one is simply filled in);
// for an unattributed caller - which withControlAuth only lets through
// with the control secret - the body's claim stands unchanged.
func (s *Server) pinBodyIdentity(r *http.Request, claimedTask, claimedTrace string) (taskID, traceID string) {
	att, ok := callerAttributionFromContext(r.Context())
	if !ok || att.TaskID == "" {
		return claimedTask, claimedTrace
	}
	if (claimedTask != "" && claimedTask != att.TaskID) || (claimedTrace != "" && claimedTrace != att.TraceID) {
		s.ledgerIdentityOverride(r, att, "body", claimedTask, claimedTrace)
	}
	return att.TaskID, att.TraceID
}

// routeClass partitions kahyad's UDS routes (G03). Anything not listed
// in openPaths/workerPaths - including a path no handler serves (would
// be 404) - is control-plane: fail-closed by default, so a route added
// later without a classification is protected, never exposed.
type routeClass int

const (
	routeControl routeClass = iota
	routeWorker
	routeOpen
)

// String names the class for test failure messages and logs.
func (c routeClass) String() string {
	switch c {
	case routeOpen:
		return "open"
	case routeWorker:
		return "worker"
	default:
		return "control"
	}
}

// openPaths need no caller identity at all: /health is the liveness probe
// probeHealth itself (and every script's readiness loop) depends on.
var openPaths = map[string]bool{"/health": true}

// workerPaths are the ONLY routes a spawned worker (and its kahya-mcp
// bridge child) may reach: the can_use_tool policy check, the
// UserPromptSubmit memory search, the MCP tool surface, and the token
// consume step. Everything else is for the kahya CLI / kahya-trigger /
// Hammerspoon (via the CLI) only. Since B-14 a worker route is served
// without the secret only to a caller attributable to a live worker.
var workerPaths = map[string]bool{
	"/policy/check":         true,
	"/v1/memory/search":     true,
	"/v1/mcp":               true,
	"/policy/consume-token": true,
}

// classifyRoute classifies path by EXACT match. Patterned paths
// (/approvals/x/decision, /jobs/trigger/x) are in neither map, so they
// are control - as intended. Unclean paths (//policy/check,
// /x/../policy/check) are in neither map either, so they are control too:
// ServeMux's own path-cleaning redirect runs INSIDE this gate, so a
// worker cannot reach a control handler dressed up as a worker path.
func classifyRoute(path string) routeClass {
	switch {
	case openPaths[path]:
		return routeOpen
	case workerPaths[path]:
		return routeWorker
	default:
		return routeControl
	}
}

// mintControlSecret writes a fresh 32-byte hex secret to path with mode
// 0600, atomically (temp file + rename), and returns it. Called from
// Prepare while the startup flock is held, so no other kahyad can race
// it; rotated on every boot - clients read the file per request.
func mintControlSecret(path string) (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("server: mint control secret: %w", err)
	}
	secret := hex.EncodeToString(raw[:])
	tmp := path + ".tmp"
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("server: create control secret %s: %w", tmp, err)
	}
	if _, err := f.WriteString(secret + "\n"); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return "", fmt.Errorf("server: write control secret: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("server: close control secret: %w", err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("server: chmod control secret: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("server: rename control secret: %w", err)
	}
	return secret, nil
}

// hasValidControlSecret reports whether r carries the exact per-boot
// secret (constant-time compare; an unset server secret never matches).
func (s *Server) hasValidControlSecret(r *http.Request) bool {
	got := strings.TrimSpace(r.Header.Get(config.ControlSecretHeader))
	if s.controlSecret == "" || got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.controlSecret)) == 1
}

// rejectControl answers 403 with MsgControlSecretRejected and ledgers
// kind=control_auth_rejected (reason: one of the controlReject* values)
// under the request's trace_id (withTraceLogging is the OUTER middleware,
// so traceIDFromContext is always non-empty here), with the peer's
// pid/pgid/sid and - when attributable - the live worker's task_id.
func (s *Server) rejectControl(w http.ResponseWriter, r *http.Request, reason string) {
	traceID := traceIDFromContext(r)
	payload := map[string]any{
		"event": "control_auth_rejected", "path": r.URL.Path, "method": r.Method, "reason": reason,
	}
	if att, ok := callerAttributionFromContext(r.Context()); ok {
		payload["peer_pid"], payload["peer_pgid"], payload["peer_sid"] = att.PID, att.PGID, att.SID
		if att.TaskID != "" {
			payload["task_id"] = att.TaskID
		}
	}
	if s.eventLogger != nil {
		if err := s.eventLogger.LogEvent(r.Context(), traceID, "control_auth_rejected", payload); err != nil {
			s.log.With(traceID).Warn("control_auth_rejected_ledger_error", "err", err.Error())
		}
	}
	s.log.With(traceID).Warn("control_auth_rejected", "path", r.URL.Path, "reason", reason)
	writeJSONError(w, http.StatusForbidden, MsgControlSecretRejected)
}

// withControlAuth is the G03 gate in front of the whole mux (see this
// file's package doc): open routes pass; an unresolved peer is refused
// everywhere else; worker routes pin an attributable live worker's ids or
// require the secret from anyone else; control routes refuse any live
// worker and require the secret from everyone else.
func (s *Server) withControlAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		class := classifyRoute(r.URL.Path)
		if class == routeOpen {
			next.ServeHTTP(w, r)
			return
		}
		att := s.attributeCaller(r)
		r = r.WithContext(context.WithValue(r.Context(), callerAttrKey{}, att))

		if att.SID <= 0 {
			// No session for this peer: it may be a live worker (e.g. a
			// child holding the fd of a connection its exited parent
			// opened), so neither the worker nor the control rule can be
			// applied - refuse, even with the secret.
			s.rejectControl(w, r, controlRejectUnresolvedPeer)
			return
		}

		if class == routeWorker {
			if att.TaskID != "" {
				if att.TraceID == "" {
					s.rejectControl(w, r, controlRejectNoTrace)
					return
				}
				// A caller attributable to a live worker kahyad spawned:
				// pin the correlation ids to that worker's OWN task.
				// /v1/mcp reads them from headers (policyGateMiddleware,
				// mcp/fs, mcp/shell, mcp/osascript, mcp/memory), the other
				// three routes from the JSON body - so (1) overwrite the
				// headers here, (2) the body-based handlers call
				// pinBodyIdentity with the att stashed above, (3) re-stash
				// the trace id so traceIDFromContext agrees with the pinned
				// identity downstream. The outer event=http_request line
				// keeps the DECLARED trace on purpose (docs/ipc.md 5a).
				claimedTrace, claimedTask := r.Header.Get(traceHeader), r.Header.Get(taskHeader)
				if (claimedTrace != "" && claimedTrace != att.TraceID) || (claimedTask != "" && claimedTask != att.TaskID) {
					s.ledgerIdentityOverride(r, att, "header", claimedTask, claimedTrace)
				}
				r.Header.Set(traceHeader, att.TraceID)
				r.Header.Set(taskHeader, att.TaskID)
				r = r.WithContext(context.WithValue(r.Context(), traceIDContextKey{}, att.TraceID))
				next.ServeHTTP(w, r)
				return
			}
			// Not attributable to any live worker (a shell script, a test
			// harness, the CLI): allowed ONLY with the control secret.
			if s.hasValidControlSecret(r) {
				next.ServeHTTP(w, r)
				return
			}
			s.rejectControl(w, r, controlRejectUnattributable)
			return
		}

		// Control route.
		if att.TaskID != "" {
			s.rejectControl(w, r, controlRejectWorkerOnControl)
			return
		}
		if !s.hasValidControlSecret(r) {
			reason := controlRejectBadSecret
			if strings.TrimSpace(r.Header.Get(config.ControlSecretHeader)) == "" {
				reason = controlRejectMissingSecret
			}
			s.rejectControl(w, r, reason)
			return
		}
		next.ServeHTTP(w, r)
	})
}
