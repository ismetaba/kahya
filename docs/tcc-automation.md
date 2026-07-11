# TCC Automation grants (W3-09)

`applescript_run`/`jxa_run`/`shortcuts_run` send Apple events, which need
per-target-app **Automation** permission granted to kahyad's *responsible
process* — running exactly as **launchd** will run it in production,
never an ad-hoc terminal invocation (TCC keys grants to process identity).

**Apps needed for the MVP:** Finder (acceptance script: `make new folder
at desktop`). Add a row here the first time a script targets a new app.

**Trigger the dialog (must be launchd, must be daytime):**
1. `make build && make install-agent` (LaunchAgent points at real signed `bin/kahyad`).
2. `launchctl kickstart -k gui/$(id -u)/com.kahya.kahyad`
3. Send `applescript_run` targeting the app (e.g. `tell application "Finder" to make
   new folder at desktop`) via `kahya`, approve the WYSIWYE diff.
4. macOS shows the Automation dialog the **first time only**, attributed to kahyad's
   launchd process — click Allow. An unattended 03:00 run never sees this dialog
   (fail-closed), so the grant must happen once, daytime, per target app.

**Verify:**
- `System Settings > Privacy & Security > Automation > kahyad` — target app checked.
- Or (needs Full Disk Access): `sqlite3 "$HOME/Library/Application Support/com.apple.TCC/TCC.db" \
  "select client,auth_value from access where service='kAEEventAppleEvents'"` —
  `auth_value=2` means granted.
- Never call `tccutil reset` from production code paths.
