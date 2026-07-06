# W4-01 — Scheduler: launchd wall-clock jobs + in-daemon ticks

**Status:** todo
**Phase:** W4 — Durability
**Depends on:** W12-01
**Flags:** none
**Handoff refs:** §4 stack ⚑ scheduling, §6 W4, §7 (macOS izin checklist ⚑)

## Goal

kahyad gains a two-tier scheduler: wall-clock jobs (nightly backup, 08:30 briefing, consolidation)
are driven by launchd `StartCalendarInterval` plists that kahyad itself installs and syncs, and a
separate in-daemon `robfig/cron/v3` tick facility exists for short-interval internal work
(outbox scans, anchor pushes, idle-TTL checks). After this task, W4-05/W4-06/W5-01/W5-02 only
declare jobs — they never touch launchd or cron directly.

## Context you need

Binding decision (HANDOFF §4, stack table, quote verbatim):

> ⚑ Duvar-saati işleri (08:30 brifingi, gecelik konsolidasyon) **launchd `StartCalendarInterval`** ile (uykuda kaçarsa uyanışta bir kez koşar); daemon-içi `robfig/cron` yalnız daemon-çalışırken kısa-aralık iç tick'ler için (Go darwin monotonic saati uykuda durur, golang/go#24595 → duvar-saati işlerine güvenme)

- Rationale: Go's darwin monotonic clock stops during sleep (golang/go#24595), so an in-daemon
  cron silently misses wall-clock times. launchd `StartCalendarInterval` coalesces intervals
  missed during sleep into exactly one run on wake — which is the behavior the ⚑ demands.
- W12-01 gives you: kahyad under launchd (`KeepAlive=true`, label `com.kahya.kahyad`), an
  HTTP-over-UDS server on `~/Library/Application Support/Kahya/kahyad.sock`, JSONL logging with
  `trace_id`, and a YAML config file in `~/Library/Application Support/Kahya/`.
- The §5 safety #6 fs write-deny glob `~/Library/LaunchAgents/**` binds the **model-facing fs
  tool (W3-03)**, not kahyad's own trusted job-install code path. kahyad writing its own plists
  there is correct and intended.
- launchd jobs must be LaunchAgents in the user GUI session (`gui/$UID`), never LaunchDaemons —
  TCC grants attach to the responsible process (§7 checklist).

## Deliverables

- `kahyad/internal/scheduler/scheduler.go` — job registry, trigger dispatch, tick API
- `kahyad/internal/scheduler/launchd.go` — plist render + `launchctl bootstrap/bootout` sync
- `kahyad/internal/scheduler/plist.tmpl` — LaunchAgent template
- `kahyad/internal/scheduler/scheduler_test.go`, `launchd_test.go` (golden plist test)
- `kahyad/cmd/kahya-trigger/main.go` — tiny binary launchd runs; POSTs to kahyad over UDS
- Config schema extension (same config file as W12-01): a `jobs:` section
- New UDS endpoint on kahyad: `POST /jobs/trigger/{name}`

## Steps

1. Extend kahyad config with a `jobs:` list. Each entry:
   `name` (DNS-label chars only), `calendar` (map mirroring `StartCalendarInterval` keys:
   `Minute`, `Hour`, `Day`, `Weekday`), and `handler` (name of a Go handler registered in code).
   Jobs used later: `backup-nightly` (W4-06), `memory-push` (W4-06), `briefing` (W5-01),
   `consolidation` (W5-02). Ship a built-in `smoke` handler (writes one ledger event) for tests.
2. Write `plist.tmpl`: label `com.kahya.job.<name>`, `ProgramArguments` =
   `[<abs path to kahya-trigger>, <name>]`, `StartCalendarInterval` from config,
   `StandardOutPath`/`StandardErrorPath` under `~/Library/Logs/Kahya/job-<name>.log`.
   **All paths in the rendered plist must be absolute** — launchd does not expand `~` —
   and the sync step (step 5) creates `~/Library/Logs/Kahya/` if missing.
3. Implement `kahya-trigger`: connect to the UDS socket, `POST /jobs/trigger/{name}` with a 10s
   timeout, print the JSON response, exit 0 on any 2xx (the endpoint responds 202, step 4) /
   non-zero otherwise. No other logic — the daemon does all work so a single code path handles
   manual and scheduled triggers.
4. Implement `POST /jobs/trigger/{name}` in kahyad: mint a fresh `trace_id`, append ledger
   events `job.triggered` and later `job.completed`/`job.failed` (with `job_name`, `trace_id`),
   run the registered handler asynchronously, respond 202 with `{"trace_id": ...}`.
   Unknown job name → 404. All log lines JSONL with the minted `trace_id`.
5. Implement startup sync in `launchd.go`: for every declared job, render the plist to
   `~/Library/LaunchAgents/com.kahya.job.<name>.plist`; if content changed or new, run
   `launchctl bootout gui/$UID/com.kahya.job.<name>` (ignore "not loaded" errors) then
   `launchctl bootstrap gui/$UID <plist path>`. Remove plist + bootout for jobs no longer in
   config. Must be idempotent; a `-sync-jobs` kahyad flag runs sync once and exits.
6. Implement the tick API: `scheduler.RegisterTick(name string, spec string, fn func(ctx))`
   wrapping `robfig/cron/v3` (already in the stack, §4/§9 — do not add another cron lib).
   Doc-comment the hard rule: ticks are for short-interval work while the daemon runs; any
   wall-clock semantics MUST be a launchd job. Ticks log JSONL with a per-run `trace_id`.
7. Tests: golden-file test for a rendered plist; trigger-endpoint test (in-process UDS server:
   known job → 202 + `job.triggered` event row; unknown → 404); tick test (100ms tick fires
   ≥3 times within 1s using a real cron instance).

## Acceptance criteria

- [ ] `make test` green, including `kahyad/internal/scheduler` tests (golden plist, trigger
      endpoint, tick cadence).
- [ ] With a `smoke` job declared (`Minute: 0` any hour), after `kahyad -sync-jobs`:
      `launchctl print gui/$(id -u)/com.kahya.job.smoke` exits 0 and shows the trigger binary.
- [ ] `launchctl kickstart gui/$(id -u)/com.kahya.job.smoke` then
      `sqlite3 ~/Library/Application\ Support/Kahya/brain.db "SELECT kind FROM events WHERE json_extract(payload,'$.job_name')='smoke' ORDER BY id DESC LIMIT 2;"`
      returns `job.completed` and `job.triggered`.
- [ ] The JSONL daemon log contains lines for that run whose `trace_id` equals the one in the
      `job.triggered` event payload (grep to verify — README "single trace_id" convention).
- [ ] Removing the `smoke` job from config and re-running `kahyad -sync-jobs` deletes
      `~/Library/LaunchAgents/com.kahya.job.smoke.plist` and `launchctl print` now fails.
- [ ] `./kahya-trigger no-such-job` exits non-zero and prints the 404 body.

## Out of scope

- Job **content**: backups (W4-06), anchor cadence (W4-05 uses the tick API), briefing (W5-01),
  consolidation (W5-02).
- Catch-up for wall-clock jobs missed while the *daemon* was dead (launchd `KeepAlive=true`
  makes that window negligible; launchd already coalesces sleep-missed runs on wake).
- Embedded NATS or any queue beyond the existing outbox (§8 deferred).
- Hammerspoon/palette wiring (W6) and any Telegram delivery (W3-07 already owns the bot).
