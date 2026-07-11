// egress_integration_test.go: the W3-05 LIVE Docker network integration
// gate — this task's own §6 W3 acceptance item, verbatim: "container ici
// curl allowlist'i atlayamiyor (test)". Gated behind KAHYA_DOCKER_TESTS=1
// exactly like container_test.go (this file's own doc comment mirrors
// that file's contract: SKIPPED when the var is unset, FAILS — never
// skips — on any problem when it IS set, so a broken egress sandbox can
// never silently pass).
//
// mcp/shell cannot import kahyad/internal/egress directly (Go's
// internal-package import boundary: kahyad/internal/* is only importable
// from code rooted at kahya/kahyad/...; mcp/shell lives at kahya/mcp/shell
// — see mcp/fs's own package doc comment for the identical constraint
// against kahyad/internal/policy). The REAL egress.Gate's decision logic
// (allowlist normalization, budget accounting, sensitive-read block) is
// exhaustively unit/proxy-tested in kahyad/internal/egress's own test
// suite (gate_test.go, proxy_test.go, ALL pure-Go, no Docker involved).
// This file's job is narrower and Docker-SPECIFIC: prove the kahya-egress
// (--internal, no route out) network + kahya-egress-fwd sidecar topology
// actually CONTAINS a needs_network:true container's egress, so that no
// in-container curl can ever bypass whatever gate sits behind the
// sidecar — by direct IP, by --noproxy, or by DNS — regardless of which
// gate is running it. testEgressStandinProxy below is therefore a small,
// self-contained, SAME-SHAPE stand-in (CONNECT + plain-HTTP, gate-before-
// dial, exactly one allowed host) — NOT the real gate — used only to
// drive this Docker plumbing end to end, through the exact same
// EgressNetworkEnsurer + Runner.Run wiring production kahyad uses (only
// the listener on the OTHER end of the sidecar's dumb TCP forward
// differs).
package shell

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// liveEgressDigestPath resolves docker/egress/IMAGE_DIGEST — the exact
// same "two directories up from this test file's own location" pattern
// container_test.go's liveDigestPath already uses for docker/sandbox/
// IMAGE_DIGEST, independent of the working directory `go test` happens
// to run from.
func liveEgressDigestPath(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(wd, "..", "..", "docker", "egress", "IMAGE_DIGEST")
}

// testEgressStandinProxy is this file's minimal HTTP(S) forward-proxy
// stand-in (see this file's own doc comment for why it exists instead of
// the real kahyad/internal/egress.Proxy): it allows EXACTLY
// allowedHost:allowedPort (CONNECT) / allowedHost (plain HTTP), denying
// everything else with a 403 BEFORE ever dialing out — the same
// gate-before-dial shape the real proxy.go implements, independently
// reproduced here since this package cannot import that one.
func startTestEgressStandinProxy(t *testing.T, allowedHost string, allowedPort int) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			host, portStr, err := net.SplitHostPort(r.Host)
			if err != nil {
				host, portStr = r.Host, "443"
			}
			p, _ := strconv.Atoi(portStr)
			if host != allowedHost || p != allowedPort {
				http.Error(w, "egress denied: not allowlisted", http.StatusForbidden)
				return
			}
			upstream, err := net.DialTimeout("tcp", net.JoinHostPort(host, portStr), 10*time.Second)
			if err != nil {
				http.Error(w, "bad gateway", http.StatusBadGateway)
				return
			}
			defer upstream.Close()
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "hijack unsupported", http.StatusInternalServerError)
				return
			}
			client, _, err := hj.Hijack()
			if err != nil {
				return
			}
			defer client.Close()
			if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
				return
			}
			go func() { _, _ = io.Copy(upstream, client) }()
			_, _ = io.Copy(client, upstream)
			return
		}

		if !r.URL.IsAbs() || r.URL.Hostname() != allowedHost {
			http.Error(w, "egress denied: not allowlisted", http.StatusForbidden)
			return
		}
		outReq := r.Clone(r.Context())
		outReq.RequestURI = ""
		resp, err := http.DefaultTransport.RoundTrip(outReq)
		if err != nil {
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vv := range resp.Header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	})

	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return port
}

