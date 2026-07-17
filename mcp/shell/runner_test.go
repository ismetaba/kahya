package shell

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestRunner(home string, denyGlobs []string, pc *fakePolicyClient, ledger *fakeLedger, log *fakeLogger, exec *fakeExecutor, digest *fakeDigestChecker, health *fakeHealthChecker, pinnedDigest string) *Runner {
	r := &Runner{
		Home: home, DenyGlobs: denyGlobs, ImageTag: "kahya-sandbox:test", PinnedDigest: pinnedDigest,
		Policy: pc, Ledger: ledger, Log: log,
		Exec: exec, DigestChecker: digest, Health: health,
		EnvLookup: func(string) (string, bool) { return "", false },
	}
	r.SetTimeoutUnit(time.Millisecond) // every test runs fast; no real 300s wait anywhere
	return r
}

// baseFixture returns t.TempDir(), resolved through filepath.EvalSymlinks
// (mirrors mcp/fs/paths_test.go's identical testHome helper): on macOS,
// t.TempDir()'s own path lives under /var/folders, itself a symlink to
// /private/var/folders — Canonicalize resolves that same symlink
// internally, so comparing an un-resolved t.TempDir() value against a
// deny-glob built from it would spuriously mismatch on this platform.
func baseFixture(t *testing.T) (home, workdir string) {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(t.TempDir()): %v", err)
	}
	home = resolved
	workdir = filepath.Join(home, "task-workdir")
	if err := os.MkdirAll(workdir, 0o700); err != nil {
		t.Fatal(err)
	}
	return home, workdir
}

// ---- buildDockerRunArgs: pure function, no docker daemon involved. ----

func TestBuildDockerRunArgs_NonNegotiableFlags(t *testing.T) {
	args := buildDockerRunArgs(dockerRunSpec{
		ImageTag: "kahya-sandbox:0.1.0", ImageRef: "sha256:verifiedid", ContainerName: "kahya-sandbox-abc123", TaskID: "task-1",
		Workdir: "/canonical/workdir",
	})
	joined := " " + strings.Join(args, " ") + " "

	for _, want := range []string{
		" --network none ",
		" --read-only ",
		" --tmpfs /tmp ",
		" -v /canonical/workdir:/work:rw ",
		" --user 1000:1000 ",
		" --pids-limit 256 ",
		" --memory 2g ",
		" --cap-drop ALL ",
		" --security-opt no-new-privileges ",
		" --label kahya.task_id=task-1 ",
		" --name kahya-sandbox-abc123 ",
		" --rm ",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("docker run args missing %q\nfull args: %v", want, args)
		}
	}
	// FINDING #14: docker run targets the digest-pin-VERIFIED image ID
	// (ImageRef), never the mutable tag (ImageTag) — see dockerRunSpec.ImageRef.
	if !strings.Contains(joined, " sha256:verifiedid /bin/sh") {
		t.Errorf("docker run args must end with the resolved image ID + /bin/sh, got: %v", args)
	}
	if strings.Contains(joined, " kahya-sandbox:0.1.0 /bin/sh") {
		t.Errorf("docker run must NOT target the mutable tag as its positional image arg: %v", args)
	}
	// The Docker socket must NEVER be mounted (this task's own gotcha).
	if strings.Contains(joined, "docker.sock") {
		t.Fatalf("docker run args must never mount the docker socket: %v", args)
	}
}

func TestBuildDockerRunArgs_EnvAllowlistSorted(t *testing.T) {
	args := buildDockerRunArgs(dockerRunSpec{
		ImageTag: "kahya-sandbox:0.1.0", ContainerName: "c", TaskID: "t",
		Workdir: "/w", EnvPairs: map[string]string{"ZEBRA": "z", "ALPHA": "a"},
	})
	joined := strings.Join(args, " ")
	// deterministic (sorted) order: ALPHA before ZEBRA.
	if strings.Index(joined, "ALPHA=a") > strings.Index(joined, "ZEBRA=z") {
		t.Fatalf("expected ALPHA before ZEBRA in sorted env args, got: %v", args)
	}
	if !strings.Contains(joined, "-e ALPHA=a") || !strings.Contains(joined, "-e ZEBRA=z") {
		t.Fatalf("expected -e NAME=VALUE pairs, got: %v", args)
	}
}

// ---- Runner.Run: mechanical pre-policy refusals (never reach the executor). ----

func TestRun_EmptyScriptRejected(t *testing.T) {
	home, workdir := baseFixture(t)
	exec := &fakeExecutor{}
	r := newTestRunner(home, nil, &fakePolicyClient{decision: allowDecision("tok")}, &fakeLedger{}, newFakeLogger(), exec,
		&fakeDigestChecker{digest: "sha256:same"}, &fakeHealthChecker{healthy: true}, "sha256:same")

	_, err := r.Run(context.Background(), "trace-1", "task-1", RunInput{Script: "   ", Workdir: workdir})
	if err == nil {
		t.Fatal("expected error for empty script")
	}
	if exec.totalCalls() != 0 {
		t.Fatalf("empty script must never reach the executor, got %d calls", exec.totalCalls())
	}
}

