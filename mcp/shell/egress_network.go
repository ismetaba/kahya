// egress_network.go implements the W3-05 Docker network glue HANDOFF §5
// safety #1's ⚑ container-egress bullet requires: "agin gerektiren isler
// yalniz kahyad'in egress proxy'si ... uzerinden cikar - aksi halde
// container ici curl allowlist'i atlar." Concretely:
//
//   - `docker network create --internal kahya-egress` — an internal
//     Docker network with NO gateway/route out at all.
//   - a `kahya-egress-fwd` sidecar container (a pinned `alpine/socat`
//     digest — docker/egress/IMAGE_DIGEST, loaded via this package's own
//     LoadPinnedDigest, the same helper docker/sandbox/IMAGE_DIGEST
//     uses), attached to BOTH kahya-egress and the default bridge
//     network, dumbly TCP-forwarding kahya-egress-fwd:3128 to
//     host.docker.internal:<kahyad's own egress.port> — the ONLY route
//     out of kahya-egress.
//
// The sidecar is a single `socat TCP-LISTEN:...,fork TCP:...` invocation —
// DUMB TCP forwarding, nothing else. ALL policy (allowlist, budget,
// sensitive-read block) lives in kahyad Go code
// (kahyad/internal/egress.Gate), never here — the sidecar cannot even see
// HTTP, let alone make a decision about it.
package shell

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// Docker network/sidecar names and ports (this task's spec, verbatim) —
// fixed, not a runtime config knob (the same "boring, narrow" posture
// env_allowlist.go/hostexec.go already use for their own closed sets).
const (
	EgressNetworkName = "kahya-egress"
	EgressSidecarName = "kahya-egress-fwd"
	// EgressSidecarListenPort is the port the sidecar listens on WITHIN
	// kahya-egress — container jobs set HTTP_PROXY/HTTPS_PROXY to
	// "http://kahya-egress-fwd:<EgressSidecarListenPort>" (this task's
	// spec, verbatim: "HTTP_PROXY=http://kahya-egress-fwd:3128").
	EgressSidecarListenPort = 3128

	egressSidecarBaseImage = "alpine/socat"
)

// dockerSharedResourceLockPath is a fixed, well-known path used to
// serialize every test process (in this package OR any other, e.g.
// tests/w3's W3-10 Gate 4) that creates/uses/removes the kahya-egress-fwd
// sidecar / kahya-egress network by their fixed, well-known,
// process-wide-singleton names above. Those names are deliberately fixed
// (a single system-wide sidecar, matching the real daemon's own posture),
// which is exactly why two DIFFERENT test binaries touching them at the
// same time (Go's `go test ./...` runs different packages concurrently by
// default) race each other's create/recreate/remove calls on the SAME
// Docker-global resource — confirmed empirically once a second package
// (tests/w3) started exercising this same egress-network path.
const dockerSharedResourceLockPath = "/tmp/kahya-egress-docker-shared-test.lock"

