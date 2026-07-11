// Package shell implements the W3-04 shell tool set: shell_docker (a
// pinned-image, network-off-by-default Docker sandbox) and shell_host (a
// narrow, argument-validated set of host commands) — two kahyad-owned MCP
// tools, registered into kahyad's shared /v1/mcp server exactly like
// mcp/fs (W3-03): the worker reaches these tools only through kahyad's
// POST /v1/mcp.
//
// Like mcp/fs, this package performs its OWN policy decision + one-time
// token consumption, in a fixed order, INSIDE Runner.Run/HostExec.Handle,
// rather than relying on kahyad's generic /v1/mcp policy-gate middleware:
//
//  1. mechanical, non-negotiable checks that can NEVER be overridden by an
//     approval — canonicalize + deny-glob check on the workdir
//     (shell_docker) or the argv validator (shell_host), needs_network
//     fail-closed, the docker-health check, the image-digest pin — each of
//     these denies BEFORE any policy decision is even consulted, mirroring
//     mcp/fs's "deny-glob check runs BEFORE approval flow" ordering
//     (HANDOFF §5 safety #6).
//  2. Policy.Check (the same wire shape as POST /policy/check).
//  3. Policy.ConsumeToken (POST /policy/consume-token) — must succeed
//     before a single byte of the script/argv is ever executed.
//  4. the actual execution (docker run / the fixed-argv host command).
//  5. the ledger event (shell_exec / hostexec_exec or hostexec_denied).
//
// PolicyClient, Ledger, and Logger are literal type ALIASES of mcp/fs's
// own interfaces (see the aliases below) — mcp/fs's package doc comment
// already anticipated this: "a LATER out-of-process tool (W3-04's shell
// tool) can satisfy the exact same interface with a real HTTP client with
// zero changes to the call sites in this file". Reusing the identical
// interface TYPE (rather than merely a same-shaped one) means kahyad's
// existing mcp/fs adapters (enginePolicyClient, fsLoggerAdapter,
// *store.Store) satisfy this package's dependencies with no new adapter
// code at all — see kahyad/internal/server/shell.go.
//
// mcp/fs.Canonicalize/MatchesAnyGlobCI are reused directly (not
// reimplemented) for the shell_docker workdir and shell_host's RepoPath/
// paths — the exact same NFC-normalize + bidi/zero-width-rune-reject +
// deepest-existing-ancestor-symlink-resolve pipeline W3-03 built, per this
// task's own instruction.
package shell

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	mcpfs "kahya/mcp/fs"
)

// PolicyClient/Ledger/Logger are type ALIASES of mcp/fs's own interfaces
// (this file's package doc comment explains why: identical type, not
// merely identical shape, so kahyad's existing mcp/fs adapters satisfy
// this package with zero new code).
type (
	PolicyClient   = mcpfs.PolicyClient
	Ledger         = mcpfs.Ledger
	Logger         = mcpfs.Logger
	PolicyDecision = mcpfs.PolicyDecision
)

// Result values, aliased from mcp/fs for the same reason as PolicyClient/
// Ledger/Logger above.
const (
	PolicyResultAllow         = mcpfs.PolicyResultAllow
	PolicyResultNeedsApproval = mcpfs.PolicyResultNeedsApproval
	PolicyResultDeny          = mcpfs.PolicyResultDeny
)

const (
	// defaultScope is the ladder scope shell_docker/shell_host check
	// under (policy.yaml declares no scope_key for either, which
	// kahyad/internal/policy/loader.go's normalize step defaults to
	// exactly this value — mirrors mcp/fs's identical defaultScope).
	defaultScope = "global"

	// defaultTimeoutSeconds is shell_docker's hard timeout when the
	// caller supplies TimeoutS<=0 (this task's spec step 7, verbatim:
	// "hard timeout_s (default 300)").
	defaultTimeoutSeconds = 300

	// containerWorkdir is the FIXED in-container mount point every
	// invocation's workdir is bound to (this task's spec step 3).
	containerWorkdir = "/work"

	// sandboxUser/pidsLimit/memoryLimit are the NON-NEGOTIABLE resource/
	// identity flags every docker run carries (this task's spec step 3) —
	// matches docker/sandbox/Dockerfile's own uid 1000 "kahya" user.
	sandboxUser = "1000:1000"
	pidsLimit   = "256"
	memoryLimit = "2g"
)