func TestRun_WorkdirDenyGlobRejectedBeforeExecutor(t *testing.T) {
	home, workdir := baseFixture(t)
	exec := &fakeExecutor{}
	ledger := &fakeLedger{}
	pc := &fakePolicyClient{decision: allowDecision("tok")}
	r := newTestRunner(home, []string{filepath.Join(home, "task-workdir") + "/**", filepath.Join(home, "task-workdir")}, pc, ledger, newFakeLogger(), exec,
		&fakeDigestChecker{digest: "sha256:same"}, &fakeHealthChecker{healthy: true}, "sha256:same")

	_, err := r.Run(context.Background(), "trace-1", "task-1", RunInput{Script: "echo hi", Workdir: workdir})
	if err == nil {
		t.Fatal("expected deny-glob rejection")
	}
	if exec.totalCalls() != 0 {
		t.Fatalf("deny-glob hit must never reach the executor, got %d calls", exec.totalCalls())
	}
	if pc.checkCalls != 0 {
		t.Fatalf("deny-glob hit must never even reach Policy.Check, got %d calls", pc.checkCalls)
	}
	if len(ledger.find("shell_deny_glob_hit")) != 1 {
		t.Fatalf("expected exactly one shell_deny_glob_hit ledger event, got %d", len(ledger.find("shell_deny_glob_hit")))
	}
}

// TestRun_NeedsNetworkRejectedWithoutEgressEnsurer proves needs_network:
// true still fails CLOSED (never reaches the executor/policy gate) when
// this Runner has no EgressEnsurer wired at all — the fail-closed floor
// for a misconfigured/test build (kahyad's real wiring, main.go, always
// sets one).
func TestRun_NeedsNetworkRejectedWithoutEgressEnsurer(t *testing.T) {
	home, workdir := baseFixture(t)
	exec := &fakeExecutor{}
	ledger := &fakeLedger{}
	pc := &fakePolicyClient{decision: allowDecision("tok")}
	r := newTestRunner(home, nil, pc, ledger, newFakeLogger(), exec,
		&fakeDigestChecker{digest: "sha256:same"}, &fakeHealthChecker{healthy: true}, "sha256:same")

	_, err := r.Run(context.Background(), "trace-1", "task-1", RunInput{Script: "echo hi", Workdir: workdir, NeedsNetwork: true})
	if err == nil {
		t.Fatal("expected needs_network rejection")
	}
	if !strings.Contains(err.Error(), reasonEgressNetworkFailed) {
		t.Errorf("error = %q, want it to contain %q", err.Error(), reasonEgressNetworkFailed)
	}
	if exec.totalCalls() != 0 {
		t.Fatalf("needs_network:true with no EgressEnsurer must never reach the executor, got %d calls", exec.totalCalls())
	}
	if pc.checkCalls != 0 {
		t.Fatalf("needs_network:true with no EgressEnsurer must never even reach Policy.Check, got %d calls", pc.checkCalls)
	}
	if len(ledger.find("shell_egress_network_failed")) != 1 {
		t.Fatalf("expected shell_egress_network_failed ledger event")
	}
}

// TestRun_NeedsNetworkAttachesKahyaEgressNetwork proves the HAPPY path:
// once an EgressEnsurer is wired (and Ensure succeeds — the fakeExecutor's
// default zero-value Result{} for every unrecognized docker subcommand IS
// success, ExitCode 0), a needs_network:true run attaches the container
// to kahya-egress (never --network none) and sets HTTP_PROXY/HTTPS_PROXY/
// NO_PROXY pointed at the kahya-egress-fwd sidecar (this task's spec,
// verbatim).
func TestRun_NeedsNetworkAttachesKahyaEgressNetwork(t *testing.T) {
	home, workdir := baseFixture(t)
	exec := &fakeExecutor{}
	ledger := &fakeLedger{}
	log := newFakeLogger()
	pc := &fakePolicyClient{decision: allowDecision("tok")}
	r := newTestRunner(home, nil, pc, ledger, log, exec,
		&fakeDigestChecker{digest: "sha256:same"}, &fakeHealthChecker{healthy: true}, "sha256:same")
	r.SetEgressEnsurer(NewEgressNetworkEnsurer(exec, "sha256:pinned-test-digest"), 3128)

	_, err := r.Run(context.Background(), "trace-1", "task-1", RunInput{Script: "echo hi", Workdir: workdir, NeedsNetwork: true})
	if err != nil {
		t.Fatalf("Run() error = %v, want success", err)
	}

	runCalls := exec.callsFor("run")
	if len(runCalls) == 0 {
		t.Fatal("expected at least one docker run call")
	}
	// The MAIN sandbox container's docker run is always the LAST "run"
	// call (the sidecar's own "docker run -d --name kahya-egress-fwd..."
	// call, if any, happens first, inside EgressEnsurer.Ensure).
	mainRun := runCalls[len(runCalls)-1]
	joined := " " + strings.Join(mainRun.args, " ") + " "
	if !strings.Contains(joined, " --network kahya-egress ") {
		t.Fatalf("expected --network kahya-egress in the main container's argv, got: %v", mainRun.args)
	}
	if strings.Contains(joined, " --network none ") {
		t.Fatalf("did not expect --network none for a needs_network:true run, got: %v", mainRun.args)
	}
	for _, want := range []string{
		"-e HTTP_PROXY=http://kahya-egress-fwd:3128",
		"-e HTTPS_PROXY=http://kahya-egress-fwd:3128",
		"-e NO_PROXY=localhost,127.0.0.1",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected %q in argv, got: %v", want, mainRun.args)
		}
	}

	// The transcript must NOT redact the (non-secret) proxy values.
	transcript := log.find("shell_docker_run")
	if len(transcript) != 1 {
		t.Fatalf("expected one shell_docker_run transcript line, got %d", len(transcript))
	}
	argv, _ := argValue(transcript[0].args, "docker_argv").([]string)
	if !strings.Contains(strings.Join(argv, " "), "HTTP_PROXY=http://kahya-egress-fwd:3128") {
		t.Fatalf("expected the proxy env value un-redacted in the transcript: %v", argv)
	}
}

