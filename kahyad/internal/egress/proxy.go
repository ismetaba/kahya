// proxy.go implements the W3-05 HTTP(S) forward proxy: the ONLY route out
// of the kahya-egress Docker network (HANDOFF §5 safety #1 flag:
// "agin gerektiren isler yalniz kahyad'in egress proxy'si ... uzerinden
// cikar — aksi halde container ici curl allowlist'i atlar"). mcp/shell's
// needs_network container jobs reach this listener via the
// kahya-egress-fwd sidecar (mcp/shell/egress_network.go), which is a
// DUMB TCP forward — ALL policy lives here, in kahyad Go code, never in
// the sidecar.
//
// Plain HTTP: the target host is read straight from the request URL,
// gated via Gate.Check, then forwarded. HTTPS (CONNECT): the gate
// decision is made on host:port BEFORE this proxy ever dials the
// upstream — a denied CONNECT opens no TCP connection at all (Dial is
// only ever called after decision.Allow) — then the two sides are
// tunneled opaquely while both directions' combined byte count is
// metered into the budget once the tunnel closes (Gate.MeterUsage — a
// CONNECT tunnel is opaque after the handshake, so there is nothing to
// inspect mid-flight, only a total to observe once it ends).
//
// This proxy resolves DNS itself (via Dial, a net.Dialer by default):
// clients on the kahya-egress internal Docker network get NO DNS server
// at all (an --internal network has none), so name resolution cannot
// leak a bypass route independent of this proxy's own gate.
package egress

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"kahya/kahyad/internal/traceid"
)

// Resolver resolves host to its IP addresses — BLOCKER D (DNS rebinding):
// production uses net.DefaultResolver.LookupIPAddr directly (*net.Resolver
// already has this exact method), tests inject a fake mapping so a
// rebinding scenario (an allowlisted hostname resolving to a private/
// link-local/loopback address) is reproducible with no real DNS or
// /etc/hosts change involved.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// Proxy is the W3-05 forward proxy.
type Proxy struct {
	Gate *Gate
	// Addr is the listen address (127.0.0.1:<config egress.port>).
	Addr string
	// Dial resolves a CONNECT tunnel's upstream connection (production:
	// a plain net.Dialer.DialContext). Tests override this to prove a
	// denied CONNECT dials nothing at all. BLOCKER D: Dial is now called
	// with an ALREADY-RESOLVED, ALREADY-VALIDATED IP literal (never the
	// original hostname) — see resolveAndPin.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// Transport forwards a plain-HTTP request once the gate allows it
	// (production: http.DefaultTransport). Tests override this the same
	// way Dial is overridden.
	Transport http.RoundTripper
	// Resolver resolves an allowlisted HOSTNAME's IP addresses before
	// every dial (BLOCKER D). Defaults to net.DefaultResolver.
	Resolver Resolver
	// Tokens maps a per-task egress-proxy credential (carried in the
	// CONNECT/plain-HTTP request's Proxy-Authorization header — see
	// sessionForRequest) to that task's own SessionInfo (BLOCKER B/C).
	// nil (the zero value) means every request is attributed to a fresh,
	// anonymous, untainted session — this proxy's ORIGINAL behavior,
	// still fully allowlist+budget-gated, just never sensitive-read-
	// tainted. kahyad's real wiring (main.go) always sets this to the
	// SAME *ProxySessionRegistry mcp/shell.Runner registers tokens into
	// (via kahyad/internal/server.NewEgressTokenRegistrar).
	Tokens *ProxySessionRegistry

	ln  net.Listener
	srv *http.Server
}

