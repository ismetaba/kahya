# TCC checklist — W6 (Hammerspoon palette + local approval cards)

HANDOFF §7 ⚑ rule, verbatim, binds every grant below: TCC keys a grant to
the *responsible process's code identity*, not to whichever terminal
happened to trigger the dialog — **exercise every grant under launchd**
(`launchctl kickstart -k gui/$(id -u)/com.kahya.kahyad` for kahyad;
Hammerspoon's own grants are exercised by Hammerspoon itself, which is
always launchd/loginwindow-started, never a terminal child). Do the
clicking **by hand, during the day** — an unattended nightly run never
sees a TCC dialog at all (fail-closed: it just fails, it never silently
gets a permissive default).

## 1. Hammerspoon: Accessibility, Microphone, Input Monitoring

These three are requested together now (this task) even though only
Accessibility is strictly needed for the ⌥Space palette/approval cards —
W6-02's push-to-talk needs Microphone + Input Monitoring and its own
nightly-adjacent use cannot ever show a first-run dialog either.

1. Install Hammerspoon if not already present: `brew install --cask hammerspoon`,
   then enable "Launch Hammerspoon at login" (Hammerspoon menu bar icon →
   Launch at Login).
2. `make hammerspoon-install` (this repo) — installs `hammerspoon/kahya.lua`
   to `~/.hammerspoon/kahya.lua` (with `@KAHYA_BIN@` resolved to the real
   built `bin/kahya` path) and wires `require("kahya")` into
   `~/.hammerspoon/init.lua`.
3. Hammerspoon menu bar icon → Reload Config (or restart Hammerspoon).
4. Press **⌥Space** once. macOS shows the Accessibility prompt (Hammerspoon
   needs it for `hs.hotkey`/`hs.chooser`) — click **Allow**, then reload
   config again if prompted.
5. System Settings → Privacy & Security → **Microphone** → toggle
   Hammerspoon on (W6-02 dependency — grant it now so a later nightly/
   unattended PTT flow never needs a fresh dialog).
6. System Settings → Privacy & Security → **Input Monitoring** → toggle
   Hammerspoon on (same W6-02 rationale — PTT's global key-down hook).
7. **Notification style — do this or the approval card silently degrades**:
   System Settings → Notifications → Hammerspoon → set **Alerts** (not
   "Banners"). Banner-style notifications auto-dismiss after a few seconds
   and hide the action button (`Görüntüle`) the approval card's whole flow
   depends on — with Alerts, the notification stays on screen until
   dismissed or acted on.

**Verify:**
- System Settings → Privacy & Security → Accessibility / Microphone /
  Input Monitoring — Hammerspoon checked in all three.
- `tccutil` has no query subcommand for a script to introspect cleanly;
  the authoritative check is a live one: press ⌥Space and confirm the
  palette actually opens (Accessibility), and (once W6-02 lands) hold the
  PTT key and confirm mic capture starts (Microphone/Input Monitoring).
- Or, read-only, `sqlite3 "$HOME/Library/Application Support/com.apple.TCC/TCC.db" \
  "select service,client,auth_value from access where client like '%hammerspoon%'"` —
  `auth_value=2` per service row means granted. Never call `tccutil reset`
  from any production code path (manual troubleshooting only).

## 2. Full Disk Access — kahyad (the `fs` tool's owning process)

§7 table row 3: `| korunan dizinleri okuyan fs aracı | Full Disk Access |`.
The `fs` MCP tool (`fs_read`/`fs_write`/`fs_delete`, W3-03) lives **inside
kahyad's own process** — there is no separate "fs tool" binary — so the
"responsible process" the FDA grant binds to is the launchd-started,
codesigned `kahyad` itself. W0-04's stable `Kahya Dev` signing identity is
what keeps this grant from breaking across rebuilds (TCC keys off the
binary's code signature, not its path or mtime); W3-03 deliberately
deferred the grant work itself to this checklist and, until it is granted,
`fs_read` fails cleanly and structurally with `Tam Disk Erişimi gerekli:
<path>` (`mcp/fs.FullDiskAccessError`, `mcp/fs/server.go`) rather than a
generic error or (worse) a silent empty read.

**Grant:**
1. `make build && codesign -f -s "Kahya Dev" bin/kahyad` (or simply `make codesign`,
   which does both) — the binary launchd will actually run must be the
   real, stably-signed one, never an ad-hoc `go run`.
2. `make install-agent` (writes the LaunchAgent plist pointed at that
   binary) if not already installed, then
   `launchctl kickstart -k gui/$(id -u)/com.kahya.kahyad` to restart under
   launchd.
3. System Settings → Privacy & Security → **Full Disk Access** → click
   `+` → navigate to and add the built `bin/kahyad` binary (or
   `~/bin/kahyad` if installed via `make install`) → ensure its toggle is
   on.
4. `launchctl kickstart -k gui/$(id -u)/com.kahya.kahyad` again (TCC does
   not always require a restart for FDA specifically, but this project's
   own convention is: after any grant change, restart under launchd before
   trusting the result — cheap and removes any doubt).

**Verify (must show bytes, not the fail-closed error):**
1. Issue an `fs_read` of a TCC-protected directory through a real task,
   e.g. `kahya ask "~/Library/Mail altındaki dosyaları listele"` (or any
   prompt that drives the worker to call `fs_read` against a path under
   `~/Library/Mail`, `~/Library/Messages`, or similar TCC-gated locations).
2. **Granted**: the tool call returns real directory contents/bytes.
3. **Not granted / revoked**: the tool call's own error is EXACTLY
   `Tam Disk Erişimi gerekli: <path>` (`mcp/fs.FullDiskAccessError.Error()`)
   — never a generic "permission denied", never silently empty results.
4. To rehearse the revoked path deliberately: toggle kahyad off in Full
   Disk Access (System Settings) or run
   `tccutil reset SystemPolicyAllFiles com.kahya.kahyad` (manual
   troubleshooting only — never call this from an automated/production
   path), restart under launchd, and repeat step 1 — confirm the exact
   Turkish message reappears.

## Blocked-user handling (both sections)

If the user has not yet clicked through Accessibility/Microphone/Input
Monitoring/Full Disk Access: per `tasks/README.md`'s protocol, this task's
own `Status:` is set to `blocked-user` for the manual acceptance criteria
that depend on a live grant (the hermetic, code-level acceptance criteria
are unaffected and stay green regardless). Tell the user exactly which
System Settings pane and toggle is missing; **never** weaken `fs_read`'s
own fail-closed `Tam Disk Erişimi gerekli: <path>` response as a
workaround, and never treat an ungranted Hammerspoon permission as "palette
disabled, fall back to something more permissive" — it simply does not
fire until granted.