// fakeEgressTokenRegistrar is EgressTokenRegistrar's test double: records
// every Register/Release call so a test can assert Runner.Run mints and
// wires a per-task token end to end (BLOCKER B/C).
type fakeEgressTokenRegistrar struct {
	mu             sync.Mutex
	registered     []struct{ token, traceID, taskID string }
	released       []string
	registeredThen func() // if set, called synchronously inside Register — this test's stand-in for "the container is now running with this token", so it can assert Release has NOT happened yet at that point.
}

func (f *fakeEgressTokenRegistrar) Register(token, traceID, taskID string) {
	f.mu.Lock()
	f.registered = append(f.registered, struct{ token, traceID, taskID string }{token, traceID, taskID})
	then := f.registeredThen
	f.mu.Unlock()
	if then != nil {
		then()
	}
}

func (f *fakeEgressTokenRegistrar) Release(token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, token)
}

// TestRun_NeedsNetworkWithTokenRegistrar_MintsRegistersAndReleasesToken is
// the BLOCKER B/C regression test at the Runner level: once an
// EgressTokenRegistrar is wired, a needs_network:true Run mints a
// per-task token, registers it (keyed on THIS call's own traceID/taskID)
// BEFORE the container starts, embeds it in the container's HTTP_PROXY/
// HTTPS_PROXY as Basic-auth userinfo, and releases it once the container
// (docker run) has exited — never before.
func TestRun_NeedsNetworkWithTokenRegistrar_MintsRegistersAndReleasesToken(t *testing.T) {
	home, workdir := baseFixture(t)
	exec := &fakeExecutor{}
	ledger := &fakeLedger{}
	log := newFakeLogger()
	pc := &fakePolicyClient{decision: allowDecision("tok")}
	r := newTestRunner(home, nil, pc, ledger, log, exec,
		&fakeDigestChecker{digest: "sha256:same"}, &fakeHealthChecker{healthy: true}, "sha256:same")
	r.SetEgressEnsurer(NewEgressNetworkEnsurer(exec, "sha256:pinned-test-digest"), 3128)

	registrar := &fakeEgressTokenRegistrar{}
	var releasedBeforeRunReturns bool
	registrar.registeredThen = func() {
		// At the moment Register fires (BEFORE the container starts),
		// nothing has been released yet.
		registrar.mu.Lock()
		releasedBeforeRunReturns = len(registrar.released) == 0
		registrar.mu.Unlock()
	}
	r.SetEgressTokenRegistrar(registrar)

	_, err := r.Run(context.Background(), "trace-token-1", "task-token-1", RunInput{Script: "echo hi", Workdir: workdir, NeedsNetwork: true})
	if err != nil {
		t.Fatalf("Run() error = %v, want success", err)
	}

	if len(registrar.registered) != 1 {
		t.Fatalf("Register calls = %d, want 1", len(registrar.registered))
	}
	reg := registrar.registered[0]
	if reg.traceID != "trace-token-1" || reg.taskID != "task-token-1" {
		t.Errorf("Register called with (traceID=%q, taskID=%q), want (%q, %q)", reg.traceID, reg.taskID, "trace-token-1", "task-token-1")
	}
	if reg.token == "" {
		t.Error("Register called with an empty token")
	}
	if !releasedBeforeRunReturns {
		t.Error("token was released BEFORE the container ever started — a token's validity window must cover the container's own run")
	}
	if len(registrar.released) != 1 || registrar.released[0] != reg.token {
		t.Fatalf("Release calls = %v, want exactly one release of %q", registrar.released, reg.token)
	}

	// The SAME token must be embedded in the container's own HTTP_PROXY/
	// HTTPS_PROXY, as Basic-auth userinfo.
	runCalls := exec.callsFor("run")
	mainRun := runCalls[len(runCalls)-1]
	joined := strings.Join(mainRun.args, " ")
	wantProxy := "HTTP_PROXY=http://" + reg.token + ":@" + EgressSidecarName + ":3128"
	if !strings.Contains(joined, wantProxy) {
		t.Fatalf("expected %q in argv, got: %v", wantProxy, mainRun.args)
	}

	// The transcript must redact JUST the userinfo/token, not the whole
	// value (env_allowlist.go's redactProxyURL).
	transcript := log.find("shell_docker_run")
	argv, _ := argValue(transcript[0].args, "docker_argv").([]string)
	argvJoined := strings.Join(argv, " ")
	if strings.Contains(argvJoined, reg.token) {
		t.Fatalf("the per-task egress token must NEVER appear in cleartext in the transcript: %v", argv)
	}
	if !strings.Contains(argvJoined, "HTTP_PROXY=http://REDACTED@"+EgressSidecarName+":3128") {
		t.Fatalf("expected the redacted (but still host:port-visible) proxy URL in the transcript, got: %v", argv)
	}
}