// Turkish, user/model-facing deny reasons (CLAUDE.md language policy) —
// each corresponds to one of the mechanical, pre-policy-check refusals
// this package's doc comment describes; none of them can be bypassed by
// an approval token.
const (
	reasonEmptyScript     = "shell_docker reddedildi: script boş olamaz."
	reasonWorkdirDenyGlob = "shell_docker reddedildi: iş dizini izin verilmeyen bir desenle eşleşiyor (fs_write_deny_globs); onay bu kuralı geçersiz kılamaz."
	reasonNetworkRejected = "shell_docker reddedildi: ağ erişimi gerektiren görevler henüz desteklenmiyor (kahya-egress proxy'si W3-05 ile gelecek)."
	reasonDigestMismatch  = "shell_docker reddedildi: sandbox imajı beklenen özet ile eşleşmiyor (tedarik zinciri sabitleme ihlali) — 'make sandbox-image' çalıştırıp yeniden deneyin."
	// reasonDockerDown is the EXACT string this task's spec quotes
	// verbatim — do not reword.
	reasonDockerDown = "Docker çalışmıyor — 'make docker-up' ile başlatın"
)

// RunInput is shell_docker's invocation contract (this task's spec,
// verbatim): {script, workdir, timeout_s, needs_network, env_allowlist}.
// This struct doubles as the shell_docker MCP tool's wire argument type
// (server.go registers it directly — no separate wire struct).
type RunInput struct {
	Script  string `json:"script" jsonschema:"Docker sandbox içinde /bin/sh'a stdin olarak verilecek script metni"`
	Workdir string `json:"workdir" jsonschema:"rw bind-mount edilecek TEK dizin (mutlak veya ~ ile başlayan yol) — script yalnız bu dizine kalıcı yazabilir"`
	// TimeoutS<=0 defaults to defaultTimeoutSeconds.
	TimeoutS int `json:"timeout_s,omitempty" jsonschema:"saniye cinsinden sert zaman aşımı (varsayılan 300)"`
	// NeedsNetwork is ALWAYS rejected today (W3-05 has not landed) — see
	// Runner.Run's own doc comment on this fail-closed seam.
	NeedsNetwork bool `json:"needs_network,omitempty" jsonschema:"ağ erişimi gerekiyorsa true (W3-05 gelene kadar HER ZAMAN reddedilir)"`
	// EnvAllowlist names host env vars to forward into the container
	// (looked up via Runner.EnvLookup — never trusted as caller-supplied
	// values themselves).
	EnvAllowlist []string `json:"env_allowlist,omitempty" jsonschema:"konteynere aktarılacak host ortam değişkeni adları (değerler host'tan okunur)"`
}

// RunOutput is shell_docker's result.
type RunOutput struct {
	ExitCode      int    `json:"exit_code"`
	Stdout        string `json:"stdout"`
	Stderr        string `json:"stderr"`
	BytesOut      int    `json:"bytes_out"`
	ImageDigest   string `json:"image_digest"`
	Workdir       string `json:"workdir"`
	ContainerName string `json:"container_name"`
	TimedOut      bool   `json:"timed_out"`
}

// Result is one Executor.Run's outcome.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// Executor is the process-execution seam every command this package
// issues (docker run/kill/inspect/ps/info, and hostexec's git/ls/stat)
// goes through — production code (processExecutor) shells out for real;
// tests inject a stub that records invocations without ever touching a
// real process. This is exactly how this task's acceptance criteria
// ("digest pin — UNIT-testable without docker", "stub executor asserting
// zero invocations") are satisfied with no daemon involved at all.
type Executor interface {
	// Run executes name with args, feeding stdin (nil for none) on the
	// child's standard input, blocking until it exits or ctx is done.
	// When ctx is done before the process exits, the implementation kills
	// it and returns ctx.Err() alongside whatever partial Result was
	// captured.
	Run(ctx context.Context, name string, args []string, stdin []byte) (Result, error)
}

