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
// narrows it further for the worker routes.
package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"strings"

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
)

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
// Hammerspoon (via the CLI) only.
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
// kind=control_auth_rejected (reason: missing_secret|bad_secret) under the
// request's trace_id (withTraceLogging is the OUTER middleware, so
// traceIDFromContext is always non-empty here).
func (s *Server) rejectControl(w http.ResponseWriter, r *http.Request, reason string) {
	traceID := traceIDFromContext(r)
	payload := map[string]any{
		"event": "control_auth_rejected", "path": r.URL.Path, "method": r.Method, "reason": reason,
	}
	if s.eventLogger != nil {
		if err := s.eventLogger.LogEvent(r.Context(), traceID, "control_auth_rejected", payload); err != nil {
			s.log.With(traceID).Warn("control_auth_rejected_ledger_error", "err", err.Error())
		}
	}
	s.log.With(traceID).Warn("control_auth_rejected", "path", r.URL.Path, "reason", reason)
	writeJSONError(w, http.StatusForbidden, MsgControlSecretRejected)
}

// withControlAuth is the G03 gate in front of the whole mux: control
// routes require the secret; worker/open routes pass through here.
func (s *Server) withControlAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch classifyRoute(r.URL.Path) {
		case routeOpen, routeWorker:
			next.ServeHTTP(w, r)
		default:
			if !s.hasValidControlSecret(r) {
				reason := controlRejectBadSecret
				if strings.TrimSpace(r.Header.Get(config.ControlSecretHeader)) == "" {
					reason = controlRejectMissingSecret
				}
				s.rejectControl(w, r, reason)
				return
			}
			next.ServeHTTP(w, r)
		}
	})
}