// NewProxy constructs a Proxy bound to gate's decisions, listening on
// 127.0.0.1:port.
func NewProxy(gate *Gate, port int) *Proxy {
	return &Proxy{
		Gate:      gate,
		Addr:      fmt.Sprintf("127.0.0.1:%d", port),
		Dial:      (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		Transport: http.DefaultTransport,
		Resolver:  net.DefaultResolver,
	}
}

// Start binds the listener and begins serving in the background.
func (p *Proxy) Start() error {
	ln, err := net.Listen("tcp", p.Addr)
	if err != nil {
		return fmt.Errorf("egress: listen %s: %w", p.Addr, err)
	}
	p.ln = ln
	p.srv = &http.Server{Handler: p}
	go func() { _ = p.srv.Serve(ln) }()
	return nil
}

// Close stops the listener.
func (p *Proxy) Close() error {
	if p.srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return p.srv.Shutdown(ctx)
}

// ServeHTTP dispatches CONNECT (TLS tunneling) vs every other method
// (plain HTTP forwarding).
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	p.handlePlainHTTP(w, r)
}

// denyResponse writes decision's Turkish reason (or a generic fallback)
// as a 403 — used identically by both the CONNECT and plain-HTTP paths.
func denyResponse(w http.ResponseWriter, reason string) {
	if reason == "" {
		reason = "Egress reddedildi."
	}
	http.Error(w, reason, http.StatusForbidden)
}

// proxyAuthToken extracts the per-task egress-proxy credential from r's
// Proxy-Authorization header (RFC 7235 §4.4 — the proxy-specific
// counterpart of Authorization): the "Basic base64(token:)" scheme a
// needs_network:true container's HTTP_PROXY/HTTPS_PROXY env
// (mcp/shell/egress_network.go's egressProxyEnv) encodes its per-task
// token into as the proxy URL's userinfo (e.g.
// "http://<token>:@kahya-egress-fwd:3128" — curl/wget and most HTTP
// clients send that userinfo as Proxy-Authorization automatically, on
// every request through that proxy, CONNECT or plain, with zero extra
// container-side configuration). Returns "" for anything else (missing
// header, wrong scheme, malformed base64, no ":" separator) — an empty
// token is exactly the "UNKNOWN, untainted session" case
// sessionForRequest already handles via the ordinary empty-SessionID
// no-taint path.
func proxyAuthToken(r *http.Request) string {
	h := r.Header.Get("Proxy-Authorization")
	const prefix = "Basic "
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(h, prefix))
	if err != nil {
		return ""
	}
	token, _, ok := strings.Cut(string(decoded), ":")
	if !ok {
		return ""
	}
	return token
}

// sessionForRequest resolves r's SessionInfo (BLOCKER B/C): if a
// per-task egress-proxy token is present AND registered (p.Tokens), the
// request is attributed to THAT task's own SessionInfo (SessionID keyed
// on its trace_id — see egress.ProxySessionRegistry's own doc comment for
// how it got there); otherwise a fresh, anonymous, untainted session is
// minted — Gate.Check's ordinary "no session to taint-check" path already
// handles this correctly: fail-closed on TAINT is not possible without an
// identity to key it on, but the allowlist/budget gate is still fully
// enforced regardless (this is the documented, intentional floor for a
// request this proxy cannot attribute to any known task).
func (p *Proxy) sessionForRequest(r *http.Request) SessionInfo {
	if p.Tokens != nil {
		if token := proxyAuthToken(r); token != "" {
			if session, ok := p.Tokens.Lookup(token); ok {
				return session
			}
		}
	}
	return SessionInfo{TraceID: traceid.New()}
}

// resolveAndPin resolves host (already Gate-approved — this is called
// AFTER Gate.Check has returned Allow=true for it) to a single public IP
// literal to dial, closing BLOCKER D (DNS rebinding): isPrivateOrLinkLocal
// only ever inspected a bare IP LITERAL present in the original request;
// an ALLOWLISTED HOSTNAME's actual DNS answer was never checked at all,
// so a hijacked/rebound record for e.g. api.anthropic.com could silently
// pivot every dial at a private address. host that is already an IP
// literal is returned unchanged (no resolution possible or needed — its
// own literal value is what Gate.Check's isPrivateOrLinkLocal path
// already reasons about). For a HOSTNAME, EVERY resolved address is
// required to be public — rejecting the whole result if even ONE
// resolved address is private/link-local/loopback (a DNS response may
// list several A/AAAA records; trusting only the "safe-looking" one and
// ignoring the rest would still let an attacker who controls even one
// authoritative answer redirect a later connection/retry to it) — and the
// FIRST validated address is returned so the caller's actual TCP
// connection is PINNED to a Gate-checked literal, never re-resolved by
// the dialer/transport afterward (the whole point: what was checked is
// exactly what gets dialed).
func resolveAndPin(ctx context.Context, resolver Resolver, host string) (string, error) {
	if IsIPLiteral(host) {
		return host, nil
	}
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	addrs, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return "", fmt.Errorf("resolve %s: no addresses", host)
	}
	for _, a := range addrs {
		if isPrivateOrLinkLocal(a.IP.String()) {
			return "", fmt.Errorf("resolved address %s for %s is private/link-local", a.IP.String(), host)
		}
	}
	return addrs[0].IP.String(), nil
}