// processExecutor is the production Executor: os/exec, fixed argv only —
// NEVER a shell string, anywhere (HANDOFF §5 safety #6's "ikili-allowlist
// güvenlik sınırı değil" applies equally to how THIS package itself
// invokes docker/git/ls/stat: no interpolation, ever).
type processExecutor struct{}

func (processExecutor) Run(ctx context.Context, name string, args []string, stdin []byte) (Result, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	if ctx.Err() != nil {
		// Killed by our own timeout (exec.CommandContext already sent the
		// kill signal to this CLI process) — surface ctx.Err() so
		// Runner.Run can tell this apart from an ordinary non-zero exit.
		return Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: -1}, ctx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: exitErr.ExitCode()}, nil
	}
	if runErr != nil {
		return Result{}, fmt.Errorf("exec %s: %w", name, runErr)
	}
	return Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: 0}, nil
}

// DigestChecker resolves imageRef's CURRENT local image digest — the
// W3-04 supply-chain pin (this task's spec: "runner refuses to start
// containers whose image digest differs from the committed one").
// Production code (dockerDigestChecker) asks the real docker daemon;
// tests inject a fake returning a fixed/mismatched value so the pin's
// REFUSAL path is unit-testable with no daemon involved at all.
type DigestChecker interface {
	Digest(ctx context.Context, imageRef string) (string, error)
}

// dockerDigestChecker is the production DigestChecker: `docker image
// inspect --format {{.Id}}`.
//
// DEVIATION (documented here and in docker/README.md / this task's commit
// message): a purely LOCAL build that has never been pushed to a registry
// has no RepoDigest — `docker images --digests` shows "<none>" for it,
// a well-known Docker quirk, not a bug in this package. This checker pins
// against the image ID instead (the sha256 of the image's config, which
// changes on ANY layer/config change exactly like a real registry digest
// would) — the identical supply-chain-pin security property this task
// requires, just sourced from `docker image inspect` rather than a
// registry-only field.
type dockerDigestChecker struct{ exec Executor }

func (d dockerDigestChecker) Digest(ctx context.Context, imageRef string) (string, error) {
	res, err := d.exec.Run(ctx, "docker", []string{"image", "inspect", "--format", "{{.Id}}", imageRef}, nil)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("docker image inspect %s: %s", imageRef, strings.TrimSpace(string(res.Stderr)))
	}
	return strings.TrimSpace(string(res.Stdout)), nil
}

// DockerHealthChecker reports whether the docker daemon is reachable
// (`docker info`) — this task's spec step 1: "kahyad health-checks
// `docker info` at startup; if unavailable, shell_docker requests return
// a clean Turkish error ... and the task is not silently retried." This
// package re-checks on EVERY Runner.Run call (not just once at boot):
// strictly more correct (colima coming up AFTER kahyad's own boot needs no
// daemon restart to notice), and still satisfies "health-checks docker
// info at startup" since main.go also calls this once at boot to log a
// friendly readiness line.
type DockerHealthChecker interface {
	Healthy(ctx context.Context) bool
}

type dockerHealthChecker struct{ exec Executor }

func (d dockerHealthChecker) Healthy(ctx context.Context) bool {
	res, err := d.exec.Run(ctx, "docker", []string{"info"}, nil)
	return err == nil && res.ExitCode == 0
}

// noopLogger is the default Logger when New/NewRunner/NewHostExec are not
// given one (mirrors mcp/fs's identical noopLogger).
type noopLogger struct{}

func (noopLogger) With(string) Logger   { return noopLogger{} }
func (noopLogger) Info(string, ...any)  {}
func (noopLogger) Warn(string, ...any)  {}
func (noopLogger) Error(string, ...any) {}

