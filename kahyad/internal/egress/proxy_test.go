package egress

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"kahya/kahyad/internal/policy"
)

// startProxy builds a Proxy around gate, bound to an ephemeral
// 127.0.0.1 port, and returns its address plus a cleanup-registered
// Close.
func startProxy(t *testing.T, gate *Gate) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close() // NewProxy binds its own listener on this port below

	p := NewProxy(gate, port)
	if err := p.Start(); err != nil {
		t.Fatalf("Proxy.Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p.Addr
}

// dialTracker records whether a Proxy's Dial func was ever invoked -
// used to prove a denied CONNECT never dials the upstream at all (this
// task's own acceptance criterion, verbatim).
type dialTracker struct {
	called bool
}

func proxyURL(addr string) func(*http.Request) (*url.URL, error) {
	u, err := url.Parse("http://" + addr)
	if err != nil {
		panic(err)
	}
	return http.ProxyURL(u)
}

func TestProxy_CONNECT_AllowlistedHostSucceeds(t *testing.T) {
	// A local TLS test server plays the role of the "allowlisted local
	// TLS test server" the acceptance criterion names.
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello-from-backend"))
	}))
	defer backend.Close()

	backendAddr := backend.Listener.Addr().(*net.TCPAddr)
	cfg := policy.EgressConfig{
		Allowlist:              []policy.EgressAllowEntry{{Host: "127.0.0.1", Ports: []int{backendAddr.Port}}},
		DefaultDailyByteBudget: 1 << 20,
	}
	gate := NewGate(cfg, NewSensitiveTracker(), newFakeBudget(), &fakeLedger{}, nil, nil)
	proxyAddr := startProxy(t, gate)

	httpClient := &http.Client{
		Transport: &http.Transport{
			Proxy:           proxyURL(proxyAddr),
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test-only, self-signed httptest cert
		},
		Timeout: 5 * time.Second,
	}

	resp, err := httpClient.Get(backend.URL)
	if err != nil {
		t.Fatalf("GET through proxy (CONNECT) failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello-from-backend" {
		t.Fatalf("body = %q, want %q", body, "hello-from-backend")
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestProxy_CONNECT_NonAllowlistedHostDeniedBeforeDial(t *testing.T) {
	cfg := policy.EgressConfig{
		Allowlist:              []policy.EgressAllowEntry{{Host: "api.anthropic.com"}},
		DefaultDailyByteBudget: 1 << 20,
	}
	gate := NewGate(cfg, NewSensitiveTracker(), newFakeBudget(), &fakeLedger{}, nil, nil)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	tracker := &dialTracker{}
	p := NewProxy(gate, port)
	p.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		tracker.called = true
		return nil, fmt.Errorf("dial should never be reached for a denied CONNECT")
	}
	if err := p.Start(); err != nil {
		t.Fatalf("Proxy.Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	conn, err := net.Dial("tcp", p.Addr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()

	if _, err := fmt.Fprintf(conn, "CONNECT not-allowlisted.example.com:443 HTTP/1.1\r\nHost: not-allowlisted.example.com:443\r\n\r\n"); err != nil {
		t.Fatalf("write CONNECT request: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "CONNECT"})
	if err != nil {
		t.Fatalf("read proxy response: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if tracker.called {
		t.Fatal("Dial was called for a denied CONNECT - the gate must deny BEFORE any upstream connection")
	}
}

func TestProxy_PlainHTTP_NonAllowlistedHostDenied(t *testing.T) {
	cfg := policy.EgressConfig{
		Allowlist:              []policy.EgressAllowEntry{{Host: "api.anthropic.com"}},
		DefaultDailyByteBudget: 1 << 20,
	}
	gate := NewGate(cfg, NewSensitiveTracker(), newFakeBudget(), &fakeLedger{}, nil, nil)
	proxyAddr := startProxy(t, gate)

	httpClient := &http.Client{
		Transport: &http.Transport{Proxy: proxyURL(proxyAddr)},
		Timeout:   5 * time.Second,
	}

	resp, err := httpClient.Get("http://not-allowlisted.example.com/")
	if err != nil {
		t.Fatalf("GET through proxy failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

// ---- BLOCKER D: DNS rebinding ----

// fakeResolver is Resolver's test double: a fixed host->addresses map, no
// real DNS lookup involved — this is exactly what lets the rebinding
// scenario (an allowlisted hostname's DNS answer turning out private) be
// reproduced deterministically, with no real DNS or /etc/hosts change.
type fakeResolver struct {
	ips map[string][]net.IPAddr
}

func (f *fakeResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	return f.ips[host], nil
}

// TestProxy_DNSRebinding_ResolvedPrivateIPDeniedAtDialTime is BLOCKER D's
// own regression test: an ALLOWLISTED hostname (Check itself says
// Allow=true for it) whose DNS answer resolves to a private/link-local/
// loopback address must be denied AT DIAL TIME, and Dial must never be
// called at all — mirroring TestProxy_CONNECT_NonAllowlistedHostDeniedBeforeDial's
// "denied before dial" shape, but for a DIFFERENT reason (a hostile DNS
// answer behind an otherwise-legitimate allowlist entry, not a plain
// allowlist miss).
func TestProxy_DNSRebinding_ResolvedPrivateIPDeniedAtDialTime(t *testing.T) {
	for _, rebindIP := range []string{"127.0.0.1", "192.168.1.1", "169.254.169.254"} {
		t.Run(rebindIP, func(t *testing.T) {
			cfg := policy.EgressConfig{
				Allowlist:              []policy.EgressAllowEntry{{Host: "rebind-test.example.com", Ports: []int{443}}},
				DefaultDailyByteBudget: 1 << 20,
			}
			ledger := &fakeLedger{}
			gate := NewGate(cfg, NewSensitiveTracker(), newFakeBudget(), ledger, nil, nil)

			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			port := ln.Addr().(*net.TCPAddr).Port
			ln.Close()

			tracker := &dialTracker{}
			p := NewProxy(gate, port)
			p.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
				tracker.called = true
				return nil, fmt.Errorf("dial should never be reached for a DNS-rebind-denied CONNECT")
			}
			p.Resolver = &fakeResolver{ips: map[string][]net.IPAddr{
				"rebind-test.example.com": {{IP: net.ParseIP(rebindIP)}},
			}}
			if err := p.Start(); err != nil {
				t.Fatalf("Proxy.Start: %v", err)
			}
			t.Cleanup(func() { _ = p.Close() })

			conn, err := net.Dial("tcp", p.Addr)
			if err != nil {
				t.Fatalf("dial proxy: %v", err)
			}
			defer conn.Close()

			if _, err := fmt.Fprintf(conn, "CONNECT rebind-test.example.com:443 HTTP/1.1\r\nHost: rebind-test.example.com:443\r\n\r\n"); err != nil {
				t.Fatalf("write CONNECT request: %v", err)
			}
			resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "CONNECT"})
			if err != nil {
				t.Fatalf("read proxy response: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (resolved address %s is private)", resp.StatusCode, rebindIP)
			}
			if tracker.called {
				t.Fatal("Dial was called for a DNS-rebind-denied CONNECT — the resolved address must be validated BEFORE ever dialing")
			}
			if ledger.count(EventBlockedDNSRebind) != 1 {
				t.Errorf("egress_blocked_dns_rebind ledger rows = %d, want 1", ledger.count(EventBlockedDNSRebind))
			}
		})
	}
}

// TestProxy_DNSRebinding_AllPublicAddressesStillSucceeds is the negative
// companion: an allowlisted hostname that resolves to an ordinary PUBLIC
// address must still work exactly as before — BLOCKER D's fix must never
// deny a legitimate resolution. Dial is overridden to redirect the
// (correctly PINNED, per the assertion below) resolved address to a real
// local httptest backend, so this stays a hermetic test with no real DNS
// or external network dependency.
func TestProxy_DNSRebinding_AllPublicAddressesStillSucceeds(t *testing.T) {
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello-from-backend"))
	}))
	defer backend.Close()
	backendAddr := backend.Listener.Addr().(*net.TCPAddr)

	cfg := policy.EgressConfig{
		Allowlist:              []policy.EgressAllowEntry{{Host: "public-test.example.com", Ports: []int{backendAddr.Port}}},
		DefaultDailyByteBudget: 1 << 20,
	}
	gate := NewGate(cfg, NewSensitiveTracker(), newFakeBudget(), &fakeLedger{}, nil, nil)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	p := NewProxy(gate, port)

	// A real public IP literal (example.com's) — genuinely NOT private, so
	// resolveAndPin's per-address validation passes. It is never actually
	// dialed for real: Dial below redirects to the local httptest backend
	// instead, so this test needs no real network access.
	const publicIP = "93.184.216.34"
	p.Resolver = &fakeResolver{ips: map[string][]net.IPAddr{
		"public-test.example.com": {{IP: net.ParseIP(publicIP)}},
	}}
	var dialedAddr string
	p.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialedAddr = addr
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, backendAddr.String())
	}
	if err := p.Start(); err != nil {
		t.Fatalf("Proxy.Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	httpClient := &http.Client{
		Transport: &http.Transport{
			Proxy:           proxyURL(p.Addr),
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test-only, self-signed httptest cert
		},
		Timeout: 5 * time.Second,
	}
	resp, err := httpClient.Get(fmt.Sprintf("https://public-test.example.com:%d/", backendAddr.Port))
	if err != nil {
		t.Fatalf("GET through proxy (CONNECT) failed: %v", err)
	}
	defer resp.Body.Close()

	wantDialed := fmt.Sprintf("%s:%d", publicIP, backendAddr.Port)
	if dialedAddr != wantDialed {
		t.Errorf("Dial called with addr = %q, want the PINNED resolved address %q (never re-resolved)", dialedAddr, wantDialed)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello-from-backend" {
		t.Fatalf("body = %q, want %q", body, "hello-from-backend")
	}
}

// ---- MINOR F: chunked request body metering ----

// TestProxy_PlainHTTP_ChunkedRequestBodyIsMetered is MINOR F's regression
// test: a request with NO Content-Length (chunked transfer encoding —
// ContentLength == -1) must still have its ACTUAL body size counted
// against the host's budget, not clamped to 0.
func TestProxy_PlainHTTP_ChunkedRequestBodyIsMetered(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()
	backendAddr := backend.Listener.Addr().(*net.TCPAddr)

	budget := newFakeBudget()
	cfg := policy.EgressConfig{
		Allowlist:              []policy.EgressAllowEntry{{Host: "127.0.0.1", Ports: []int{backendAddr.Port}}},
		DefaultDailyByteBudget: 1 << 20,
	}
	gate := NewGate(cfg, NewSensitiveTracker(), budget, &fakeLedger{}, nil, nil)
	proxyAddr := startProxy(t, gate)

	httpClient := &http.Client{
		Transport: &http.Transport{Proxy: proxyURL(proxyAddr)},
		Timeout:   5 * time.Second,
	}

	payload := strings.Repeat("x", 9000)
	req, err := http.NewRequest(http.MethodPost, backend.URL, io.NopCloser(strings.NewReader(payload)))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.ContentLength = -1 // force chunked transfer encoding: no upfront Content-Length at all

	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("POST (chunked) through proxy failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Fatalf("body = %q, want %q", body, "ok")
	}

	total, err := budget.Bytes(context.Background(), "127.0.0.1", time.Now().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("budget.Bytes: %v", err)
	}
	if total <= int64(len(payload)) {
		t.Fatalf("metered bytes = %d, want > %d — the chunked request body's own %d bytes must be counted, not clamped to 0", total, len(payload), len(payload))
	}
}

// ---- BLOCKER B/C: proxy-side session attribution via Proxy-Authorization ----

// TestProxy_TokenRegistry_AttributesSessionToTraceIDAndSensitiveBlockFires
// is the BLOCKER B/C regression test: a needs_network:true container's
// per-task Proxy-Authorization token attributes its connections to that
// task's own trace_id, so a secret-lane read marked under the SAME
// trace_id (mcp/fs's fs_read seam, simulated here directly against the
// SAME Gate) hard-blocks this container's later allowlist-external
// egress — proving the token->trace_id wiring the proxy consults really
// reaches the SensitiveTracker mcp/fs's seam marks, not a disconnected or
// always-empty one.
func TestProxy_TokenRegistry_AttributesSessionToTraceIDAndSensitiveBlockFires(t *testing.T) {
	cfg := policy.EgressConfig{
		Allowlist:              []policy.EgressAllowEntry{{Host: "api.anthropic.com"}},
		DefaultDailyByteBudget: 1 << 20,
	}
	sessions := NewSensitiveTracker()
	gate := NewGate(cfg, sessions, newFakeBudget(), &fakeLedger{}, nil, nil)
	tokens := NewProxySessionRegistry()

	traceID := "trace-container-attrib"
	token := "kahya-egt-test-token"
	tokens.Register(token, SessionInfo{SessionID: traceID, TraceID: traceID, TaskID: "task-1"})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	p := NewProxy(gate, port)
	p.Tokens = tokens
	if err := p.Start(); err != nil {
		t.Fatalf("Proxy.Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	// The exact URL shape mcp/shell/egress_network.go's egressProxyEnv
	// produces: "http://<token>:@host:port" — Go's http.Transport (like
	// curl/wget) sends that userinfo as Proxy-Authorization automatically.
	proxyU, err := url.Parse(fmt.Sprintf("http://%s:@127.0.0.1:%d", token, port))
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	httpClient := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyU)},
		Timeout:   5 * time.Second,
	}

	// Sanity: before any secret-lane mark, a non-allowlisted host is
	// denied for the ORDINARY allowlist reason — this container's token
	// alone grants nothing extra.
	resp, err := httpClient.Get("http://not-allowlisted.example.com/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if strings.Contains(string(body), "hassas") {
		t.Fatalf("expected the ORDINARY allowlist deny reason before any sensitive mark, got: %s", body)
	}

	// Mark the SAME trace_id sensitive — mirrors mcp/fs's fs_read seam
	// (BLOCKER A: taint is always keyed on the request's own trace_id).
	if err := gate.MarkSensitiveRead(context.Background(), traceID, traceID); err != nil {
		t.Fatalf("MarkSensitiveRead: %v", err)
	}

	// BLOCKER B/C: the SAME container (same token) now gets the SENSITIVE
	// deny reason, not merely the generic allowlist one — proving the
	// proxy really attributed this connection to trace_id.
	resp2, err := httpClient.Get("http://not-allowlisted.example.com/")
	if err != nil {
		t.Fatalf("GET (post-mark): %v", err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp2.StatusCode)
	}
	if !strings.Contains(string(body2), "hassas") {
		t.Fatalf("expected the SENSITIVE deny reason after marking this trace_id, got: %s", body2)
	}
}

// TestProxy_UnknownProxyTokenIsAnonymousNeverInheritsAnothersTaint proves
// the flip side: a request with NO token (or one this Proxy's registry
// does not recognize) is treated as an anonymous, untainted session — it
// must NEVER pick up some OTHER task's sensitive-read mark just because
// that other task happens to be tracked in the same registry.
func TestProxy_UnknownProxyTokenIsAnonymousNeverInheritsAnothersTaint(t *testing.T) {
	cfg := policy.EgressConfig{
		Allowlist:              []policy.EgressAllowEntry{{Host: "api.anthropic.com"}},
		DefaultDailyByteBudget: 1 << 20,
	}
	sessions := NewSensitiveTracker()
	gate := NewGate(cfg, sessions, newFakeBudget(), &fakeLedger{}, nil, nil)
	tokens := NewProxySessionRegistry()

	markedTrace := "trace-other-task-marked"
	tokens.Register("kahya-egt-other-task", SessionInfo{SessionID: markedTrace, TraceID: markedTrace})
	if err := gate.MarkSensitiveRead(context.Background(), markedTrace, markedTrace); err != nil {
		t.Fatalf("MarkSensitiveRead: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	p := NewProxy(gate, port)
	p.Tokens = tokens
	if err := p.Start(); err != nil {
		t.Fatalf("Proxy.Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	// No userinfo on this proxy URL at all — an unattributed request.
	httpClient := &http.Client{
		Transport: &http.Transport{Proxy: proxyURL(p.Addr)},
		Timeout:   5 * time.Second,
	}
	resp, err := httpClient.Get("http://not-allowlisted.example.com/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if strings.Contains(string(body), "hassas") {
		t.Fatalf("an unattributed (no-token) request must NEVER inherit another task's sensitive-read taint, got: %s", body)
	}
}

func TestProxy_PlainHTTP_AllowlistedHostSucceeds(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello-plain"))
	}))
	defer backend.Close()
	backendAddr := backend.Listener.Addr().(*net.TCPAddr)

	cfg := policy.EgressConfig{
		Allowlist:              []policy.EgressAllowEntry{{Host: "127.0.0.1", Ports: []int{backendAddr.Port}}},
		DefaultDailyByteBudget: 1 << 20,
	}
	gate := NewGate(cfg, NewSensitiveTracker(), newFakeBudget(), &fakeLedger{}, nil, nil)
	proxyAddr := startProxy(t, gate)

	httpClient := &http.Client{
		Transport: &http.Transport{Proxy: proxyURL(proxyAddr)},
		Timeout:   5 * time.Second,
	}
	resp, err := httpClient.Get(backend.URL)
	if err != nil {
		t.Fatalf("GET through proxy failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello-plain" {
		t.Fatalf("body = %q, want %q", body, "hello-plain")
	}
}

// ---- FINDING #8: per-connection egress budget cap ----

// TestProxy_CONNECT_TunnelSeveredWhenStreamingPastBudget is FINDING #8's
// regression test: a single CONNECT tunnel to an allowlisted host must not
// stream unbounded past that host's daily budget. With a small per-host
// budget and a response far larger than it, the tunnel must be SEVERED
// mid-stream (the client never receives the full payload) AND a
// egress_blocked_budget ledger row must be written — the running cap Check's
// nbytes=0 admission test alone cannot enforce for an opaque tunnel.
func TestProxy_CONNECT_TunnelSeveredWhenStreamingPastBudget(t *testing.T) {
	const payloadSize = 1 << 20 // 1 MiB — far larger than the budget below
	payload := strings.Repeat("k", payloadSize)
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, payload)
	}))
	defer backend.Close()
	backendAddr := backend.Listener.Addr().(*net.TCPAddr)

	ledger := &fakeLedger{}
	cfg := policy.EgressConfig{
		Allowlist:              []policy.EgressAllowEntry{{Host: "127.0.0.1", Ports: []int{backendAddr.Port}}},
		DefaultDailyByteBudget: 64 * 1024, // small enough that a 1 MiB body streams past it
	}
	gate := NewGate(cfg, NewSensitiveTracker(), newFakeBudget(), ledger, nil, nil)
	proxyAddr := startProxy(t, gate)

	httpClient := &http.Client{
		Transport: &http.Transport{
			Proxy:           proxyURL(proxyAddr),
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test-only, self-signed httptest cert
		},
		Timeout: 10 * time.Second,
	}

	resp, err := httpClient.Get(backend.URL)
	got := 0
	if err == nil {
		body, _ := io.ReadAll(resp.Body) // truncated read once the tunnel is severed
		resp.Body.Close()
		got = len(body)
	}
	// The tunnel must have been severed: the client either failed outright or
	// received strictly fewer bytes than the full payload.
	if err == nil && got >= payloadSize {
		t.Fatalf("received the full %d-byte payload; the tunnel was NOT severed at the budget cap", got)
	}

	// A budget denial must have been ledgered. DenyBudget runs after the
	// tunnel closes (post wg.Wait), which races the client observing the
	// close, so poll briefly.
	deadline := time.Now().Add(3 * time.Second)
	for ledger.count(EventBlockedBudget) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ledger.count(EventBlockedBudget) == 0 {
		t.Fatal("expected an egress_blocked_budget ledger row after the tunnel was severed at the budget cap")
	}
}

// TestProxy_PlainHTTP_RequestBodyCountedExactlyOnce is FINDING #13's
// regression test: a plain-HTTP POST with a known-size request body must
// raise the host's daily budget by EXACTLY (request-body + response-body)
// bytes — not (request-body*2 + response-body), which the old admission-time
// Check(nbytes=ContentLength) + post-hoc MeterUsage double-count produced.
func TestProxy_PlainHTTP_RequestBodyCountedExactlyOnce(t *testing.T) {
	responsePayload := strings.Repeat("r", 300)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, responsePayload)
	}))
	defer backend.Close()
	backendAddr := backend.Listener.Addr().(*net.TCPAddr)

	budget := newFakeBudget()
	cfg := policy.EgressConfig{
		Allowlist:              []policy.EgressAllowEntry{{Host: "127.0.0.1", Ports: []int{backendAddr.Port}}},
		DefaultDailyByteBudget: 1 << 20,
	}
	gate := NewGate(cfg, NewSensitiveTracker(), budget, &fakeLedger{}, nil, nil)
	proxyAddr := startProxy(t, gate)

	httpClient := &http.Client{
		Transport: &http.Transport{Proxy: proxyURL(proxyAddr)},
		Timeout:   5 * time.Second,
	}

	requestPayload := strings.Repeat("q", 500)
	// http.NewRequest with a *strings.Reader sets ContentLength=500 — a
	// non-chunked body with a known Content-Length, exactly the shape that
	// used to be double-counted.
	req, err := http.NewRequest(http.MethodPost, backend.URL, strings.NewReader(requestPayload))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("POST through proxy failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if string(body) != responsePayload {
		t.Fatalf("response body length = %d, want %d", len(body), len(responsePayload))
	}

	total, err := budget.Bytes(context.Background(), "127.0.0.1", time.Now().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("budget.Bytes: %v", err)
	}
	want := int64(len(requestPayload) + len(responsePayload))
	if total != want {
		t.Fatalf("metered budget = %d, want exactly %d (request-body %d + response-body %d, each counted once)",
			total, want, len(requestPayload), len(responsePayload))
	}
}