// handleConnect implements the HTTPS tunneling path: gate on host:port
// BEFORE dialing, tunnel bytes both directions once allowed, meter the
// observed total after the tunnel closes.
func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	host, portStr, err := net.SplitHostPort(r.Host)
	if err != nil {
		host, portStr = r.Host, "443"
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		http.Error(w, "gecersiz CONNECT hedef portu", http.StatusBadRequest)
		return
	}

	// BLOCKER B/C: attribute this connection to a per-task session via its
	// Proxy-Authorization credential (needs_network:true containers), not
	// an always-fresh, always-anonymous one.
	session := p.sessionForRequest(r)
	decision, err := p.Gate.Check(r.Context(), Target{Host: host, Port: port}, 0, session)
	if err != nil {
		// Fail-closed (tasks/README.md global convention): a gate error
		// is treated exactly like a deny, never a permissive fallback.
		denyResponse(w, "Egress kapisi hata verdi; guvenlik geregi reddedildi.")
		return
	}
	if !decision.Allow {
		denyResponse(w, decision.Reason)
		return
	}

	// FINDING #8: Check admitted this tunnel with nbytes=0 (an opaque
	// CONNECT has no pre-known size), so its budget test alone would only
	// ever refuse a host ALREADY over budget — never THIS tunnel as it
	// streams past the limit. Fetch the host's REMAINING daily budget here
	// so the io.Copy loops below can enforce it as a RUNNING cap, and deny
	// up front (never even dialing) when the host has no budget left.
	remaining, limited, err := p.Gate.RemainingBudget(r.Context(), host)
	if err != nil {
		// Fail-closed (tasks/README.md): a budget-store error is a deny.
		denyResponse(w, "Egress kapisi hata verdi; guvenlik geregi reddedildi.")
		return
	}
	if limited && remaining <= 0 {
		d := p.Gate.DenyBudget(r.Context(), host, session)
		denyResponse(w, d.Reason)
		return
	}

	// BLOCKER D: resolve+validate host's OWN DNS answer before ever
	// dialing, even though Check already allowed the hostname itself —
	// see resolveAndPin's doc comment.
	dialHost, resolveErr := resolveAndPin(r.Context(), p.Resolver, host)
	if resolveErr != nil {
		d := p.Gate.DenyDNSRebind(r.Context(), host, session)
		denyResponse(w, d.Reason)
		return
	}

	// Only AFTER the gate allows AND the resolved address is validated do
	// we ever dial the upstream (this file's own doc comment) — dialing
	// dialHost (the PINNED, already-checked literal), never host itself,
	// so the dialer/transport can never independently re-resolve to a
	// different (possibly hostile) address.
	upstream, err := p.Dial(r.Context(), "tcp", net.JoinHostPort(dialHost, portStr))
	if err != nil {
		http.Error(w, "yukari akis baglantisi kurulamadi", http.StatusBadGateway)
		return
	}
	defer upstream.Close()

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack desteklenmiyor", http.StatusInternalServerError)
		return
	}
	client, clientBuf, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer client.Close()

	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}

	// FINDING #8: both tunnel directions share ONE atomic byte counter and
	// the SAME remaining-budget cap. total accumulates only bytes actually
	// FORWARDED, so MeterUsage below is exact. When limited, a goroutine
	// that is about to push the running total past remaining severs BOTH
	// ends and stops BEFORE forwarding that buffer — bounding the overshoot
	// to at most one in-flight buffer per direction (the check-then-write
	// race between the two goroutines can add one peer buffer, never
	// unbounded). Closing both conns unblocks the peer goroutine's blocked
	// Read/Write so the WaitGroup always completes.
	var total atomic.Int64
	var capped atomic.Bool
	copyDir := func(dst io.Writer, src io.Reader) {
		buf := make([]byte, 32*1024)
		for {
			nr, er := src.Read(buf)
			if nr > 0 {
				if limited && total.Load()+int64(nr) > remaining {
					capped.Store(true)
					_ = client.Close()
					_ = upstream.Close()
					return
				}
				if _, ew := dst.Write(buf[:nr]); ew != nil {
					return
				}
				total.Add(int64(nr))
			}
			if er != nil {
				return
			}
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		// clientBuf.Reader, NOT client directly: the client may have
		// already pipelined bytes (e.g. the TLS ClientHello) right after
		// the CONNECT request, in the SAME TCP segment the stdlib's own
		// bufio.Reader read the request line/headers from — those bytes
		// live in clientBuf.Reader's buffer, not on the raw net.Conn, and
		// reading from client directly here would silently drop them.
		copyDir(upstream, clientBuf.Reader)
	}()
	go func() {
		defer wg.Done()
		// The reverse direction has no such buffering concern (nothing
		// upstream of the server ever wrote to the client on our behalf
		// before this point) — a plain write straight to the raw conn is
		// correct.
		copyDir(client, upstream)
	}()
	wg.Wait()

	p.Gate.MeterUsage(context.Background(), host, total.Load(), session)
	if capped.Load() {
		// The tunnel was severed at the daily-budget boundary: record it as
		// a budget denial (EventBlockedBudget + Turkish notify), consistent
		// with every other budget refusal, so the ledger shows WHY the
		// connection ended.
		p.Gate.DenyBudget(context.Background(), host, session)
	}
}