// Runner implements shell_docker's full lifecycle (this file's package
// doc comment): canonicalize workdir → deny-glob check → needs_network
// fail-closed → docker health check → digest pin check → policy gate
// (Check+ConsumeToken) → docker run with the non-negotiable flags →
// timeout/kill → ledger shell_exec.
type Runner struct {
	// Home is the directory "~" expands against for Workdir
	// canonicalization (mcp/fs.Canonicalize) — same convention as
	// mcp/fs.Server.Home.
	Home string
	// DenyGlobs is policy.yaml's fs_write_deny_globs (already
	// ~-expanded): a task's workdir must not match any of these (this
	// task's spec step 3).
	DenyGlobs []string

	// ImageTag is the sandbox image's tag (e.g. "kahya-sandbox:0.1.0").
	ImageTag string
	// PinnedDigest is docker/sandbox/IMAGE_DIGEST's committed content
	// (empty, or any value that does not match DigestChecker's answer,
	// means every run is refused — fail-closed).
	PinnedDigest string

	Policy PolicyClient
	Ledger Ledger
	Log    Logger

	Exec          Executor
	DigestChecker DigestChecker
	Health        DockerHealthChecker

	// EnvLookup resolves an allowed env var's value from the host
	// environment (defaults to os.LookupEnv in NewRunner) —
	// RunInput.EnvAllowlist NAMES are looked up through this; their
	// VALUES are never trusted as caller-supplied.
	EnvLookup func(name string) (string, bool)

	// timeoutUnit scales RunInput.TimeoutS into a time.Duration (defaults
	// to time.Second; tests shrink this to time.Millisecond so a
	// timeout/kill test runs in milliseconds, never a real 300s wait —
	// see SetTimeoutUnit).
	timeoutUnit time.Duration
}

// NewRunner constructs a production Runner: home is the real user home
// directory; imageTag/pinnedDigest are typically cfg.DockerImageTag and
// the content of docker/sandbox/IMAGE_DIGEST (LoadPinnedDigest);
// denyGlobs is policy.yaml's fs_write_deny_globs.
func NewRunner(home, imageTag, pinnedDigest string, denyGlobs []string, policy PolicyClient, ledger Ledger, log Logger) *Runner {
	if log == nil {
		log = noopLogger{}
	}
	exec := processExecutor{}
	return &Runner{
		Home: home, DenyGlobs: denyGlobs, ImageTag: imageTag, PinnedDigest: pinnedDigest,
		Policy: policy, Ledger: ledger, Log: log,
		Exec: exec, DigestChecker: dockerDigestChecker{exec: exec}, Health: dockerHealthChecker{exec: exec},
		EnvLookup: os.LookupEnv, timeoutUnit: time.Second,
	}
}

// SetTimeoutUnit overrides the unit RunInput.TimeoutS is scaled by
// (production default: time.Second). Tests only — lets a timeout/kill
// test run in milliseconds.
func (r *Runner) SetTimeoutUnit(d time.Duration) { r.timeoutUnit = d }

