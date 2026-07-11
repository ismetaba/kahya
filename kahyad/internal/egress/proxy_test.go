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
