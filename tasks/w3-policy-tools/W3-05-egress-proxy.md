# W3-05 — Egress proxy: allowlist + volume budgets for every off-box byte

**Status:** todo
**Phase:** W3 — Policy + tools
**Depends on:** W3-02; W3-03 + W3-04 for the fs sensitive-read hook and the Docker network glue/integration test (the gate + proxy code itself needs only W3-02; backlog order already places W3-03/04 first)
**Flags:** none
**Handoff refs:** §5 safety #1 ⚑ (all three bullets), §4 IPC (forward-proxy), §6 W3

## Goal
kahyad gains an egress proxy that is the single gate for every byte leaving the machine: target allowlist + per-host volume budgets, hard block of allowlist-external egress after a sensitive read in the same session, approval cards counted as egress, and an internal Docker network so network-approved containers can ONLY exit through this proxy.

## Context you need
The invariant, all three bullets binding here (HANDOFF §5 safety #1):

> 1. **Egress birinci-sınıf kapılı bir yetenek.** Off-box'a byte gönderen her çağrı (HTTP gövde *ve* URL, DNS, mail, panoya-uzak) hedef **allowlist** + hacim bütçesine tabi. Aynı oturumda hassas okuma varsa allowlist-dışı egress sert bloke.
>    - ⚑ **Model-yazımı shell konteyneri varsayılan `--network none`;** ağ gerektiren işler yalnız kahyad'ın egress proxy'si (allowlist + hacim bütçesi aynı noktada) üzerinden çıkar — aksi hâlde container içi `curl` allowlist'i atlar.
>    - ⚑ **Onay kartları egress sayılır ve aynı kapıdan geçer.** Allowlist-*içi* ama içerik-taşıyabilen hedefler (Telegram `sendMessage`, `gh` yazma uçları) da hassas-okuma-sonrası içerik kısıtına tabidir.

Related plumbing that already exists: W12-08 built the **Anthropic forward-proxy** (auth header injection + cost governor). Per §4: "Maliyet valisi, cache-hit metriği ve model-çağrısı egress kapısı bu proxy noktasında uygulanır" — so model-call egress gating means: the W12-08 proxy becomes a *client* of this task's allowlist/budget engine rather than a separate hole. Allowlist + budget config comes from `policy.yaml` (W3-01, `egress:` section). "Sensitive read" at W3 = any tool result whose path matched `secret_lane_globs` or content the W3-08 pre-classifier labeled secret-lane; kahyad tracks a per-task/session `sensitive_read=true` flag (in-memory + ledger event; durable taint persistence is W4-03).

Gotchas:
- CONNECT tunnels are opaque after the handshake: the gate decision is on host:port BEFORE connecting; byte metering counts tunneled bytes both directions. You cannot inspect HTTPS bodies and you don't need to — allowlist + budget + sensitive-read flag operate on host + volume.
- Budget "day" = local wall-clock day (Europe/Istanbul-agnostic: use the Mac's local zone via `time.Now().Format("2006-01-02")`); document it in `budget.go`.
- colima needs `--network-address` or the `host.docker.internal` mapping verified — assert reachability from the sidecar in the integration test setup, fail with a clear Turkish message (`Egress ağı kurulamadı`) otherwise.
- The `kahya-egress-fwd` sidecar is dumb TCP forwarding to kahyad's proxy — ALL policy lives in kahyad Go code, never in the sidecar.
- Reject requests whose target is a private/link-local range (RFC1918, 127/8 except kahyad's own listeners, 169.254/16) unless explicitly allowlisted — prevents proxy-as-pivot into the LAN.

## Deliverables
- `kahyad/internal/egress/proxy.go` — HTTP(S) forward proxy (CONNECT tunneling for TLS; decision on host:port before connect) listening on `127.0.0.1:<config egress.port>`.
- `kahyad/internal/egress/gate.go` — the decision engine: allowlist match, budget accounting, sensitive-read block. Exported Go API `egress.Check(ctx, Target, nbytes, sessionInfo)` so in-process callers (Telegram bot W3-07, anchor push W4-05, W12-08 proxy) use the SAME gate as the proxy path.
- `kahyad/internal/egress/budget.go` — per-host daily byte counters persisted in a `egress_budget(host, day, bytes)` table (goose migration, next free number) so restarts don't reset budgets.
- Docker network setup in `mcp/shell` glue: `docker network create --internal kahya-egress` + a `kahya-egress-fwd` sidecar container (pinned `alpine/socat` digest) attached to both `kahya-egress` and the default bridge, forwarding to `host.docker.internal:<egress.port>` — the only route out of the internal network.
- `mcp/fs` wiring (edited in THIS task): the `secret_lane_read` seam W3-03 left in `fs_read` now also calls the sensitive-read endpoint below, setting the session flag.
- Tests: `kahyad/internal/egress/gate_test.go`, `proxy_test.go`, plus a Docker integration test `mcp/shell/egress_integration_test.go`.

## Steps
1. `gate.go`: input = canonical host (lowercased, punycode-decoded, IP-literal rejected unless explicitly allowlisted), port, byte count, direction metadata, session/task IDs. Decision order: (1) session `sensitive_read` && host not in allowlist ⇒ DENY (`egress_blocked_sensitive`); (2) host not in allowlist ⇒ DENY (`egress_blocked_allowlist`); (3) budget exceeded (`default_daily_byte_budget` or per-host override) ⇒ DENY (`egress_blocked_budget`) + Turkish notification `Egress bütçesi aşıldı: <host>`; (4) allow + count bytes. Every decision ⇒ ledger event with `trace_id`.
2. `proxy.go`: plain HTTP requests are inspected (host from URL); HTTPS via CONNECT — gate on host:port, then tunnel while metering both directions' bytes into the budget. DNS: the proxy resolves names itself; clients on `kahya-egress` get no DNS (internal network), so name resolution cannot leak — assert this in the integration test with a raw `getent hosts` failing in-container.
3. Sensitive-read flag: add `POST /session/sensitive-read` (UDS) called by tools when a read matches `secret_lane_globs`. THIS task edits `mcp/fs` to call it from the `secret_lane_read` seam W3-03 left in `fs_read` (W3-08 hooks its content-classifier verdicts when it lands — do not build content classification here). Flag is per `session_id`, **rises only, never clears within the session** (same "taint only rises" direction as §5 #2; durable cross-restart persistence is W4-03); ledger event `sensitive_read_marked`.
4. Approval cards go through the gate: expose `egress.Check` and require the Telegram sender (W3-07) and any future card channel to call it with the rendered card's byte size and host `api.telegram.org`, honoring the sensitive-read content restriction (W3-07 enforces the redaction; this gate enforces deny-on-nonallowlisted and budget).
5. Route W12-08 through the gate: the Anthropic forward-proxy calls `egress.Check(host="api.anthropic.com", nbytes=request_size, session)` before forwarding. A sensitive-read session cannot even reach `api.anthropic.com` if that host were ever off-allowlist; the stronger content rule ("no secret-lane byte to cloud") is W3-08's classifier + W3-10's test.
6. Docker wiring per Deliverables; `mcp/shell` `needs_network` jobs (W3-04 step 4) attach to `kahya-egress` and receive `HTTP_PROXY=http://kahya-egress-fwd:3128` etc. Verify `--internal` blocks direct routes.
7. Tests: unit — allowlist normalization (case, punycode `xn--`, trailing dot), budget rollover at day boundary, sensitive-read block, IP-literal rejection; proxy — CONNECT to allowlisted host succeeds against a local TLS test server entry, non-allowlisted host gets 403 before any upstream connection; integration (Docker) — in-container `curl https://<nonallowlisted>` via proxy env ⇒ 403; direct `curl --noproxy '*' https://1.1.1.1` ⇒ network unreachable/timeout (internal network has no route).

## Acceptance criteria
- [ ] `go test ./kahyad/internal/egress/...` green in `make test`.
- [ ] Docker integration test green under `KAHYA_DOCKER_TESTS=1` (same guard as W3-04): in-container curl cannot bypass the allowlist by any of: direct IP, `--noproxy`, DNS lookup. This is the §6 W3 gate item "container içi `curl` allowlist'i atlayamıyor (test)".
- [ ] Manual: mark a session sensitive (`curl --unix-socket ... /session/sensitive-read`), then request an allowlist-external host through the proxy ⇒ 403 and a ledger row `egress_blocked_sensitive` with the session's `trace_id`.
- [ ] Budget test: set a 1KB budget for a test host in a fixture policy, push 2KB ⇒ second request blocked, `egress_blocked_budget` event exists, counter survives a kahyad restart (persisted row asserted).
- [ ] Grep proves single gate: `grep -rn "http.ProxyFromEnvironment\|net.Dial" kahyad/ mcp/ worker/` shows no off-box dialing outside `kahyad/internal/egress/` and the W12-08 proxy (which calls `egress.Check`). Document the check as a test or lint rule in `make lint`.
- [ ] Private-range pivot test green: proxying to `192.168.1.1` or `169.254.169.254` is denied even though no allowlist entry blocks it explicitly.
- [ ] End-to-end sensitive-read chain (automated test): `fs_read` of a `secret_lane_globs` fixture path ⇒ `sensitive_read_marked` ledger event for the session; a subsequent proxy request from that session to an allowlist-external host ⇒ 403 + `egress_blocked_sensitive`; the flag never clears for the session's lifetime (a later attempt in the same session still blocks).
- [ ] Every gate decision (allow and deny) produces a JSONL log line and an events row carrying `trace_id` — verified by a test counting rows for a scripted sequence.

## Out of scope
- Redaction of secret-lane content in Telegram messages — W3-07 (this gate only meters/permits the channel).
- Content-level secret-lane classification — W3-08.
- Durable taint persistence across resume — W4-03 (here the flag is session-lifetime, ledgered).
- External ledger anchor push — W4-05 (it will call `egress.Check` when it lands).
- Clipboard-to-remote and mail egress interception beyond tools built in this phase — enforced as those tools appear (mail tooling is not an MVP W3 deliverable).
- Endpoint Security network extension / system-wide firewall — deferred (§8); the boundary here is "all Kahya-originated egress", enforced by construction (containers on the internal network, tools built on `egress.Check`).
- Secret-lane redaction rules — W3-07/W3-08 own content; this task owns hosts and bytes.
