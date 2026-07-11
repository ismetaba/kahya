# W3-04 — Docker shell tool: runtime, sandbox image, mount policy, network=none

**Status:** done — all acceptance criteria pass LIVE (docker/colima was up during this task's execution; see the closing note below).
**Phase:** W3 — Policy + tools
**Depends on:** W3-02
**Flags:** long-running
**Handoff refs:** §5 safety #6 ⚑ shell, §5 safety #1 ⚑ container egress, §6 W3

## Goal
All model-written shell runs inside Docker by default: a pinned sandbox image, a mount policy that rw-binds only the task's explicit workdir, and `--network none` unless a job is explicitly routed through the egress proxy (W3-05). A narrow, argument-validated host-exec set exists for the few commands that must touch the host. Includes the Docker runtime setup itself (colima or Docker Desktop).

## Context you need
The shell invariant (HANDOFF §5 safety #6):

> **Shell:** ikili-allowlist güvenlik sınırı **değil** (`git -c`, `find -exec`, `tar --checkpoint-action` = keyfi kod). Tüm model-yazımı shell **varsayılan Docker'da** (yalnız görevin açık iş-dizini rw bind-mount, gerisi yok/ro; ağ kapalı); host yürütme yalnız arg-doğrulamalı dar bir set.

The container-egress corollary (HANDOFF §5 safety #1):

> ⚑ **Model-yazımı shell konteyneri varsayılan `--network none`;** ağ gerektiren işler yalnız kahyad'ın egress proxy'si (allowlist + hacim bütçesi aynı noktada) üzerinden çıkar — aksi hâlde container içi `curl` allowlist'i atlar.

§6 W3 names the sub-items: "shell`(Docker: **runtime kurulumu — colima/Docker Desktop — + sandbox imajı + mount politikası + `network=none`**)". Tool class: `shell_docker` and `shell_host` are registered W2 in policy.yaml (W3-01), so at MVP autonomy levels every execution needs an approval token (W3-02) with a WYSIWYE diff of the script bytes (W3-06 — until it lands, bind the token to the raw command bytes' SHA-256). Network-approved jobs attach to the internal Docker network created by W3-05; this task creates the attachment mechanism, W3-05 creates the network + proxy.

Machine context (§3): Apple Silicon (M5 Max), so prefer **colima** (`brew install colima docker`) — scriptable, no GUI license; document Docker Desktop as the fallback.

Gotchas:
- colima's VM boundary is what actually isolates the container from macOS; the flags in step 3 defend against in-VM escalation and accidental mounts. Never mount the Docker socket into a sandbox container.
- Bind-mount paths must be the CANONICAL host path (W3-03 `paths.go`): symlinked workdirs otherwise mount more than approved.
- The container clock/arch is linux/arm64 — scripts expecting macOS tools (`say`, `osascript`, `pbcopy`) fail by design; that is correct behavior, not a bug to fix with more mounts.

## Deliverables
- `docker/sandbox/Dockerfile` — pinned base (e.g. `debian:bookworm-slim@sha256:<digest>`), non-root user `kahya` (uid 1000), coreutils/git/python3/jq only; image tag `kahya-sandbox:<version>`.
- `docker/README.md` — colima install/start commands + Docker Desktop fallback (≤30 lines).
- `mcp/shell/server.go` — `shell_docker` MCP tool (kahyad-owned, same in-process pattern as `mcp/fs`).
- `mcp/shell/hostexec.go` — `shell_host` arg-validated narrow set.
- `mcp/shell/runner.go` — container lifecycle: create/run/timeout/kill, log capture to JSONL with `trace_id`.
- Makefile targets: `make sandbox-image` (build + pin), `make docker-up` (start colima if not running).
- Tests: `mcp/shell/runner_test.go`, `mcp/shell/hostexec_test.go`.

## Steps
1. Runtime setup: `brew install colima docker docker-buildx` then `colima start --cpu 4 --memory 8 --vm-type vz`. kahyad health-checks `docker info` at startup; if unavailable, `shell_docker` requests return a clean Turkish error (`Docker çalışmıyor — 'make docker-up' ile başlatın`) and the task is not silently retried.
2. Write the Dockerfile; build and record the image digest in `docker/sandbox/IMAGE_DIGEST` (committed). Runner refuses to start containers whose image digest differs from the committed one (supply-chain pin).
3. `runner.go` — invocation contract: input `{script, workdir, timeout_s, needs_network(bool), env_allowlist}`. Flags (non-negotiable): `--network none` (default), `--read-only`, `--tmpfs /tmp`, `-v <workdir>:/work:rw` (ONLY the task's explicit workdir; nothing else mounted — "gerisi yok/ro" means default to *no* other mounts), `--user 1000:1000`, `--pids-limit 256`, `--memory 2g`, `--cap-drop ALL`, `--security-opt no-new-privileges`. Workdir path is canonicalized with the W3-03 `paths.go` helper and must NOT match any `fs_write_deny_globs`.
4. `needs_network: true` path: attach to the internal network `kahya-egress` (created by W3-05) instead of `none`, inject `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` env pointing at the proxy sidecar; NEVER attach to the default bridge. If W3-05 has not landed yet, `needs_network: true` is rejected outright (fail-closed).
5. Gate chain before every run: `/policy/check` for `shell_docker` (W2) → approval token consumed → run → ledger event `{event:"shell_exec", image_digest, workdir, exit_code, bytes_out, trace_id}`.
6. `hostexec.go` — the narrow host set, each with a strict arg validator (no shell interpolation; `exec.Command` with fixed argv): `git` (subcommands `status|log|diff|show` only, repo path canonicalized, no `-c`, no `--exec-path`), `ls`, `stat`. Anything else ⇒ DENY with ledger event. This list is intentionally boring; growth requires editing Go code, not config. `shell_host` is W2: the FULL step-5 gate chain applies to it too (`/policy/check` for `shell_host` → approval token consumed → exec → ledger event `hostexec_exec` with argv + `trace_id`); the arg validator runs BEFORE the policy check so a denied argv can never mint an approval (same principle as W3-03's deny-glob-before-approval).
7. Timeout/kill: hard `timeout_s` (default 300); on timeout `docker kill`; on kahyad shutdown, kill all containers labeled `kahya.task_id` (label every container).
8. Tests: mount policy — script `cat /etc/passwd` works (image file) but `ls /Users` fails (not mounted); write outside `/work` fails (read-only root); default network — `getent hosts example.com || curl --max-time 3 https://example.com` exits non-zero; digest pin — runner rejects a mismatched digest; hostexec — `git -c core.pager=evil log` rejected, `find` rejected entirely; container labels present.

## Acceptance criteria
- [x] `make sandbox-image` builds; `docker images --digests kahya-sandbox` matches `docker/sandbox/IMAGE_DIGEST`. **PASSED LIVE**: built `kahya-sandbox:0.1.0`, both the `docker images --digests` column and the committed `IMAGE_DIGEST` file read `sha256:bbc17a38ab244481bb14f06be4ec15cce98b4c2d4c3647289276216a91c16a4e`.
- [x] `go test ./mcp/shell/...` green in `make test` (tests that need the Docker daemon fail — not skip — when it is down, so the gate can't silently pass; guard them behind `KAHYA_DOCKER_TESTS=1` exported by `make test` when `docker info` succeeds). **PASSED**: `make test` auto-detected the running daemon, exported `KAHYA_DOCKER_TESTS=1`, and `mcp/shell`'s container tests (`container_test.go`) ran for real and passed.
- [x] Manual: a `shell_docker` run with default flags cannot reach the network — `docker run` transcript in JSONL logs shows `--network none`, and in-container `curl https://api.telegram.org --max-time 3` exits non-zero (this becomes the automated W3-10 bypass test). **PASSED LIVE** (promoted to an automated test, `TestLive_DefaultNetworkNoneBlocksEgress`): `curl` is intentionally not installed in the minimal image (coreutils/git/python3/jq only per this task's own deliverable), so the live evidence instead used `getent hosts api.telegram.org` (exit 2, "nodename nor servname provided") and `python3 -c "urllib.request.urlopen(...)"` (`socket.gaierror: Temporary failure in name resolution`) — both prove real DNS/network unreachability under `--network none`, not merely a missing binary.
- [x] Manual: script writing to `/work/out.txt` succeeds and appears in the host workdir; script writing to `/etc/x` fails. **PASSED LIVE** (`TestLive_MountPolicyAndReadOnlyRoot`): `/etc/passwd` readable, `/Users` not mounted, `/work/out.txt` round-trips to the host, `/etc/kahya_test` write fails ("Read-only file system").
- [x] `shell_host` with `git -c ...` or `tar --checkpoint-action=...` is denied and a `hostexec_denied` ledger event exists (test). **PASSED** (unit test + live curl against the real daemon/brain.db).
- [x] `shell_host` with a VALID argv (`git status`) but no consumed approval token does not execute — the gate chain, not the validator, is the boundary (test with stub executor asserting zero invocations). **PASSED** (`TestHandle_ValidArgvNoApprovalNeverExecutes`).
- [x] Every container run produces a `shell_exec` events row carrying the task's `trace_id` (verify via `sqlite3 brain.db`). **PASSED LIVE**: ran the real `bin/kahyad` against a scratch `brain.db`, called `shell_docker` over the real `/v1/mcp` HTTP-over-UDS endpoint, and `sqlite3 brain.db "select trace_id,kind,payload from events where kind='shell_exec'"` returned exactly one row with the request's `trace_id`, `image_digest`, `workdir`, `exit_code`, `bytes_out` all present.

Closing note: docker/colima was already up by the time this task ran, so every criterion above was verified LIVE, not left pending. One deviation worth flagging: `docker images --digests` normally shows `<none>` for a purely local (never-pushed) build — this environment's buildx/containerd configuration happened to populate a real manifest-list digest anyway, so no fallback to the documented image-ID pin (`dockerDigestChecker`'s doc comment in `mcp/shell/runner.go`) was actually needed, though that fallback remains the production-safe behavior if a different Docker configuration ever produces `<none>` again. A second deviation: the sandbox image deliberately excludes `curl` (not in the coreutils/git/python3/jq allowlist), so the network-blocked live check used `getent`/`python3 urllib` instead of the spec's literal `curl` example — same property (DNS/connect failure under `--network none`), different binary.

## Out of scope
- The egress proxy + `kahya-egress` internal network + volume budgets — W3-05 (this task only consumes them).
- WYSIWYE script-diff rendering — W3-06.
- AppleScript/JXA/Shortcuts (same "arbitrary code" class) — W3-09.
- Virtualization.framework VM isolation, Endpoint Security — deferred (§8).
- Expanding the host-exec set beyond step 6 — requires a design conversation, not this task.