// TestRun_NeedsNetworkWithoutTokenRegistrar_UsesCredentialFreeProxyURL
// proves the fallback: no EgressTokenRegistrar wired means no token is
// minted at all, and the container's proxy URL is the ORIGINAL
// credential-free one — exactly this Runner's pre-BLOCKER-B/C-fix
// behavior.
func TestRun_NeedsNetworkWithoutTokenRegistrar_UsesCredentialFreeProxyURL(t *testing.T) {
	home, workdir := baseFixture(t)
	exec := &fakeExecutor{}
	pc := &fakePolicyClient{decision: allowDecision("tok")}
	r := newTestRunner(home, nil, pc, &fakeLedger{}, newFakeLogger(), exec,
		&fakeDigestChecker{digest: "sha256:same"}, &fakeHealthChecker{healthy: true}, "sha256:same")
	r.SetEgressEnsurer(NewEgressNetworkEnsurer(exec, "sha256:pinned-test-digest"), 3128)
	// No SetEgressTokenRegistrar call at all.

	_, err := r.Run(context.Background(), "trace-1", "task-1", RunInput{Script: "echo hi", Workdir: workdir, NeedsNetwork: true})
	if err != nil {
		t.Fatalf("Run() error = %v, want success", err)
	}
	runCalls := exec.callsFor("run")
	mainRun := runCalls[len(runCalls)-1]
	joined := strings.Join(mainRun.args, " ")
	if !strings.Contains(joined, "HTTP_PROXY=http://"+EgressSidecarName+":3128") {
		t.Fatalf("expected the credential-free proxy URL when no registrar is wired, got: %v", mainRun.args)
	}
}

// TestRun_NeedsNetworkEgressEnsureFailurePropagates proves an
// EgressEnsurer.Ensure failure (e.g. host.docker.internal unreachable)
// fails the run CLOSED, before the executor's main container ever runs.
func TestRun_NeedsNetworkEgressEnsureFailurePropagates(t *testing.T) {
	home, workdir := baseFixture(t)
	exec := &fakeExecutor{}
	ledger := &fakeLedger{}
	pc := &fakePolicyClient{decision: allowDecision("tok")}
	r := newTestRunner(home, nil, pc, ledger, newFakeLogger(), exec,
		&fakeDigestChecker{digest: "sha256:same"}, &fakeHealthChecker{healthy: true}, "sha256:same")
	// An EMPTY pinned digest is EgressNetworkEnsurer's own fail-closed
	// state (mirrors Runner.PinnedDigest's identical convention).
	r.SetEgressEnsurer(NewEgressNetworkEnsurer(exec, ""), 3128)

	_, err := r.Run(context.Background(), "trace-1", "task-1", RunInput{Script: "echo hi", Workdir: workdir, NeedsNetwork: true})
	if err == nil {
		t.Fatal("expected an error when the egress sidecar image digest is not pinned")
	}
	if !strings.Contains(err.Error(), reasonEgressNetworkFailed) {
		t.Errorf("error = %q, want it to contain %q", err.Error(), reasonEgressNetworkFailed)
	}
	if len(exec.callsFor("run")) != 0 {
		t.Fatalf("expected zero docker run calls when Ensure fails before ever dialing docker, got %d", len(exec.callsFor("run")))
	}
	if pc.checkCalls != 0 {
		t.Fatalf("expected Policy.Check never reached, got %d calls", pc.checkCalls)
	}
}

func TestRun_DockerUnavailableReturnsExactTurkishMessage(t *testing.T) {
	home, workdir := baseFixture(t)
	exec := &fakeExecutor{}
	ledger := &fakeLedger{}
	r := newTestRunner(home, nil, &fakePolicyClient{decision: allowDecision("tok")}, ledger, newFakeLogger(), exec,
		&fakeDigestChecker{digest: "sha256:same"}, &fakeHealthChecker{healthy: false}, "sha256:same")

	_, err := r.Run(context.Background(), "trace-1", "task-1", RunInput{Script: "echo hi", Workdir: workdir})
	if err == nil || err.Error() != "Docker çalışmıyor — 'make docker-up' ile başlatın" {
		t.Fatalf("expected the exact spec'd Turkish docker-down message, got: %v", err)
	}
	if exec.totalCalls() != 0 {
		t.Fatalf("docker-down must never reach the executor, got %d calls", exec.totalCalls())
	}
	if len(ledger.find("shell_docker_unavailable")) != 1 {
		t.Fatalf("expected shell_docker_unavailable ledger event")
	}
}

// ---- digest pin: UNIT-testable without docker via a fake DigestChecker. ----

func TestRun_DigestMismatchRefusesBeforeExecutor(t *testing.T) {
	home, workdir := baseFixture(t)
	exec := &fakeExecutor{}
	ledger := &fakeLedger{}
	pc := &fakePolicyClient{decision: allowDecision("tok")}
	r := newTestRunner(home, nil, pc, ledger, newFakeLogger(), exec,
		&fakeDigestChecker{digest: "sha256:actual-different"}, &fakeHealthChecker{healthy: true}, "sha256:committed-pin")

	_, err := r.Run(context.Background(), "trace-1", "task-1", RunInput{Script: "echo hi", Workdir: workdir})
	if err == nil {
		t.Fatal("expected digest-mismatch refusal")
	}
	if exec.totalCalls() != 0 {
		t.Fatalf("digest mismatch must never reach the executor, got %d calls", exec.totalCalls())
	}
	if pc.checkCalls != 0 {
		t.Fatalf("digest mismatch must never even reach Policy.Check (supply-chain pin outranks approval), got %d calls", pc.checkCalls)
	}
	if len(ledger.find("shell_digest_mismatch")) != 1 {
		t.Fatalf("expected shell_digest_mismatch ledger event")
	}
}

