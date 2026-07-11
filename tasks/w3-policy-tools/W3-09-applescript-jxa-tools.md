# W3-09 — AppleScript/JXA/Shortcuts MCP tools (≥W2, WYSIWYE-gated)

**Status:** blocked-user — code + unit tests + `docs/tcc-automation.md` + loader floor are done and green (`make build && make test && make lint`); the two live "Manual" acceptance criteria below (a real Finder folder created via `applescript_run` under launchd, and the actual TCC Automation grant) need the user to click through a real macOS permission dialog and cannot be faked or automated. See the note under Acceptance criteria.
**Phase:** W3 — Policy + tools
**Depends on:** W3-06, W3-04
**Flags:** slidable, user-assist
**Handoff refs:** §5 safety #6 ⚑ osascript, §6 timing note (may slide to W4–5), §7 TCC checklist ⚑

## Goal
MCP tools for AppleScript, JXA, and Shortcuts execution, treated with the same suspicion as shell: statically classed ≥W2, script bytes approved via a WYSIWYE byte-exact diff, and bodies containing `do shell script`/`doShellScript` rejected or routed to the Docker shell tool.

## Context you need
The binding rule (HANDOFF §5 safety #6):

> ⚑ **`osascript`/JXA/Shortcuts gövdeleri shell ile aynı 'keyfi kod' sınıfıdır** — statik etiketi **en az W2**, script baytları WYSIWYE diff'iyle onaylanır; `do shell script`/`doShellScript` içeren gövdeler reddedilir veya Docker-shell aracına yönlendirilir.

Schedule pressure (HANDOFF §6 timing note): "W3 en riskli hafta olduğundan AppleScript/JXA/Shortcuts W4–W5'e kaydırılabilir." — hence the slidable flag; it must still land before W7-8 acceptance.

TCC (HANDOFF §7): the worker/kahyad process sending Apple events needs per-target-app **Automation** permission, and grants bind to the *responsible process* — "her W3/W6 aracını **launchd altında** test et, terminalden değil; Automation diyaloglarını **gündüz elle** tetikleyip onayla, gece 03:00 rutini diyaloğu göremez". That is the user-assist part: dialogs must be manually triggered and approved during the day, once per target app.

Building blocks you consume: `policy.yaml` registration `osascript` (W2, `reversible: false`) from W3-01; approval tokens + `/policy/consume-token` (W3-02); WYSIWYE canonical payload kind `osascript` and diff renderer (W3-06); Docker shell tool for rerouting shell-bearing bodies (W3-04). These scripts run ON THE HOST (that is their purpose — driving Mac apps), which is exactly why the gate is byte-exact approval of the script source, not sandboxing.

Gotchas:
- The static scanner is a REJECT filter, not a safety proof — like the shell binary-allowlist (§5 #6), it is not a security boundary. The security boundary is the byte-exact human approval; the scanner just refuses the obviously shell-shaped class the handoff names.
- `osascript` exit code 1 covers both script errors and user-cancelled dialogs; parse stderr for the `(-1743)` Automation-denied marker specifically.
- `shortcuts` CLI exists from macOS 12+; check `command -v shortcuts` at tool registration and disable `shortcuts_run` cleanly if absent.
- Timeout: Apple events can hang on a modal dialog in the target app — enforce a hard 120s timeout, kill the `osascript` process group, ledger `osascript_timeout`.

## Deliverables
- `mcp/osascript/server.go` — kahyad-owned MCP tools: `applescript_run`, `jxa_run`, `shortcuts_run`.
- `mcp/osascript/scan.go` — static body scan: shell-escape detection + size/charclass limits.
- `mcp/osascript/scan_test.go`, `mcp/osascript/server_test.go`.
- policy.yaml additions (if not present from W3-01): `applescript_run`, `jxa_run`, `shortcuts_run` — all `class: W2`, `reversible: false`. Loader-enforced floor: these three tool names may never be registered below W2 (add the check to `kahyad/internal/policy/loader.go` + a test).
- `docs/tcc-automation.md` — per-app Automation grant checklist (≤25 lines): which apps, how to trigger the dialog under launchd, how to verify with `tccutil`/System Settings.

## Steps
1. `scan.go`: reject bodies containing (case-insensitive, whitespace-tolerant) `do shell script`, `doShellScript`, `NSTask`, `NSAppleScript`, or `current application's` + `Task` patterns; also reject scripts > 32KB and scripts containing bidi/zero-width code points (reuse `kahyad/internal/canon`). Rejection response includes the Turkish hint: `Kabuk komutu içeren script reddedildi — Docker shell aracını kullanın.` and, when the body is PURELY a shell invocation wrapper, a structured `reroute: shell_docker` suggestion with the extracted command (the model/worker decides to re-request via `shell_docker`; no silent auto-rerouting of code the user never saw).
2. `applescript_run`: gate chain — scan → `/policy/check` (`applescript_run`, W2) → WYSIWYE approval payload kind `osascript` (full script bytes, target app name in the summary) → consume token with hash of the EXACT bytes about to run → execute `osascript -e` via stdin (`osascript - <<'EOF'` pattern avoids argv mangling; pass bytes verbatim) → capture stdout/stderr → ledger event `{event:"osascript_exec", lang:"applescript", target_app, exit_code, trace_id}`.
3. `jxa_run`: same chain with `osascript -l JavaScript -`; scanner also rejects `ObjC.import('Foundation')` combined with `NSTask`.
4. `shortcuts_run`: runs a NAMED existing shortcut (`shortcuts run <name> [--input-path <file>]`) — arbitrary shortcut creation is out; the approval payload is the shortcut name + canonicalized input path. Scanner note: shortcut bodies are opaque to us, which is why only user-created, named shortcuts run, and the name+input is what gets approved.
5. TCC handling: detect Automation-denied errors (`-1743` / `errAEEventNotPermitted`) and return the Turkish message `Otomasyon izni gerekli: <app> — docs/tcc-automation.md adımlarını izleyin` with `Status: blocked-user` semantics for the calling task. Write `docs/tcc-automation.md`; test at least one grant (e.g. Finder) with kahyad running **under launchd**, not from a terminal.
6. Tests: scanner fixtures — `do shell script "rm -rf ~"` rejected; `do  shell   script` (extra whitespace) rejected; `tell application "Finder" to get name of every window` passes scan; JXA `doShellScript` rejected; byte-mutation between approval and execution rejected (reuse W3-06 harness); policy floor test — a fixture policy.yaml with `applescript_run: class: W1` fails to load; ledger event emitted with `trace_id`.

## Acceptance criteria
- [x] `go test ./mcp/osascript/...` green in `make test` (execution tests behind `KAHYA_OSASCRIPT_TESTS=1` — a real-`osascript`, no-target-app, no-TCC-needed smoke test in `mcp/osascript/live_test.go`; scanner + gate-chain + timeout tests unconditional with a stub executor).
- [x] Scanner test matrix green, including whitespace-variant `do shell script` and JXA `doShellScript` fixtures (`mcp/osascript/scan_test.go`).
- [x] Loader floor test green: registering any of the three tools below W2 fails policy load (`kahyad/internal/policy/loader.go`'s `osascriptFloorTools` + `TestLoadRejectsOsascriptToolBelowW2Floor`, fixture `kahyad/internal/policy/testdata/invalid_osascript_below_w2.yaml`).
- [ ] **DEFERRED — needs the user.** Manual: `applescript_run` with `tell application "Finder" to make new folder at desktop` → WYSIWYE diff of the script bytes shown on the approval surface, approve, folder appears; ledger `osascript_exec` row carries the `trace_id`; run it via kahyad under launchd (not a terminal) to bind the TCC grant correctly. Code path is fully implemented and unit-tested with a stub executor (`mcp/osascript/runner_test.go`'s happy-path tests) and with the REAL `osascript` binary via a no-target-app script (`live_test.go`) — only the real Finder+launchd+TCC run itself is deferred, since it requires a human to click "Allow" on a live Automation dialog.
- [x] A body containing `do shell script "whoami"` is rejected with the Turkish reroute hint; nothing executes — unit-tested with the stub executor (`TestRunApplescriptScanRejectedNeverConsultsPolicy`, `TestCallToolShellShapedScriptRejectedOverWire`: zero `Executor.Run` calls, asserted directly). The "manually via `log stream`" half of this criterion is folded into the same deferred TCC/launchd manual pass below, since it needs a live kahyad instance to observe.
- [ ] **DEFERRED — needs the user.** `docs/tcc-automation.md` committed (done — see the file) and followed once for at least one app (screenshot or `System Settings > Privacy & Security > Automation` entry noted in the task log) — this is the live TCC grant itself, see Status above.
- [x] Timeout test green (stub executor that sleeps): process group killed at the limit, `osascript_timeout` ledger event with `trace_id` exists (`TestRunApplescriptTimeoutKillsAndLedgers`, `TestRunShortcutTimeoutKillsAndLedgers`; the REAL process-group kill mechanics are additionally proven against a real `sleep` subprocess in `TestProcessGroupExecutorKillsOnTimeout`).
- [x] `shortcuts_run` approval payload contains the shortcut name + canonical input path and nothing else (test asserts the serialized payload bytes) — `kahyad/internal/approval/payload_test.go`'s `TestBuildShortcut_PayloadContainsOnlyNameAndInputPath` decodes the length-prefixed `CanonicalBytes` and asserts exactly 3 fields; `mcp/osascript/shortcuts_test.go`'s `TestShortcutsRunApprovalToolInputContainsOnlyNameAndInputPath` asserts the same at the tool_input-envelope layer.

## Out of scope
- Sliding decision: if W3 is running late, mark this task `[ ]` and move on — W3-10 does not depend on it (per BACKLOG deps and §6 timing note). It must be done before W7-8.
- Vision computer-use, ekran gözlemi firehose — deferred (§8).
- Creating/editing Shortcuts programmatically — only running named existing shortcuts is in scope.
- Sandboxing host AppleScript execution in a VM (Virtualization.framework) — deferred (§8).
- Generic shell execution — W3-04 owns it; this task only REJECTS or suggests rerouting shell-bearing bodies.
- Automation-permission automation: TCC grants are user-clicked by design; the task ships the checklist doc and clean error surfaces, never `tccutil` reset hacks in production code paths.
