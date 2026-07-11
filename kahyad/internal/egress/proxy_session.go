// proxy_session.go implements the W3-05 BLOCKER B/C fix's proxy-side
// session-attribution registry: kahyad/internal/egress.Proxy (proxy.go)
// is the shared, daemon-lifetime forward-proxy every needs_network:true
// container (mcp/shell) reaches through the kahya-egress-fwd sidecar —
// unlike the per-task anthproxy listener (kahyad/internal/anthproxy),
// this ONE listener serves every task at once, so it cannot infer which
// task a given TCP connection belongs to from the listener itself. Before
// this fix, every request the proxy served minted a brand-new random
// trace_id and an EMPTY SessionID, so Gate.Check's sensitive-read check
// (`session.SessionID != "" && ...`) could never fire for container
// egress at all, no matter what mcp/fs's fs_read seam had marked.
//
// The fix: a needs_network:true container's HTTP_PROXY/HTTPS_PROXY
// environment (mcp/shell/egress_network.go's egressProxyEnv) carries a
// per-task, kahyad-minted credential as the proxy URL's Basic-auth
// userinfo (e.g. "http://<token>:@kahya-egress-fwd:3128" — curl/wget send
// that userinfo as a Proxy-Authorization header automatically on every
// request through that proxy, CONNECT or plain). kahyad registers
// token -> this task's own trace_id in THIS registry the moment it opens
// the container's network access (mcp/shell.Runner.Run, via the
// EgressTokenRegistrar seam), and releases it once that container's run
// has exited. proxy.go's sessionForRequest looks the token back up here
// on every inbound request, so a needs_network:true container's egress is
// attributed to the SAME trace_id-keyed session mcp/fs's fs_read seam
// (BLOCKER A) and the anthproxy egress-gate factory (anthproxy_hook.go)
// both use — one consistent taint/session key, everywhere in W3.
//
// A request carrying no token, or one this registry does not recognize
// (never registered, or already released), is treated as an ANONYMOUS,
// UNTAINTED session (SessionID "") — Gate.Check's ordinary "no session to
// taint-check" path already handles that fail-open-on-taint-but-still-
// allowlist+budget-gated posture correctly; see proxy.go's
// sessionForRequest doc comment for why that is the correct behavior
// here (fail-closed on taint is not POSSIBLE without an identity to key
// it on — the allowlist/budget gate is still fully enforced regardless).
package egress

import "sync"

// ProxySessionRegistry maps a per-task egress-proxy credential (the
// Proxy-Authorization Basic-auth token a needs_network:true container's
// HTTP_PROXY/HTTPS_PROXY carries) to that task's own SessionInfo. Safe
// for concurrent use — kahyad registers/releases tokens from
// mcp/shell.Runner.Run's own goroutine while proxy.go's HTTP server
// handles requests concurrently on others.
type ProxySessionRegistry struct {
	mu  sync.Mutex
	byT map[string]SessionInfo
}

// NewProxySessionRegistry constructs an empty ProxySessionRegistry.
func NewProxySessionRegistry() *ProxySessionRegistry {
	return &ProxySessionRegistry{byT: make(map[string]SessionInfo)}
}

// Register binds token to session. An empty token is a no-op (there is
// nothing to key a lookup on) — mirrors SensitiveTracker.Mark's identical
// "empty key is always a deliberate no-op" convention.
func (r *ProxySessionRegistry) Register(token string, session SessionInfo) {
	if token == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byT[token] = session
}

// Release forgets token (mcp/shell.Runner.Run calls this once the
// container that was issued it has exited) — a token's validity window is
// exactly that ONE container's own run, never reused across containers.
func (r *ProxySessionRegistry) Release(token string) {
	if token == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byT, token)
}

// Lookup returns token's registered SessionInfo, if any.
func (r *ProxySessionRegistry) Lookup(token string) (SessionInfo, bool) {
	if token == "" {
		return SessionInfo{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.byT[token]
	return s, ok
}