func TestRun_EmptyPinnedDigestAlwaysRefuses(t *testing.T) {
	// The committed docker/sandbox/IMAGE_DIGEST file has no real digest
	// line yet (image never built) — PinnedDigest=="" must fail closed
	// even when the DigestChecker itself is perfectly healthy.
	home, workdir := baseFixture(t)
	exec := &fakeExecutor{}
	r := newTestRunner(home, nil, &fakePolicyClient{decision: allowDecision("tok")}, &fakeLedger{}, newFakeLogger(), exec,
		&fakeDigestChecker{digest: "sha256:anything-at-all"}, &fakeHealthChecker{healthy: true}, "")

	_, err := r.Run(context.Background(), "trace-1", "task-1", RunInput{Script: "echo hi", Workdir: workdir})
	if err == nil {
		t.Fatal("expected refusal on empty PinnedDigest")
	}
	if exec.totalCalls() != 0 {
		t.Fatalf("empty pinned digest must never reach the executor, got %d calls", exec.totalCalls())
	}
}

func TestRun_DigestCheckerErrorRefuses(t *testing.T) {
	home, workdir := baseFixture(t)
	exec := &fakeExecutor{}
	r := newTestRunner(home, nil, &fakePolicyClient{decision: allowDecision("tok")}, &fakeLedger{}, newFakeLogger(), exec,
		&fakeDigestChecker{err: errors.New("no such image")}, &fakeHealthChecker{healthy: true}, "sha256:committed-pin")

	_, err := r.Run(context.Background(), "trace-1", "task-1", RunInput{Script: "echo hi", Workdir: workdir})
	if err == nil {
		t.Fatal("expected refusal when DigestChecker itself errors (image not built)")
	}
	if exec.totalCalls() != 0 {
		t.Fatalf("digest-checker error must never reach the executor, got %d calls", exec.totalCalls())
	}
}

// ---- gate chain: Check/ConsumeToken must both succeed before the executor
// ever runs — the same "validator, not gate chain, must not be the sole
// boundary" property this task's shell_host acceptance criterion names
// explicitly, exercised here for shell_docker too. ----

func TestRun_NeedsApprovalNeverReachesExecutor(t *testing.T) {
	home, workdir := baseFixture(t)
	exec := &fakeExecutor{}
	pc := &fakePolicyClient{decision: PolicyDecision{Result: PolicyResultNeedsApproval, Reason: "onay gerekiyor"}}
	r := newTestRunner(home, nil, pc, &fakeLedger{}, newFakeLogger(), exec,
		&fakeDigestChecker{digest: "sha256:same"}, &fakeHealthChecker{healthy: true}, "sha256:same")

	_, err := r.Run(context.Background(), "trace-1", "task-1", RunInput{Script: "echo hi", Workdir: workdir})
	if err == nil {
		t.Fatal("expected needs_approval to surface as an error")
	}
	if exec.totalCalls() != 0 {
		t.Fatalf("needs_approval must never reach the executor, got %d calls", exec.totalCalls())
	}
	if pc.consumeCalls != 0 {
		t.Fatalf("needs_approval must never reach ConsumeToken, got %d calls", pc.consumeCalls)
	}
}

func TestRun_ConsumeTokenFailureNeverReachesExecutor(t *testing.T) {
	home, workdir := baseFixture(t)
	exec := &fakeExecutor{}
	pc := &fakePolicyClient{decision: allowDecision("tok"), consumeErr: errors.New("policy: approval token invalid, expired, or already consumed")}
	r := newTestRunner(home, nil, pc, &fakeLedger{}, newFakeLogger(), exec,
		&fakeDigestChecker{digest: "sha256:same"}, &fakeHealthChecker{healthy: true}, "sha256:same")

	_, err := r.Run(context.Background(), "trace-1", "task-1", RunInput{Script: "echo hi", Workdir: workdir})
	if err == nil {
		t.Fatal("expected ConsumeToken failure to surface as an error")
	}
	if exec.totalCalls() != 0 {
		t.Fatalf("a failed ConsumeToken must never reach the executor, got %d calls", exec.totalCalls())
	}
	if pc.checkCalls != 1 || pc.consumeCalls != 1 {
		t.Fatalf("expected exactly one Check + one ConsumeToken call, got check=%d consume=%d", pc.checkCalls, pc.consumeCalls)
	}
}

// ---- successful run: flags, ledger, transcript. ----

