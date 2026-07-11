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
	"strconv"
	"strings"
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

// reasonEgressNetworkFailed is the EXACT Turkish string this task's spec
// fixes verbatim: "Egress ağı kurulamadı" — every failure path in this
// file (and Runner.Run's needs_network handling) wraps its error with
// this prefix, so a grep for this literal string always finds every
// egress-network failure, unit test or live daemon alike.
const reasonEgressNetworkFailed = "Egress ağı kurulamadı"

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
// This is kahyad-INJECTED, never model-controlled — see
// runner.go's Run (spec.ProxyEnvPairs) and redactDockerArgv's
// nonSecretEnvNames (env_allowlist.go), which deliberately does NOT
// redact these values in the JSONL/ledger transcript (they are not
// secret — a fixed, publicly-documented sidecar address).
func egressProxyEnv() map[string]string {
	proxyURL := fmt.Sprintf("http://%s:%d", EgressSidecarName, EgressSidecarListenPort)
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