// handlePlainHTTP implements the plain-HTTP forwarding path: the target
// host/port come straight from the request URL (a proxy request always
// carries an absolute-form URI, RFC 7230 §5.3.2).
func (p *Proxy) handlePlainHTTP(w http.ResponseWriter, r *http.Request) {
	if !r.URL.IsAbs() {
		http.Error(w, "proxy: mutlak istek URI'si gerekli", http.StatusBadRequest)
		return
	}
	host := r.URL.Hostname()
	portStr := r.URL.Port()
	if portStr == "" {
		portStr = "80"
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		http.Error(w, "gecersiz port", http.StatusBadRequest)
		return
	}

	// BLOCKER B/C: attribute this request to a per-task session via its
	// Proxy-Authorization credential (needs_network:true containers), not
	// an always-fresh, always-anonymous one.
	session := p.sessionForRequest(r)
	// FINDING #13: admit with nbytes=0, exactly like the CONNECT path. The
	// request body's ACTUAL bytes are metered post-hoc (reqCounter +
	// MeterUsage below); passing r.ContentLength here too would Add them to
	// the budget a SECOND time (Check.Add at admission, then MeterUsage
	// after RoundTrip). With nbytes=0 Check still denies a host already over
	// budget but Adds nothing (gate.go guards Add on nbytes>0), leaving
	// MeterUsage the sole request-body count.
	decision, err := p.Gate.Check(r.Context(), Target{Host: host, Port: port}, 0, session)
	if err != nil {
		denyResponse(w, "Egress kapisi hata verdi; guvenlik geregi reddedildi.")
		return
	}
	if !decision.Allow {
		denyResponse(w, decision.Reason)
		return
	}

	// FINDING #8: fetch the host's REMAINING daily budget so the response
	// body copy below can be enforced as a RUNNING cap — a large download on
	// an allowlisted host would otherwise stream unbounded past the budget,
	// blocking only the NEXT request. Deny up front when nothing is left.
	remaining, limited, err := p.Gate.RemainingBudget(r.Context(), host)
	if err != nil {
		// Fail-closed (tasks/README.md): a budget-store error is a deny.
		denyResponse(w, "Egress kapisi hata verdi; guvenlik geregi reddedildi.")
		return
	}
	if limited && remaining <= 0 {
		d := p.Gate.DenyBudget(r.Context(), host, session)
		denyResponse(w, d.Reason)
		return
	}

	// BLOCKER D: resolve+validate host's OWN DNS answer before ever
	// dialing, even though Check already allowed the hostname itself —
	// see resolveAndPin's doc comment.
	dialHost, resolveErr := resolveAndPin(r.Context(), p.Resolver, host)
	if resolveErr != nil {
		d := p.Gate.DenyDNSRebind(r.Context(), host, session)
		denyResponse(w, d.Reason)
		return
	}

	// MINOR F: wrap the request body (if any) in a counting reader BEFORE
	// cloning — r.Clone copies the Body field by reference, so outReq.Body
	// below is this SAME counter, and every byte RoundTrip reads from it
	// (and therefore streams upstream) is counted for real, regardless of
	// whether Content-Length was known up front.
	var reqCounter *countingReadCloser
	if r.Body != nil {
		reqCounter = &countingReadCloser{ReadCloser: r.Body}
		r.Body = reqCounter
	}

	outReq := r.Clone(r.Context())
	outReq.RequestURI = ""
	// BLOCKER D: dial the PINNED, already-validated IP literal, never
	// host itself — outReq.Host preserves the ORIGINAL Host header
	// (virtual hosting on the upstream still sees the hostname the client
	// asked for; only the actual TCP connection target is pinned).
	outReq.Host = r.URL.Host
	outReq.URL.Host = net.JoinHostPort(dialHost, portStr)
	resp, err := p.Transport.RoundTrip(outReq)
	if err != nil {
		http.Error(w, "yukari akis istegi basarisiz", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	// FINDING #8: cap the response body at the host's remaining daily budget
	// (a RUNNING limit), mirroring the CONNECT tunnel — a large streaming
	// download must not blow past the budget and only block the NEXT
	// request. n counts bytes actually forwarded, so MeterUsage stays exact.
	n, capped := copyCapped(w, resp.Body, remaining, limited)

	var reqBytes int64
	if reqCounter != nil {
		reqBytes = reqCounter.n
	}
	p.Gate.MeterUsage(context.Background(), host, n+reqBytes, session)
	if capped {
		// The response body was severed at the daily-budget boundary: record
		// it as a budget denial (EventBlockedBudget + Turkish notify),
		// consistent with the CONNECT cap and every other budget refusal.
		p.Gate.DenyBudget(context.Background(), host, session)
	}
}

// copyCapped streams src->dst like io.Copy, but when limited it stops the
// moment forwarding the next buffer would push the running total past
// remaining — returning how many bytes it actually forwarded and whether it
// was cut short by the cap (FINDING #8). It checks BEFORE each dst.Write, so
// the forwarded total never exceeds remaining (the read overshoots by at
// most one buffer, which is never forwarded). When !limited it is a plain
// copy that never caps.
func copyCapped(dst io.Writer, src io.Reader, remaining int64, limited bool) (written int64, capped bool) {
	buf := make([]byte, 32*1024)
	for {
		nr, er := src.Read(buf)
		if nr > 0 {
			if limited && written+int64(nr) > remaining {
				return written, true
			}
			wn, ew := dst.Write(buf[:nr])
			written += int64(wn)
			if ew != nil {
				return written, false
			}
		}
		if er != nil {
			return written, false
		}
	}
}

// countingReadCloser wraps an io.ReadCloser, counting every byte Read
// returns (MINOR F fix): a chunked request body has no Content-Length at
// all (handlePlainHTTP's own nbytes pre-admission estimate clamps to 0
// for it), so without this, that body's real size never reaches the
// budget at all, even after the fact — this counts the ACTUAL bytes
// streamed upstream (via RoundTrip reading from it), independent of
// whatever Content-Length (if any) the request declared.
type countingReadCloser struct {
	io.ReadCloser
	n int64
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.n += int64(n)
	return n, err
}