// LockDockerSharedResources acquires an exclusive, cross-process flock on
// dockerSharedResourceLockPath and returns a release func (call via
// t.Cleanup). Every test — in this package or another — that drives a
// needs_network:true shell_docker job by the fixed EgressNetworkName/
// EgressSidecarName must hold this for the whole test.
func LockDockerSharedResources() (release func(), err error) {
	f, err := os.OpenFile(dockerSharedResourceLockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("shell: open docker shared-resource lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("shell: flock docker shared-resource lock: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// reasonEgressNetworkFailed is the EXACT Turkish string this task's spec
// fixes verbatim: "Egress ağı kurulamadı" — every failure path in this
// file (and Runner.Run's needs_network handling) wraps its error with
// this prefix, so a grep for this literal string always finds every
// egress-network failure, unit test or live daemon alike.
const reasonEgressNetworkFailed = "Egress ağı kurulamadı"

// EgressTokenRegistrar is the W3-05 BLOCKER B/C seam Runner.Run calls to
// bind a needs_network:true container's own per-task egress-proxy
// credential (embedded in its HTTP_PROXY/HTTPS_PROXY as Basic-auth
// userinfo — see egressProxyEnv below) to this task's trace_id, so
// kahyad's forward-proxy (kahyad/internal/egress.Proxy, the ONLY route
// out of kahya-egress) can attribute that container's traffic to the SAME
// SensitiveTracker/session key every other in-process caller (mcp/fs's
// fs_read seam, the anthproxy egress-gate factory) keys on: trace_id.
// mcp/shell cannot import kahyad/internal/egress directly (Go's
// internal-package import boundary — see mcp/fs's package doc comment for
// the identical constraint), so kahyad's own wiring
// (kahyad/internal/server.NewEgressTokenRegistrar) satisfies this with a
// thin adapter straight onto egress.ProxySessionRegistry, exactly like
// mcp/fs.SensitiveReadMarker's "in-process today" seam.
//
// Register is called once, right before the container starts
// (Runner.Run); Release is called once that Run call's docker run has
// exited (defer) — a token's validity window is exactly this ONE
// container's own lifetime (Run blocks until the container exits, so
// nothing else can ever present this token after Release runs).
type EgressTokenRegistrar interface {
	Register(token, traceID, taskID string)
	Release(token string)
}

// EgressNetworkEnsurer idempotently sets up the kahya-egress Docker
// network + kahya-egress-fwd sidecar. Runner.Run calls Ensure before
// EVERY needs_network:true invocation (mirrors Runner's own "re-check
// docker health on every call, not just once at boot" posture) — cheap
// once the network/sidecar already exist (a handful of `docker network
// inspect`/`docker ps` reads), and self-healing if the sidecar was ever
// removed out-of-band.
type EgressNetworkEnsurer struct {
	Exec Executor
	// PinnedDigest is docker/egress/IMAGE_DIGEST's committed content (via
	// LoadPinnedDigest) — an empty value (not yet pinned) is Ensure's own
	// fail-closed state, exactly like Runner.PinnedDigest for the sandbox
	// image.
	PinnedDigest string
}

// NewEgressNetworkEnsurer constructs an EgressNetworkEnsurer. exec is
// typically the SAME Executor Runner itself uses (kahyad's wiring passes
// r.Exec straight through — no reason for a second implementation).
func NewEgressNetworkEnsurer(exec Executor, pinnedDigest string) *EgressNetworkEnsurer {
	return &EgressNetworkEnsurer{Exec: exec, PinnedDigest: pinnedDigest}
}

// imageRef is the fully pinned image reference `docker run`/`docker
// exec` use: "alpine/socat@sha256:...".
func (e *EgressNetworkEnsurer) imageRef() string {
	return egressSidecarBaseImage + "@" + e.PinnedDigest
}

// Ensure creates the kahya-egress internal network (if absent), starts/
// attaches the kahya-egress-fwd sidecar (if not already running),
// forwarding to host.docker.internal:hostEgressPort, and asserts
// host.docker.internal is actually reachable from inside it (colima's
// virtiofs/network setup, or a misconfigured Docker Desktop network, can
// otherwise leave a sidecar that starts cleanly but can never actually
// reach kahyad — this must fail LOUD, not silently degrade to "container
// egress times out"). Every failure wraps reasonEgressNetworkFailed.
func (e *EgressNetworkEnsurer) Ensure(ctx context.Context, hostEgressPort int) error {
	if strings.TrimSpace(e.PinnedDigest) == "" {
		return fmt.Errorf("%s: kahya-egress-fwd imaj özeti sabitlenmemiş (docker/egress/IMAGE_DIGEST)", reasonEgressNetworkFailed)
	}
	if err := e.ensureNetwork(ctx); err != nil {
		return fmt.Errorf("%s: %w", reasonEgressNetworkFailed, err)
	}
	// BLOCKER E fix — see ensureGatewayIsolation's own doc comment. Runs
	// right after the network exists (whether just-created or
	// pre-existing) and BEFORE the sidecar starts, so no needs_network:true
	// container can ever run against a network whose gateway pivot has not
	// yet been closed.
	if err := e.ensureGatewayIsolation(ctx, EgressNetworkName); err != nil {
		return fmt.Errorf("%s: %w", reasonEgressNetworkFailed, err)
	}
	if err := e.ensureSidecar(ctx, hostEgressPort); err != nil {
		return fmt.Errorf("%s: %w", reasonEgressNetworkFailed, err)
	}
	if err := e.verifyHostDockerInternalReachable(ctx); err != nil {
		return fmt.Errorf("%s: %w", reasonEgressNetworkFailed, err)
	}
	return nil
}

func (e *EgressNetworkEnsurer) networkExists(ctx context.Context) (bool, error) {
	res, err := e.Exec.Run(ctx, "docker", []string{"network", "inspect", EgressNetworkName}, nil)
	if err != nil {
		return false, err
	}
	return res.ExitCode == 0, nil
}

func (e *EgressNetworkEnsurer) ensureNetwork(ctx context.Context) error {
	exists, err := e.networkExists(ctx)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	res, err := e.Exec.Run(ctx, "docker", []string{"network", "create", "--internal", EgressNetworkName}, nil)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("docker network create %s: %s", EgressNetworkName, strings.TrimSpace(string(res.Stderr)))
	}
	return nil
}

// ensureGatewayIsolation is BLOCKER E's fix: kahya-egress is created
// `--internal` (no default route out at all), but its GATEWAY IP (e.g.
// 172.18.0.1) is still ON-LINK — the same L2/bridge segment every
// container attached to the network shares — so a container can dial the
// gateway IP DIRECTLY (ARP + connect; on-link delivery needs no route at
// all) and reach whatever the COLIMA VM'S OWN network stack has bound on
// that address (verified live against this repo's own dev colima VM:
// `nc -zv 172.18.0.1 22` from a plain container on kahya-egress reached
// the VM's sshd) — a pivot INTO the VM that bypasses this entire egress
// design, `--internal` notwithstanding.
//
// The fix installs an iptables DROP rule for (source=kahya-egress's own
// subnet, destination=its gateway IP) — but in the colima VM's INPUT
// chain, NOT its FORWARD/DOCKER-USER chain (also verified live): traffic
// destined to the gateway's own address is LOCALLY delivered to the VM's
// network stack (it is that stack's own address), so it is filtered by
// INPUT, never FORWARDed at all — a DOCKER-USER rule (the chain Docker
// itself recommends for custom container-network firewalling) has NO
// effect on this specific traffic, since it only ever sees traffic being
// FORWARDED through the host, not traffic terminating ON it. This is
// exactly why the finding exists: the "obvious" place to add a container
// firewall rule does not cover this path at all.
//
// Nothing legitimate in this design ever needs to reach the gateway IP
// itself: container-to-container traffic on kahya-egress (a
// needs_network:true sandbox reaching kahya-egress-fwd) is a direct
// container-IP-to-container-IP L2 hop, never routed via the gateway; the
// sidecar's own route to host.docker.internal goes over the SEPARATE
// "bridge" network it is also attached to (ensureSidecar). So this rule
// blocks ALL traffic from the subnet to the gateway, unconditionally — no
// exception is needed for the sidecar or for any container this package
// ever spawns.
//
// Idempotent (checked via `-C` before `-I`) so this runs safely before
// EVERY needs_network:true invocation, mirroring ensureSidecar's own
// "cheap once already set up, self-healing" posture.
//
// RESIDUAL RISK (documented per this task's own instruction, not silently
// left): this rule lives in the colima VM's OWN iptables state. A plain
// VM reboot or dockerd restart preserves it (Linux's INPUT chain is not
// Docker-managed and is not flushed by a `docker`/`dockerd` restart), but
// a full `colima delete && colima start` VM recreation would lose it —
// Ensure() re-applies it before every needs_network:true run specifically
// so a missing rule self-heals before any container gets a chance to use
// the (at that moment unprotected) network. This is a WITHIN-COLIMA-VM
// mitigation only, scoped exactly to the gateway-IP pivot this finding
// names: off-box egress is already independently blocked and verified
// (the allowlist/budget gate, the --internal network's lack of any route
// out, DNS resolution unavailable inside the network); this fix does not
// and cannot (from inside the VM's own iptables) protect against a
// hypothetical escape from the colima VM to the macOS host itself — that
// is colima's own VM security boundary, out of this package's scope. A
// non-colima Docker backend (e.g. Docker Desktop) has no `colima ssh`
// target at all, so this step fails closed (Ensure returns an error,
// needs_network:true is refused) rather than silently skipping the
// mitigation — matching this file's own established "fail LOUD, never
// silently degrade" posture (see verifyHostDockerInternalReachable).
func (e *EgressNetworkEnsurer) ensureGatewayIsolation(ctx context.Context, network string) error {
	subnet, gateway, err := e.networkSubnetGateway(ctx, network)
	if err != nil {
		return fmt.Errorf("kahya-egress alt ağı/gateway bilgisi okunamadı: %w", err)
	}
	exists, err := e.gatewayDropRuleExists(ctx, subnet, gateway)
	if err != nil {
		return fmt.Errorf("gateway izolasyon kuralı kontrol edilemedi: %w", err)
	}
	if exists {
		return nil
	}
	res, err := e.Exec.Run(ctx, "colima", []string{
		"ssh", "--", "sudo", "iptables", "-I", "INPUT", "-s", subnet, "-d", gateway, "-j", "DROP",
	}, nil)
	if err != nil {
		return fmt.Errorf("colima ssh iptables -I basarisiz: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("iptables -I INPUT basarisiz (exit %d): %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	return nil
}

// networkSubnetGateway reads network's assigned subnet (CIDR) and gateway
// IP via `docker network inspect --format`, so ensureGatewayIsolation's
// iptables rule always targets the ACTUAL assigned values (Docker
// auto-assigns kahya-egress's subnet — nothing in this package pins it),
// never a hardcoded guess.
func (e *EgressNetworkEnsurer) networkSubnetGateway(ctx context.Context, network string) (subnet, gateway string, err error) {
	res, err := e.Exec.Run(ctx, "docker", []string{
		"network", "inspect", network,
		"--format", "{{(index .IPAM.Config 0).Subnet}} {{(index .IPAM.Config 0).Gateway}}",
	}, nil)
	if err != nil {
		return "", "", err
	}
	if res.ExitCode != 0 {
		return "", "", fmt.Errorf("docker network inspect %s: %s", network, strings.TrimSpace(string(res.Stderr)))
	}
	fields := strings.Fields(strings.TrimSpace(string(res.Stdout)))
	if len(fields) != 2 {
		return "", "", fmt.Errorf("beklenmeyen docker network inspect çıktısı: %q", string(res.Stdout))
	}
	return fields[0], fields[1], nil
}

// gatewayDropRuleExists reports whether the INPUT-chain DROP rule
// ensureGatewayIsolation installs is already present, via `iptables -C`
// (exit 0 = present, nonzero = absent) — the idempotency check that lets
// Ensure() call this on every needs_network:true invocation without
// accumulating duplicate rules.
func (e *EgressNetworkEnsurer) gatewayDropRuleExists(ctx context.Context, subnet, gateway string) (bool, error) {
	res, err := e.Exec.Run(ctx, "colima", []string{
		"ssh", "--", "sudo", "iptables", "-C", "INPUT", "-s", subnet, "-d", gateway, "-j", "DROP",
	}, nil)
	if err != nil {
		return false, err
	}
	return res.ExitCode == 0, nil
}

func (e *EgressNetworkEnsurer) sidecarRunning(ctx context.Context) (bool, error) {
	res, err := e.Exec.Run(ctx, "docker", []string{
		"ps", "-q", "--filter", "name=^/" + EgressSidecarName + "$", "--filter", "status=running",
	}, nil)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(res.Stdout)) != "", nil
}

func (e *EgressNetworkEnsurer) ensureSidecar(ctx context.Context, hostEgressPort int) error {
	running, err := e.sidecarRunning(ctx)
	if err != nil {
		return err
	}
	if running {
		return nil
	}

	// Best-effort: clear any stopped/stale container occupying the name
	// (the only failure mode the `docker run` below would otherwise hit).
	_, _ = e.Exec.Run(ctx, "docker", []string{"rm", "-f", EgressSidecarName}, nil)

	target := "host.docker.internal:" + strconv.Itoa(hostEgressPort)
	args := []string{
		"run", "-d", "--name", EgressSidecarName,
		"--network", EgressNetworkName,
		"--restart", "unless-stopped",
		e.imageRef(),
		"TCP-LISTEN:" + strconv.Itoa(EgressSidecarListenPort) + ",fork,reuseaddr",
		"TCP:" + target,
	}
	res, err := e.Exec.Run(ctx, "docker", args, nil)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("docker run %s: %s", EgressSidecarName, strings.TrimSpace(string(res.Stderr)))
	}

	// `docker run` attaches only ONE network at creation time — connect
	// the default bridge too, the sidecar's OWN route to
	// host.docker.internal (kahya-egress is --internal and has no
	// gateway at all).
	res, err = e.Exec.Run(ctx, "docker", []string{"network", "connect", "bridge", EgressSidecarName}, nil)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("docker network connect bridge %s: %s", EgressSidecarName, strings.TrimSpace(string(res.Stderr)))
	}
	return nil
}

// egressProxyEnv is the fixed set of proxy env vars a needs_network:true
// container gets (this task's spec, verbatim: "HTTP_PROXY=http://
// kahya-egress-fwd:3128 (and HTTPS_PROXY/NO_PROXY)") — both the upper-
// and lower-case spellings are set since tools vary in which one they
// honor (curl/wget prefer lowercase; many others only check uppercase).
// This is kahyad-INJECTED, never model-controlled.
//
// BLOCKER B/C fix: token, when non-empty (Runner.Run mints one whenever
// an EgressTokenRegistrar is wired), is embedded as the proxy URL's
// Basic-auth USERINFO — "http://<token>:@kahya-egress-fwd:3128" — the
// standard convention curl/wget/most HTTP clients already implement for
// ANY HTTP_PROXY URL with userinfo: they send it as a Proxy-Authorization:
// Basic header automatically, on every request through that proxy
// (CONNECT and plain HTTP alike), with ZERO extra configuration needed
// inside the container. This is the ONLY channel a needs_network:true
// container has to identify itself to kahyad's forward-proxy at all — the
// dumb kahya-egress-fwd sidecar (a single `socat` TCP forward) carries it
// through completely unmodified, byte for byte, exactly like every other
// byte it forwards. kahyad/internal/egress/proxy.go's
// Proxy.sessionForRequest reads this same credential back out of the
// inbound request's Proxy-Authorization header and maps it back to this
// task's own trace_id via the SAME ProxySessionRegistry Runner.Run
// registered it into (EgressTokenRegistrar). An empty token (no
// registrar wired) falls back to the original credential-free proxy URL —
// still allowlist+budget-gated (Gate.Check's ordinary empty-SessionID
// path), just never sensitive-read-tainted; see EgressTokenRegistrar's
// own doc comment.
//
// redactDockerArgv (env_allowlist.go) partially redacts this value in the
// shell_docker_run transcript — the userinfo/token portion, never the
// scheme/host/port — since the token IS a credential now, even though the
// sidecar address itself remains fine to show in cleartext.
func egressProxyEnv(token string) map[string]string {
	proxyURL := fmt.Sprintf("http://%s:%d", EgressSidecarName, EgressSidecarListenPort)
	if token != "" {
		proxyURL = fmt.Sprintf("http://%s:@%s:%d", token, EgressSidecarName, EgressSidecarListenPort)
	}
	noProxy := "localhost,127.0.0.1"
	return map[string]string{
		"HTTP_PROXY":  proxyURL,
		"HTTPS_PROXY": proxyURL,
		"NO_PROXY":    noProxy,
		"http_proxy":  proxyURL,
		"https_proxy": proxyURL,
		"no_proxy":    noProxy,
	}
}

// verifyHostDockerInternalReachable asserts host.docker.internal
// resolves from INSIDE the already-running sidecar (this task's own
// gotcha: colima needs its host.docker.internal mapping verified,
// asserted in the integration setup, failing with reasonEgressNetworkFailed
// otherwise).
func (e *EgressNetworkEnsurer) verifyHostDockerInternalReachable(ctx context.Context) error {
	res, err := e.Exec.Run(ctx, "docker", []string{"exec", EgressSidecarName, "getent", "hosts", "host.docker.internal"}, nil)
	if err != nil || res.ExitCode != 0 {
		return fmt.Errorf("host.docker.internal çözülemedi (colima/Docker ağ eşleme sorunu)")
	}
	return nil
}