// Run executes one shell_docker invocation end to end (this file's
// package doc comment lists the fixed gate order). needs_network:true is
// ALWAYS rejected: W3-05 (the kahya-egress internal network + proxy
// sidecar this flag is supposed to attach to) has not landed yet, so
// there is nothing safe to attach to — accepting it anyway would mean a
// container reaching the network with no allowlist/volume-budget
// enforcement at all, exactly the bypass HANDOFF §5 safety #1 warns
// against ("aksi hâlde container içi curl allowlist'i atlar"). The
// attachment mechanism this comment refers to is intentionally NOT
// implemented here — W3-05 adds it; this fail-closed rejection is the
// documented seam.
func (r *Runner) Run(ctx context.Context, traceID, taskID string, in RunInput) (RunOutput, error) {
	if strings.TrimSpace(in.Script) == "" {
		return RunOutput{}, errors.New(reasonEmptyScript)
	}
	timeoutS := in.TimeoutS
	if timeoutS <= 0 {
		timeoutS = defaultTimeoutSeconds
	}

	cp, err := mcpfs.Canonicalize(r.Home, in.Workdir)
	if err != nil {
		return RunOutput{}, fmt.Errorf("shell_docker: %w", err)
	}
	hit, err := mcpfs.MatchesAnyGlobCI(cp.Match, r.DenyGlobs)
	if err != nil {
		return RunOutput{}, fmt.Errorf("shell_docker: %w", err)
	}
	if hit {
		logAndLedger(ctx, r.Ledger, r.Log, traceID, "shell_deny_glob_hit", map[string]any{
			"event": "shell_deny_glob_hit", "tool": "shell_docker", "canonical_workdir": cp.Match, "task_id": taskID,
		})
		return RunOutput{}, errors.New(reasonWorkdirDenyGlob)
	}

	if in.NeedsNetwork {
		logAndLedger(ctx, r.Ledger, r.Log, traceID, "shell_network_rejected", map[string]any{
			"event": "shell_network_rejected", "tool": "shell_docker", "workdir": cp.Match, "task_id": taskID,
		})
		return RunOutput{}, errors.New(reasonNetworkRejected)
	}

	if r.Health != nil && !r.Health.Healthy(ctx) {
		logAndLedger(ctx, r.Ledger, r.Log, traceID, "shell_docker_unavailable", map[string]any{
			"event": "shell_docker_unavailable", "tool": "shell_docker", "task_id": taskID,
		})
		return RunOutput{}, errors.New(reasonDockerDown)
	}

	actualDigest, digErr := r.DigestChecker.Digest(ctx, r.ImageTag)
	if digErr != nil || r.PinnedDigest == "" || actualDigest != r.PinnedDigest {
		logAndLedger(ctx, r.Ledger, r.Log, traceID, "shell_digest_mismatch", map[string]any{
			"event": "shell_digest_mismatch", "tool": "shell_docker", "task_id": taskID,
			"image_tag": r.ImageTag, "pinned_digest": r.PinnedDigest, "actual_digest": actualDigest,
		})
		return RunOutput{}, errors.New(reasonDigestMismatch)
	}

	// WYSIWYE (HANDOFF §5 safety #5, until W3-06's real normalize+hash
	// pipeline lands): the token/pending-approval is bound to the RAW
	// SCRIPT BYTES' sha256 — this task's spec, verbatim: "Token binds to
	// the raw command bytes SHA-256".
	toolInput := []byte(in.Script)
	decision, err := r.Policy.Check(ctx, "shell_docker", defaultScope, taskID, traceID, toolInput)
	if err != nil {
		return RunOutput{}, fmt.Errorf("shell_docker: %w", err)
	}
	if decision.Result != mcpfs.PolicyResultAllow {
		return RunOutput{}, errors.New(decision.Reason)
	}
	if err := r.Policy.ConsumeToken(ctx, decision.Token, "shell_docker", decision.Class, defaultScope, taskID, traceID, toolInput); err != nil {
		return RunOutput{}, fmt.Errorf("shell_docker: onay jetonu tüketilemedi: %w", err)
	}

	containerName := containerNameFor(taskID)
	args := buildDockerRunArgs(dockerRunSpec{
		ImageTag: r.ImageTag, ContainerName: containerName, TaskID: taskID,
		Workdir: cp.Op, EnvPairs: r.resolveEnv(in.EnvAllowlist),
	})

	// Logged BEFORE execution so the JSONL transcript this task's own
	// acceptance criteria grep for ("docker run transcript ... shows
	// --network none", "container labels present") exists regardless of
	// how the run itself turns out.
	logAndLedger(ctx, r.Ledger, r.Log, traceID, "shell_docker_run", map[string]any{
		"event": "shell_docker_run", "tool": "shell_docker", "task_id": taskID,
		"container_name": containerName, "image_tag": r.ImageTag, "image_digest": actualDigest,
		"workdir": cp.Match, "docker_argv": append([]string{"docker"}, args...),
	})

	unit := r.timeoutUnit
	if unit <= 0 {
		unit = time.Second
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutS)*unit)
	defer cancel()

	res, runErr := r.Exec.Run(timeoutCtx, "docker", args, []byte(in.Script))
	timedOut := errors.Is(runErr, context.DeadlineExceeded)
	if timedOut {
		// this task's spec step 7: "on timeout docker kill" — issued on a
		// fresh, un-timed-out context, since timeoutCtx is already
		// expired.
		_, _ = r.Exec.Run(context.Background(), "docker", []string{"kill", containerName}, nil)
		r.logWith(traceID).Warn("shell_docker_timeout", "container_name", containerName, "task_id", taskID, "timeout_s", timeoutS)
	} else if runErr != nil {
		return RunOutput{}, fmt.Errorf("shell_docker: %w", runErr)
	}

	out := RunOutput{
		ExitCode: res.ExitCode, Stdout: string(res.Stdout), Stderr: string(res.Stderr),
		BytesOut: len(res.Stdout) + len(res.Stderr), ImageDigest: actualDigest,
		Workdir: cp.Match, ContainerName: containerName, TimedOut: timedOut,
	}

	logAndLedger(ctx, r.Ledger, r.Log, traceID, "shell_exec", map[string]any{
		"event": "shell_exec", "image_digest": actualDigest, "workdir": cp.Match,
		"exit_code": out.ExitCode, "bytes_out": out.BytesOut, "trace_id": traceID, "task_id": taskID,
		"container_name": containerName, "timed_out": timedOut,
	})

	return out, nil
}