// TestDockerIntegration_EgressAllowlistCannotBeBypassed is this task's
// live §6 W3 gate: a needs_network:true shell_docker job, routed through
// the REAL kahya-egress Docker network + kahya-egress-fwd sidecar, cannot
// escape the allowlist behind it by ANY of: a direct IP connection
// bypassing the proxy (--noproxy), a DNS lookup of an external name, or
// simply asking the proxy for a non-allowlisted host.
func TestDockerIntegration_EgressAllowlistCannotBeBypassed(t *testing.T) {
	requireDockerTests(t)

	sandboxDigest, err := LoadPinnedDigest(liveDigestPath(t))
	if err != nil {
		t.Fatalf("LoadPinnedDigest (sandbox): %v", err)
	}
	if sandboxDigest == "" {
		t.Fatal("docker/sandbox/IMAGE_DIGEST has no pinned digest yet — run `make sandbox-image` first")
	}
	egressDigest, err := LoadPinnedDigest(liveEgressDigestPath(t))
	if err != nil {
		t.Fatalf("LoadPinnedDigest (egress sidecar): %v", err)
	}
	if egressDigest == "" {
		t.Fatal("docker/egress/IMAGE_DIGEST has no pinned digest yet")
	}

	// The stand-in gate allows EXACTLY example.com — nothing else (this
	// task's spec, verbatim: "curl https://<nonallowlisted> via proxy =>
	// 403").
	proxyPort := startTestEgressStandinProxy(t, "example.com", 443)

	r, _ := newLiveRunner(t)
	r.SetEgressEnsurer(NewEgressNetworkEnsurer(r.Exec, egressDigest), proxyPort)
	t.Cleanup(func() {
		// Best-effort teardown so repeated runs don't leave a sidecar
		// pointed at this test's now-closed stand-in proxy port lying
		// around (EgressNetworkEnsurer.Ensure would otherwise find an
		// already-"running" — but now-orphaned — container and skip
		// recreating it). context.Background(), NOT t.Context(): the
		// latter is already canceled by the time Cleanup funcs run, which
		// would make exec.CommandContext below a silent no-op.
		_, _ = r.Exec.Run(context.Background(), "docker", []string{"rm", "-f", EgressSidecarName}, nil)
	})
	workdir := liveWorkdir(t)

	script := `
FAIL=0

echo "=== step1: HTTPS to a non-allowlisted host via the proxy must be denied ==="
OUT1=$(curl -s -v --max-time 10 https://not-allowlisted.example.com/ 2>&1)
if echo "$OUT1" | grep -q "403"; then
  echo "STEP1_OK_403_DENIED"
else
  echo "STEP1_FAIL: $OUT1"
  FAIL=1
fi

echo "=== step2: direct IP bypass (--noproxy) must be unreachable — kahya-egress has no route out ==="
if curl -s --max-time 5 --noproxy '*' https://1.1.1.1 >/dev/null 2>&1; then
  echo "STEP2_FAIL_DIRECT_IP_REACHABLE"
  FAIL=1
else
  echo "STEP2_OK_DIRECT_IP_UNREACHABLE"
fi

echo "=== step3: DNS lookup of an external name must fail — kahya-egress has no DNS route ==="
if getent hosts example.com >/dev/null 2>&1; then
  echo "STEP3_FAIL_DNS_RESOLVED"
  FAIL=1
else
  echo "STEP3_OK_DNS_UNRESOLVED"
fi

echo "=== step4: an allowlisted host via the proxy must still succeed ==="
CODE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 http://example.com/)
if [ "$CODE" = "200" ] || [ "$CODE" = "301" ] || [ "$CODE" = "308" ]; then
  echo "STEP4_OK_ALLOWLISTED_$CODE"
else
  echo "STEP4_FAIL_CODE_$CODE"
  FAIL=1
fi

exit $FAIL
`
	out, err := r.Run(t.Context(), "trace-egress-integration", "task-egress-integration", RunInput{
		Script: script, Workdir: workdir, TimeoutS: 30, NeedsNetwork: true,
	})
	if err != nil {
		t.Fatalf("shell_docker run failed: %v\nstdout=%s\nstderr=%s", err, out.Stdout, out.Stderr)
	}
	t.Logf("integration script stdout:\n%s", out.Stdout)
	if out.ExitCode != 0 {
		t.Fatalf("script exited %d (want 0 — every containment check must pass)\nstdout=%s\nstderr=%s",
			out.ExitCode, out.Stdout, out.Stderr)
	}
	for _, want := range []string{
		"STEP1_OK_403_DENIED",
		"STEP2_OK_DIRECT_IP_UNREACHABLE",
		"STEP3_OK_DNS_UNRESOLVED",
		"STEP4_OK_ALLOWLISTED_",
	} {
		if !strings.Contains(out.Stdout, want) {
			t.Errorf("expected stdout to contain %q, got:\n%s", want, out.Stdout)
		}
	}
}