func TestRun_SuccessLogsTranscriptAndLedgersShellExec(t *testing.T) {
	home, workdir := baseFixture(t)
	exec := &fakeExecutor{runResult: Result{Stdout: []byte("hello\n"), Stderr: []byte(""), ExitCode: 0}}
	ledger := &fakeLedger{}
	log := newFakeLogger()
	pc := &fakePolicyClient{decision: allowDecision("tok")}
	r := newTestRunner(home, nil, pc, ledger, log, exec,
		&fakeDigestChecker{digest: "sha256:matching"}, &fakeHealthChecker{healthy: true}, "sha256:matching")

	out, err := r.Run(context.Background(), "trace-99", "task-99", RunInput{Script: "echo hello", Workdir: workdir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.ExitCode != 0 || out.Stdout != "hello\n" || out.ImageDigest != "sha256:matching" {
		t.Fatalf("unexpected RunOutput: %+v", out)
	}
	if out.BytesOut != len("hello\n") {
		t.Fatalf("BytesOut = %d, want %d", out.BytesOut, len("hello\n"))
	}
	if pc.checkCalls != 1 || pc.consumeCalls != 1 {
		t.Fatalf("expected exactly one Check + one ConsumeToken, got check=%d consume=%d", pc.checkCalls, pc.consumeCalls)
	}

	// The docker run argv was fed the script over STDIN, never as a CLI arg.
	runCalls := exec.callsFor("run")
	if len(runCalls) != 1 {
		t.Fatalf("expected exactly one docker run invocation, got %d", len(runCalls))
	}
	if string(runCalls[0].stdin) != "echo hello" {
		t.Fatalf("script must be fed over stdin verbatim, got %q", string(runCalls[0].stdin))
	}
	joinedArgs := strings.Join(runCalls[0].args, " ")
	for _, want := range []string{"--network none", "--read-only", "--user 1000:1000", "--label kahya.task_id=task-99"} {
		if !strings.Contains(joinedArgs, want) {
			t.Errorf("real invocation argv missing %q: %v", want, runCalls[0].args)
		}
	}

	// ledger shell_exec carries every field this task's spec names.
	events := ledger.find("shell_exec")
	if len(events) != 1 {
		t.Fatalf("expected exactly one shell_exec ledger event, got %d", len(events))
	}
	ev := events[0]
	if ev.traceID != "trace-99" {
		t.Fatalf("shell_exec ledger event trace_id = %q, want trace-99", ev.traceID)
	}
	for _, key := range []string{"image_digest", "workdir", "exit_code", "bytes_out", "trace_id"} {
		if _, ok := ev.payload[key]; !ok {
			t.Errorf("shell_exec ledger payload missing key %q: %+v", key, ev.payload)
		}
	}

	// The pre-run transcript line ("docker run transcript in JSONL logs
	// shows --network none") is independently greppable.
	transcript := log.find("shell_docker_run")
	if len(transcript) != 1 {
		t.Fatalf("expected exactly one shell_docker_run transcript line, got %d", len(transcript))
	}
	argv, ok := argValue(transcript[0].args, "docker_argv").([]string)
	if !ok {
		t.Fatalf("shell_docker_run transcript missing docker_argv: %+v", transcript[0].args)
	}
	joinedTranscript := strings.Join(argv, " ")
	if !strings.Contains(joinedTranscript, "--network none") {
		t.Fatalf("transcript must show --network none: %v", argv)
	}
	if !strings.Contains(joinedTranscript, "--label kahya.task_id=task-99") {
		t.Fatalf("transcript must show the kahya.task_id label: %v", argv)
	}
}

// TestRun_EnvAllowlistOnlyForwardsAllowedNames proves the happy path of
// BLOCKER 2 fix part a: a NAME actually in safeEnvAllowlist (env_
// allowlist.go) is still forwarded exactly as before; a name absent from
// the host env is still silently skipped (unrelated to the safe-name
// restriction).
func TestRun_EnvAllowlistOnlyForwardsAllowedNames(t *testing.T) {
	home, workdir := baseFixture(t)
	exec := &fakeExecutor{runResult: Result{ExitCode: 0}}
	r := newTestRunner(home, nil, &fakePolicyClient{decision: allowDecision("tok")}, &fakeLedger{}, newFakeLogger(), exec,
		&fakeDigestChecker{digest: "sha256:matching"}, &fakeHealthChecker{healthy: true}, "sha256:matching")
	r.EnvLookup = func(name string) (string, bool) {
		if name == "LANG" {
			return "en_US.UTF-8", true
		}
		return "", false
	}

	_, err := r.Run(context.Background(), "trace-1", "task-1", RunInput{
		Script: "echo hi", Workdir: workdir, EnvAllowlist: []string{"LANG", "NOT_SET"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	runCalls := exec.callsFor("run")
	joined := strings.Join(runCalls[0].args, " ")
	if !strings.Contains(joined, "-e LANG=en_US.UTF-8") {
		t.Fatalf("expected -e LANG=en_US.UTF-8 in argv: %v", runCalls[0].args)
	}
	if strings.Contains(joined, "NOT_SET") {
		t.Fatalf("NOT_SET must never appear (absent from host env): %v", runCalls[0].args)
	}
}

// TestRun_EnvAllowlistRejectsNameNotInSafeAllowlist proves BLOCKER 2 fix
// part a's "only forward from a small hardcoded SAFE-NAME allowlist" half:
// a perfectly innocuous-looking name (no secret-shaped substring/prefix at
// all) is STILL dropped if it is not one of the few names env_allowlist.go
// hardcodes, since growing the forwardable set must require editing that
// file, never a model-supplied allowlist entry.
func TestRun_EnvAllowlistRejectsNameNotInSafeAllowlist(t *testing.T) {
	home, workdir := baseFixture(t)
	exec := &fakeExecutor{runResult: Result{ExitCode: 0}}
	ledger := &fakeLedger{}
	r := newTestRunner(home, nil, &fakePolicyClient{decision: allowDecision("tok")}, ledger, newFakeLogger(), exec,
		&fakeDigestChecker{digest: "sha256:matching"}, &fakeHealthChecker{healthy: true}, "sha256:matching")
	r.EnvLookup = func(name string) (string, bool) {
		if name == "FOO" {
			return "bar", true
		}
		return "", false
	}

	_, err := r.Run(context.Background(), "trace-1", "task-1", RunInput{
		Script: "echo hi", Workdir: workdir, EnvAllowlist: []string{"FOO"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	runCalls := exec.callsFor("run")
	if strings.Contains(strings.Join(runCalls[0].args, " "), "FOO") {
		t.Fatalf("FOO is not in safeEnvAllowlist and must never be forwarded: %v", runCalls[0].args)
	}
	if len(ledger.find("shell_env_name_rejected")) != 1 {
		t.Fatalf("expected exactly one shell_env_name_rejected ledger event, got %d", len(ledger.find("shell_env_name_rejected")))
	}
}

// TestRun_EnvAllowlistRejectsSecretShapedNameAndNeverLogsItsValue is
// BLOCKER 2's central regression: env_allowlist naming a kahyad-process
// secret-shaped var (KAHYA_ANTHROPIC_KEY_OVERRIDE, kahyad/internal/
// anthproxy's own dev/CI Keychain substitute — kahyad/internal/spawn's
// secretEnvDenylist closes the identical worker-facing leak) must NOT be
// forwarded into the real docker argv, AND the secret's value must appear
// NOWHERE in any logged or ledgered payload — not even in a rejection
// event, since resolveEnv must never even call EnvLookup for a name that
// fails isForwardableEnvName.
func TestRun_EnvAllowlistRejectsSecretShapedNameAndNeverLogsItsValue(t *testing.T) {
	home, workdir := baseFixture(t)
	exec := &fakeExecutor{runResult: Result{ExitCode: 0}}
	ledger := &fakeLedger{}
	log := newFakeLogger()
	r := newTestRunner(home, nil, &fakePolicyClient{decision: allowDecision("tok")}, ledger, log, exec,
		&fakeDigestChecker{digest: "sha256:matching"}, &fakeHealthChecker{healthy: true}, "sha256:matching")
	const secretName = "KAHYA_ANTHROPIC_KEY_OVERRIDE"
	const secretValue = "sk-super-secret-value-must-never-leak"
	lookedUp := false
	r.EnvLookup = func(name string) (string, bool) {
		if name == secretName {
			lookedUp = true
			return secretValue, true
		}
		return "", false
	}

	_, err := r.Run(context.Background(), "trace-1", "task-1", RunInput{
		Script: "echo hi", Workdir: workdir, EnvAllowlist: []string{secretName},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lookedUp {
		t.Fatal("a secret-shaped env_allowlist name must never even be looked up in the host environment")
	}

	runCalls := exec.callsFor("run")
	joinedArgv := strings.Join(runCalls[0].args, " ")
	if strings.Contains(joinedArgv, secretName) {
		t.Fatalf("secret-shaped env name must never be forwarded into the real docker argv: %v", runCalls[0].args)
	}
	if strings.Contains(joinedArgv, secretValue) {
		t.Fatalf("secret value must never appear in the real docker argv: %v", runCalls[0].args)
	}

	// Nowhere in ANY logged line's args may the secret value appear.
	for _, ln := range *log.lines {
		for _, a := range ln.args {
			switch v := a.(type) {
			case string:
				if strings.Contains(v, secretValue) {
					t.Fatalf("secret value leaked into log line %q: %v", ln.event, ln.args)
				}
			case []string:
				if strings.Contains(strings.Join(v, " "), secretValue) {
					t.Fatalf("secret value leaked into log line %q argv: %v", ln.event, v)
				}
			}
		}
	}
	// Nor in any ledgered payload.
	for _, ev := range ledger.events {
		for _, v := range ev.payload {
			if s, ok := v.(string); ok && strings.Contains(s, secretValue) {
				t.Fatalf("secret value leaked into ledger event %q: %+v", ev.kind, ev.payload)
			}
		}
	}
	if len(ledger.find("shell_env_name_rejected")) != 1 {
		t.Fatalf("expected exactly one shell_env_name_rejected ledger event, got %d", len(ledger.find("shell_env_name_rejected")))
	}
}

// TestRun_TranscriptRedactsForwardedEnvValues is BLOCKER 2's part b
// regression: a NAME that IS forwarded (safe-allowlisted, present in the
// host env) still must never have its VALUE appear in the logged/ledgered
// shell_docker_run transcript — only "-e NAME=<redacted>" — while the REAL
// docker invocation (exec.callsFor("run")) still carries the real value.
func TestRun_TranscriptRedactsForwardedEnvValues(t *testing.T) {
	home, workdir := baseFixture(t)
	exec := &fakeExecutor{runResult: Result{ExitCode: 0}}
	log := newFakeLogger()
	r := newTestRunner(home, nil, &fakePolicyClient{decision: allowDecision("tok")}, &fakeLedger{}, log, exec,
		&fakeDigestChecker{digest: "sha256:matching"}, &fakeHealthChecker{healthy: true}, "sha256:matching")
	r.EnvLookup = func(name string) (string, bool) {
		if name == "LANG" {
			return "en_US.UTF-8", true
		}
		return "", false
	}

	_, err := r.Run(context.Background(), "trace-1", "task-1", RunInput{
		Script: "echo hi", Workdir: workdir, EnvAllowlist: []string{"LANG"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	transcript := log.find("shell_docker_run")
	if len(transcript) != 1 {
		t.Fatalf("expected exactly one shell_docker_run transcript line, got %d", len(transcript))
	}
	argv, ok := argValue(transcript[0].args, "docker_argv").([]string)
	if !ok {
		t.Fatalf("shell_docker_run transcript missing docker_argv: %+v", transcript[0].args)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "-e LANG=<redacted>") {
		t.Fatalf("expected -e LANG=<redacted> in the logged transcript, got: %v", argv)
	}
	if strings.Contains(joined, "en_US.UTF-8") {
		t.Fatalf("env VALUE must never appear in the logged transcript: %v", argv)
	}

	// The REAL docker invocation (never logged) must still carry the real
	// value — redaction is a transcript-only concern.
	runCalls := exec.callsFor("run")
	if !strings.Contains(strings.Join(runCalls[0].args, " "), "-e LANG=en_US.UTF-8") {
		t.Fatalf("the real docker argv must still carry the real env value: %v", runCalls[0].args)
	}
}

// ---- timeout / kill. ----

func TestRun_TimeoutKillsContainer(t *testing.T) {
	home, workdir := baseFixture(t)
	exec := &fakeExecutor{runBlocksUntilCtxDone: true}
	log := newFakeLogger()
	r := newTestRunner(home, nil, &fakePolicyClient{decision: allowDecision("tok")}, &fakeLedger{}, log, exec,
		&fakeDigestChecker{digest: "sha256:matching"}, &fakeHealthChecker{healthy: true}, "sha256:matching")
	r.SetTimeoutUnit(time.Millisecond) // TimeoutS=5 => 5ms real deadline

	out, err := r.Run(context.Background(), "trace-1", "task-1", RunInput{Script: "sleep 999", Workdir: workdir, TimeoutS: 5})
	if err != nil {
		t.Fatalf("a timeout must surface as TimedOut, not a hard error: %v", err)
	}
	if !out.TimedOut {
		t.Fatal("expected RunOutput.TimedOut = true")
	}
	killCalls := exec.callsFor("kill")
	if len(killCalls) != 1 {
		t.Fatalf("expected exactly one docker kill call on timeout, got %d", len(killCalls))
	}
	if killCalls[0].args[1] != out.ContainerName {
		t.Fatalf("docker kill must target the SAME container name that was run, got %q want %q", killCalls[0].args[1], out.ContainerName)
	}
}

// ---- container labels present (also exercised end-to-end above via the
// transcript check) — this test isolates just the label + KillAllLabeled
// seam kahyad's shutdown path uses. ----

func TestKillAllLabeled_KillsEveryListedContainer(t *testing.T) {
	exec := &fakeExecutor{psOutput: "cid1\ncid2\n"}
	r := &Runner{Exec: exec}
	if err := r.KillAllLabeled(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	psCalls := exec.callsFor("ps")
	if len(psCalls) != 1 {
		t.Fatalf("expected one docker ps call, got %d", len(psCalls))
	}
	if !strings.Contains(strings.Join(psCalls[0].args, " "), "label=kahya.task_id") {
		t.Fatalf("docker ps must filter by label=kahya.task_id: %v", psCalls[0].args)
	}
	killCalls := exec.callsFor("kill")
	if len(killCalls) != 2 {
		t.Fatalf("expected 2 docker kill calls (one per listed container), got %d", len(killCalls))
	}
	got := map[string]bool{killCalls[0].args[1]: true, killCalls[1].args[1]: true}
	if !got["cid1"] || !got["cid2"] {
		t.Fatalf("expected kill for cid1 and cid2, got %v", got)
	}
}

// ---- LoadPinnedDigest. ----

func TestLoadPinnedDigest(t *testing.T) {
	dir := t.TempDir()

	t.Run("missing file returns empty, no error", func(t *testing.T) {
		got, err := LoadPinnedDigest(filepath.Join(dir, "does-not-exist"))
		if err != nil || got != "" {
			t.Fatalf("got %q, %v; want \"\", nil", got, err)
		}
	})

	t.Run("comment-only placeholder returns empty", func(t *testing.T) {
		path := filepath.Join(dir, "placeholder")
		if err := os.WriteFile(path, []byte("# not built yet\n\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := LoadPinnedDigest(path)
		if err != nil || got != "" {
			t.Fatalf("got %q, %v; want \"\", nil", got, err)
		}
	})

	t.Run("real digest line is returned trimmed", func(t *testing.T) {
		path := filepath.Join(dir, "real")
		if err := os.WriteFile(path, []byte("# comment\nsha256:abcdef123456\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := LoadPinnedDigest(path)
		if err != nil || got != "sha256:abcdef123456" {
			t.Fatalf("got %q, %v; want \"sha256:abcdef123456\", nil", got, err)
		}
	})
}
