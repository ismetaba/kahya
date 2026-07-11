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
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"kahya/kahyad/internal/traceid"
)

// Proxy is the W3-05 forward proxy.
type Proxy struct {
	Gate *Gate
	// Addr is the listen address (127.0.0.1:<config egress.port>).
	Addr string
	// Dial resolves a CONNECT tunnel's upstream connection (production:
	// a plain net.Dialer.DialContext). Tests override this to prove a
	// denied CONNECT dials nothing at all.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// Transport forwards a plain-HTTP request once the gate allows it
	// (production: http.DefaultTransport). Tests override this the same
	// way Dial is overridden.
	Transport http.RoundTripper

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

	session := SessionInfo{TraceID: traceid.New()}
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

	// Only AFTER the gate allows do we ever dial the upstream (this
	// file's own doc comment).
	upstream, err := p.Dial(r.Context(), "tcp", net.JoinHostPort(host, portStr))
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

	var mu sync.Mutex
	var total int64
	add := func(n int64) {
		mu.Lock()
		total += n
		mu.Unlock()
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
		n, _ := io.Copy(upstream, clientBuf.Reader)
		add(n)
	}()
	go func() {
		defer wg.Done()
		// The reverse direction has no such buffering concern (nothing
		// upstream of the server ever wrote to the client on our behalf
		// before this point) — a plain write straight to the raw conn is
		// correct.
		n, _ := io.Copy(client, upstream)
		add(n)
	}()
	wg.Wait()

	p.Gate.MeterUsage(context.Background(), host, total, session)
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

	nbytes := r.ContentLength
	if nbytes < 0 {
		nbytes = 0
	}

	session := SessionInfo{TraceID: traceid.New()}
	decision, err := p.Gate.Check(r.Context(), Target{Host: host, Port: port}, nbytes, session)
	if err != nil {
		denyResponse(w, "Egress kapisi hata verdi; guvenlik geregi reddedildi.")
		return
	}
	if !decision.Allow {
		denyResponse(w, decision.Reason)
		return
	}

	outReq := r.Clone(r.Context())
	outReq.RequestURI = ""
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
	n, _ := io.Copy(w, resp.Body)
	p.Gate.MeterUsage(context.Background(), host, n, session)
}