// KillAllLabeled kills every container carrying the kahya.task_id label
// (this task's spec step 7: "on kahyad shutdown, kill all containers
// labeled kahya.task_id") — queried fresh via `docker ps`, NOT from an
// in-process registry, so it also catches containers a crashed/restarted
// kahyad process left behind. Call from kahyad's own graceful-shutdown
// path (kahyad/internal/server/shell.go).
func (r *Runner) KillAllLabeled(ctx context.Context) error {
	res, err := r.Exec.Run(ctx, "docker", []string{"ps", "-q", "--filter", "label=kahya.task_id"}, nil)
	if err != nil {
		return fmt.Errorf("shell_docker: list kahya.task_id-labeled containers: %w", err)
	}
	var firstErr error
	for _, id := range strings.Fields(string(res.Stdout)) {
		if _, err := r.Exec.Run(ctx, "docker", []string{"kill", id}, nil); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// resolveEnv looks up allowlist's names through r.EnvLookup, silently
// skipping any name absent from the host environment — RunInput.
// EnvAllowlist supplies NAMES only; VALUES always come from the host,
// never from the caller.
func (r *Runner) resolveEnv(allowlist []string) map[string]string {
	lookup := r.EnvLookup
	if lookup == nil {
		lookup = os.LookupEnv
	}
	out := make(map[string]string, len(allowlist))
	for _, name := range allowlist {
		if v, ok := lookup(name); ok {
			out[name] = v
		}
	}
	return out
}

func (r *Runner) logWith(traceID string) Logger {
	if r.Log == nil {
		return noopLogger{}
	}
	return r.Log.With(traceID)
}

// dockerRunSpec is buildDockerRunArgs' input.
type dockerRunSpec struct {
	ImageTag      string
	ContainerName string
	TaskID        string
	Workdir       string // host-side canonical path (CanonicalPath.Op)
	EnvPairs      map[string]string
}

// buildDockerRunArgs constructs the FIXED, non-negotiable `docker run`
// argv (this task's spec step 3 — every flag here is load-bearing, never
// config-overridable, and this function is a PURE function specifically
// so a unit test can assert every flag's presence with no docker daemon
// involved): `--network none` (needs_network:true never reaches this
// function at all — Runner.Run rejects it long before — see Run's own doc
// comment on that fail-closed seam), `--read-only` root + `--tmpfs /tmp`,
// `-v <workdir>:/work:rw` as the ONLY bind mount, `--user 1000:1000`,
// `--pids-limit 256`, `--memory 2g`, `--cap-drop ALL`, `--security-opt
// no-new-privileges`, `--label kahya.task_id=<id>` (spec step 7: "label
// every container"), `--rm` (auto-remove on exit so sandbox containers
// never accumulate — HANDOFF gives no container-retention requirement).
// The Docker socket is never mounted (this task's own gotcha, HANDOFF §5
// safety #6 context). The container's own command is a bare `/bin/sh`,
// fed the script over STDIN (`-i`) — never as a command-line argument —
// so the script's raw bytes are exactly what a shell-quoting bug could
// otherwise mangle, and exactly what Runner.Run's WYSIWYE token hash
// already bound the approval to.
func buildDockerRunArgs(spec dockerRunSpec) []string {
	args := []string{
		"run", "--rm", "-i",
		"--network", "none",
		"--read-only",
		"--tmpfs", "/tmp",
		"-v", spec.Workdir + ":" + containerWorkdir + ":rw",
		"--user", sandboxUser,
		"--pids-limit", pidsLimit,
		"--memory", memoryLimit,
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--workdir", containerWorkdir,
		"--name", spec.ContainerName,
		"--label", "kahya.task_id=" + spec.TaskID,
	}
	names := make([]string, 0, len(spec.EnvPairs))
	for name := range spec.EnvPairs {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic argv for tests/logs
	for _, name := range names {
		args = append(args, "-e", name+"="+spec.EnvPairs[name])
	}
	args = append(args, spec.ImageTag, "/bin/sh")
	return args
}

// containerNameFor builds a readable-but-unique container name so
// concurrent tasks never collide and `docker ps` output stays legible.
func containerNameFor(taskID string) string {
	suffix := randHex(6)
	safeTask := sanitizeForContainerName(taskID)
	if safeTask == "" {
		return "kahya-sandbox-" + suffix
	}
	return "kahya-sandbox-" + safeTask + "-" + suffix
}

// sanitizeForContainerName keeps only [A-Za-z0-9_-] (Docker's own
// container-name charset) and caps length — taskID is not attacker-hostile
// on today's path, but this function fails safe regardless.
func sanitizeForContainerName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is not something this codebase can
		// meaningfully recover from anywhere else either — fall back to a
		// timestamp so a container name is still produced (mirrors
		// mcp/fs's identical randHex fallback rationale).
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}

// LoadPinnedDigest reads docker/sandbox/IMAGE_DIGEST's committed pin: the
// first non-blank, non-"#"-comment line, trimmed. A missing file, or a
// file with no such line (e.g. the placeholder committed before the image
// was ever built), returns ("", nil) — an empty PinnedDigest IS Runner's
// fail-closed state (a real digest is never empty), so callers do not
// need to special-case "not built yet" separately.
func LoadPinnedDigest(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("shell: read %s: %w", path, err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return line, nil
	}
	return "", nil
}

// logAndLedger records kind/payload BOTH ways every shell operation must
// be observable (mirrors mcp/fs's identical helper, duplicated here since
// mcp/fs's own copy is unexported — see this package's doc comment on why
// PolicyClient/Ledger/Logger are aliased instead of reimplemented, and
// contrast with why this one small helper is NOT worth aliasing/exporting
// across the two packages): the append-only DB ledger (best-effort — a
// ledger write failure is logged but never fails the caller's own
// operation) AND a JSONL line under traceID's own scope.
func logAndLedger(ctx context.Context, ledger Ledger, log Logger, traceID, kind string, payload map[string]any) {
	if log == nil {
		log = noopLogger{}
	}
	scoped := log.With(traceID)
	if ledger != nil {
		if err := ledger.LogEvent(ctx, traceID, kind, payload); err != nil {
			scoped.Warn(kind+"_ledger_error", "err", err.Error())
		}
	}
	scoped.Info(kind, mapToArgs(payload)...)
}

// mapToArgs flattens payload into the alternating key/value... variadic
// shape Logger.Info/Warn/Error expects (mirrors mcp/fs's identical
// helper). Map iteration order is unspecified, which is fine here — JSON
// object key order carries no meaning, only which keys/values are present
// does.
func mapToArgs(payload map[string]any) []any {
	args := make([]any, 0, len(payload)*2)
	for k, v := range payload {
		args = append(args, k, v)
	}
	return args
}
