-- kahya.lua — Kâhya's Hammerspoon surface (W6-01/W6-02/W6-03, HANDOFF §4
-- UI + IPC + stack STT).
--
-- This file provides five things:
--   1. The ⌥Space command palette (hs.chooser, free-text): capture the
--      hotkey-press timestamp, run `kahya ask --palette-opened-at <t> --
--      <text>`, show the answer via hs.notify.
--   1b. ⌥Space HOLD-TO-TALK (W6-02): the SAME ⌥Space binding, extended
--      with pressedfn/releasedfn - a 300ms hold starts a local ffmpeg
--      capture instead of the text palette; release runs `kahya ask
--      --audio <wav>`, transcribed entirely on-device by mlx-whisper
--      inside the Python worker (never in this file, never in kahyad -
--      HANDOFF §4 ⚑ "mlx-whisper ... worker içinde kütüphane").
--   2. The LOCAL approval-card surface (HANDOFF §5 safety #5 ⚑: "W3 yazılı
--      'onayla' YALNIZ yerel yüzeyden kabul edilir" — from W6 on, THIS is
--      that local surface): kahyaShowApproval(id), invoked by kahyad's own
--      pending-approval hook (kahyad/internal/ui.HSCli.ShowApproval) via
--      `hs -c 'kahyaShowApproval("<id>")'`.
--   3. The generic background/scheduled-task notification path:
--      kahyaNotify(payloadB64), invoked by kahyad/internal/ui.HSCli.Notify
--      / SendNotification via `hs -c 'kahyaNotify("<base64 json>")'`.
--   4. ⌥⎋ EMERGENCY HALT (W6-03, HANDOFF §6 W6 ⚑): `kahyaBin halt` (no
--      --task = every non-terminal task) -> hs.notify "Acil durdurma —
--      tüm görevler durduruldu". Works even while the palette/an approval
--      dialog is open — deliberately W6-01 did NOT bind ⌥⎋, so this task
--      is the first to claim it.
--
-- IMPORTANT — this file is a RENDERING + INPUT surface ONLY. Every binding
-- security decision (hash verification, NFC normalization, bidi/zero-
-- width/homoglyph stripping, one-time approval tokens, the byte-exact
-- "onayla" check) lives in kahyad itself (kahyad/internal/policy.Engine,
-- kahyad/internal/canon, kahyad/internal/approval — W3-02/W3-06/W6-01).
-- This file only ever DISPLAYS what `kahya approvals show <id> --json`
-- returns and forwards the human's decision to `kahya approvals decide`;
-- it never computes, caches, or second-guesses any approval verdict
-- itself.
--
-- Installed by `make hammerspoon-install` (repo Makefile), which copies
-- this file to ~/.hammerspoon/kahya.lua, substituting @KAHYA_BIN@ with the
-- absolute path of the built `kahya` CLI (bin/kahya), and appends
-- `require("kahya")` to ~/.hammerspoon/init.lua if not already present.
--
-- Requires (docs/tcc-w6-checklist.md, do this BEFORE relying on any of
-- the below): Hammerspoon granted Accessibility (hs.hotkey/hs.chooser) +
-- Microphone/Input Monitoring (HANDOFF §7 TCC table: "Hammerspoon |
-- Accessibility (+ PTT için Mikrofon / Input Monitoring)" - the ffmpeg
-- child process captures under whichever process actually owns the mic
-- grant), Hammerspoon's own Notifications set to "Alerts" style in System
-- Settings (banner style auto-dismisses and hides the action button the
-- approval card depends on).

-- hs.task cannot resolve a bare command name via $PATH (Hammerspoon's GUI
-- launch environment has no shell PATH at all) — every hs.task.new call
-- below MUST use this absolute path, never a bare "kahya".
local kahyaBin = "@KAHYA_BIN@"

-- W6-02 push-to-talk constants (task spec step 1/5) — hs.task also cannot
-- resolve these via $PATH, so both are absolute paths, kept as local
-- constants right next to kahyaBin per the task spec's own instruction.
--
-- micDevice: the avfoundation input device index for ffmpeg's `-i`
-- argument (":<index>", audio-only — no video device). Discover it once
-- with `ffmpeg -f avfoundation -list_devices true -i ""` and hardcode the
-- result here; ":0" is the common default (the Mac's built-in mic is
-- usually the first audio device) but is NOT guaranteed on every machine
-- — verify with the list-devices command above before relying on it.
local micDevice = ":0"
-- ffmpegBin: Homebrew's Apple-Silicon prefix (matches kahyaBin's own
-- @KAHYA_BIN@ substitution convention — an absolute path, since hs.task
-- cannot resolve a bare "ffmpeg" any more than it can a bare "kahya").
local ffmpegBin = "/opt/homebrew/bin/ffmpeg"
-- kahyaTmpDir: kahyad's own W6-02 temp-audio directory (config.Config.
-- TmpDir(), created 0700 at kahyad startup) — hammerspoon writes the
-- ffmpeg capture here; kahya_worker's own delete-safety check
-- (kahya_worker.__main__._maybe_delete_audio) only ever deletes a file
-- whose PARENT directory is exactly this one.
local kahyaTmpDir = os.getenv("HOME") .. "/Library/Application Support/Kahya/tmp"

-- pttHoldThreshold: how long ⌥Space must be held (task spec step 5: "if
-- the key is released earlier, open the text palette") before push-to-
-- talk capture starts instead of the W6-01 text palette.
local pttHoldThreshold = 0.3

-- ---------------------------------------------------------------------
-- Small shared helpers
-- ---------------------------------------------------------------------

-- runKahya execs `kahyaBin <args...>`, calling onDone(exitCode, stdOut,
-- stdErr) when it finishes. Every CLI invocation in this file goes
-- through this one helper so argv construction/logging stays in one
-- place.
local function runKahya(args, onDone)
  local task = hs.task.new(kahyaBin, function(exitCode, stdOut, stdErr)
    if onDone then
      onDone(exitCode, stdOut or "", stdErr or "")
    end
  end, args)
  task:start()
  return task
end

-- firstLines returns the first n lines of s, joined back with "\n" — the
-- palette's own completion notification only ever previews the answer;
-- the FULL text is always still reachable via `kahya log --trace <id>`.
local function firstLines(s, n)
  if not s or s == "" then
    return s
  end
  local lines = {}
  for line in string.gmatch(s, "([^\n]*)\n?") do
    table.insert(lines, line)
    if #lines >= n then
      break
    end
  end
  return table.concat(lines, "\n")
end

-- ---------------------------------------------------------------------
-- 1. ⌥Space command palette
-- ---------------------------------------------------------------------

-- kahyaChooser is built once at module load. hs.chooser has no built-in
-- "free text" mode — pressing Enter always selects an existing ROW, it
-- never hands back arbitrary unselected query text on its own. The
-- standard Hammerspoon idiom for a free-text command palette (used here)
-- is to keep exactly ONE synthetic choice that always mirrors whatever
-- the user has typed so far (queryChangedCallback below rewrites it on
-- every keystroke) — Enter therefore always selects THAT row, and its
-- own .query field is read back in the completion callback.
local kahyaPaletteOpenedAt = nil

local function submitPaletteQuery(query)
  if not query or query == "" then
    return
  end
  if not kahyaPaletteOpenedAt then
    -- Should not happen (set at hotkey press, just below) — refuse to
    -- guess a timestamp rather than sending a wrong one.
    kahyaPaletteOpenedAt = hs.timer.secondsSinceEpoch()
  end
  -- %.6f: an explicit, always-decimal (never scientific-notation)
  -- formatting of the captured epoch-seconds float — kahyad's
  -- `--palette-opened-at` flag parses this with Go's strconv.ParseFloat,
  -- which accepts ordinary decimal notation but must never receive
  -- "1.79e+09"-style scientific notation from a large tostring() default.
  local paletteOpenedAtStr = string.format("%.6f", kahyaPaletteOpenedAt)
  kahyaPaletteOpenedAt = nil

  -- "--" terminates flag parsing (Go's flag package convention) so a
  -- query that itself starts with "-" is never misread as a flag.
  runKahya({ "ask", "--palette-opened-at", paletteOpenedAtStr, "--", query }, function(exitCode, stdOut, stdErr)
    local text = stdOut
    if exitCode ~= 0 and stdErr ~= "" then
      text = stdErr
    end
    hs.notify.new({
      title = "Kâhya",
      informativeText = firstLines(text, 5),
      autoWithdraw = true,
    }):send()
  end)
end

local kahyaChooser = hs.chooser.new(function(choice)
  if choice and choice.query and choice.query ~= "" then
    submitPaletteQuery(choice.query)
  end
end)
kahyaChooser:placeholderText("Kâhya'ya yaz…")
kahyaChooser:choices({})
kahyaChooser:queryChangedCallback(function(query)
  if query and query ~= "" then
    -- The one-and-only row: its own .query field (NOT hs.chooser's
    -- unrelated .text/.subText, which are display-only) is what the
    -- completion callback above reads back — text/subText are still set
    -- to something readable so the row itself looks sensible in the UI.
    kahyaChooser:choices({ { text = query, subText = "Kâhya'ya gönder", query = query } })
  else
    kahyaChooser:choices({})
  end
end)

-- ---------------------------------------------------------------------
-- 1b. ⌥Space hold-to-talk (W6-02): press starts a 300ms timer; released
-- before it fires -> the W6-01 text palette above; still held at 300ms ->
-- start an ffmpeg capture, show "🎙️ Dinliyorum…"; release while
-- recording -> terminate ffmpeg (finalizes the wav on SIGTERM) and run
-- `kahyaBin ask --audio <wav> --palette-opened-at <pressTimestamp>`.
-- ---------------------------------------------------------------------

local pttTimer = nil
local pttTask = nil
local pttWavPath = nil
-- pttRecording: true only once the hold threshold has actually fired and
-- an ffmpeg capture is (or was) running - distinguishes "released early,
-- show the palette" from "released after recording started, submit the
-- audio" in the releasedfn below.
local pttRecording = false

-- pttStartRecording is pttTimer's own 300ms callback (task spec step 5):
-- still held at this point (releasedfn would already have stopped the
-- timer otherwise) - start the ffmpeg capture and show the listening
-- notification.
local function pttStartRecording()
  pttRecording = true
  -- Millisecond epoch (not whole seconds): two holds started within the
  -- same second must never collide on the same wav filename.
  local epochMs = string.format("%d", math.floor(hs.timer.secondsSinceEpoch() * 1000))
  pttWavPath = kahyaTmpDir .. "/ptt-" .. epochMs .. ".wav"

  pttTask = hs.task.new(ffmpegBin, nil, {
    "-hide_banner",
    "-f", "avfoundation",
    "-i", micDevice,
    "-ac", "1",
    "-ar", "16000",
    "-sample_fmt", "s16",
    "-y", pttWavPath,
  })
  pttTask:start()

  hs.notify.new({
    title = "Kâhya",
    informativeText = "🎙️ Dinliyorum… (bırakınca gönderilir)",
    autoWithdraw = true,
  }):send()
end

-- pttSubmitRecording is releasedfn's own "was actually recording" branch:
-- terminate the ffmpeg capture (SIGTERM finalizes the wav) and send it to
-- `kahya ask --audio` exactly the way submitPaletteQuery sends typed text,
-- reusing the SAME hs.notify completion surface.
local function pttSubmitRecording()
  local wavPath = pttWavPath
  local pressTimestamp = kahyaPaletteOpenedAt or hs.timer.secondsSinceEpoch()
  pttWavPath = nil
  pttRecording = false

  if pttTask then
    pttTask:terminate()
    pttTask = nil
  end
  local paletteOpenedAtStr = string.format("%.6f", pressTimestamp)
  kahyaPaletteOpenedAt = nil

  runKahya({ "ask", "--audio", wavPath, "--palette-opened-at", paletteOpenedAtStr }, function(exitCode, stdOut, stdErr)
    local text = stdOut
    if exitCode ~= 0 and stdErr ~= "" then
      text = stdErr
    end
    hs.notify.new({
      title = "Kâhya",
      informativeText = firstLines(text, 5),
      autoWithdraw = true,
    }):send()
  end)
end

-- No repeatfn argument: Hammerspoon does not re-invoke pressedfn for the
-- OS's own key-repeat events unless a repeatfn is supplied, so holding
-- ⌥Space down never re-arms pttTimer or restarts a capture already in
-- progress.
hs.hotkey.bind({ "alt" }, "space", function()
  -- pressed - captured HERE, before either the chooser UI or ffmpeg
  -- capture even starts, so the north-star "palet-aç→ilk-token" metric's
  -- start timestamp is the actual moment the user asked for the palette/
  -- began speaking, not the moment they finished typing/talking.
  kahyaPaletteOpenedAt = hs.timer.secondsSinceEpoch()
  pttRecording = false
  pttTimer = hs.timer.doAfter(pttHoldThreshold, pttStartRecording)
end, function()
  -- released
  if pttTimer then
    pttTimer:stop()
    pttTimer = nil
  end
  if pttRecording then
    pttSubmitRecording()
  else
    -- Released before the 300ms threshold fired - the W6-01 text palette
    -- (kahyaPaletteOpenedAt, captured above at press, is reused as-is).
    kahyaChooser:show()
  end
end)

-- ---------------------------------------------------------------------
-- 2. Local approval-card surface
-- ---------------------------------------------------------------------

-- sendApprovalDecision calls `kahya approvals decide <id> --approve
-- --typed <text> | --reject` — the CLI's own client talks to kahyad's
-- POST /approvals/{id}/decision, which stamps surface="local" itself
-- (channel-derived, never read from this Lua file or the CLI's own
-- request body) and, for a W3 approval, verifies `typed` byte-exact
-- against "onayla" server-side. This function never decides anything on
-- its own; it only relays the human's already-made choice.
local function sendApprovalDecision(id, approve, typed)
  local args = { "approvals", "decide", id }
  if approve then
    table.insert(args, "--approve")
    table.insert(args, "--typed")
    table.insert(args, typed or "")
  else
    table.insert(args, "--reject")
  end
  runKahya(args, function(exitCode, stdOut, stdErr)
    if exitCode ~= 0 then
      -- FAIL-CLOSED DELIVERY: nothing auto-approves just because this
      -- exec failed. The pending approval (if the decision never landed)
      -- stays discoverable via `kahya approvals list`; this notice is
      -- purely informational.
      hs.notify.new({
        title = "Kâhya",
        informativeText = "Onay kararı iletilemedi: " .. id,
        autoWithdraw = true,
      }):send()
    end
  end)
end

-- openApprovalDialog pops the class-specific decision dialog for detail
-- (kahyad's own byte-exact W3-06 diff, already rendered server-side —
-- this file never re-renders or re-verifies it, only displays it
-- verbatim).
local function openApprovalDialog(detail)
  local id = detail.id
  local class = detail.class
  local rendered = detail.rendered or ""

  if class == "W3" then
    -- HANDOFF §5 safety #5 ⚑: W3 accepts nothing but the literally typed
    -- word "onayla" — this dialog's instructive text says so verbatim;
    -- the ACTUAL verification of what was typed happens server-side
    -- (kahyad/internal/policy.Engine.Approve), never here.
    local button, typed = hs.dialog.textPrompt(
      "Bu eylem geri alınamaz (W3): " .. detail.tool,
      rendered .. "\n\nOnaylamak için \"onayla\" yazın:",
      "",
      "Onayla",
      "Reddet"
    )
    sendApprovalDecision(id, button == "Onayla", typed)
  else
    local button = hs.dialog.blockAlert(
      string.format("Onay gerekiyor (%s): %s", class, detail.tool),
      rendered,
      "Onayla",
      "Reddet"
    )
    sendApprovalDecision(id, button == "Onayla", nil)
  end
end

-- kahyaShowApproval(id) is kahyad's own pending-approval hook's entry
-- point (kahyad/internal/ui.HSCli.ShowApproval execs
-- `hs -c 'kahyaShowApproval("<id>")'` for EVERY freshly minted pending
-- approval — W1/W2/W3 alike, since this IS the local surface). It fetches
-- the byte-exact diff via `kahya approvals show <id> --json`, pops an
-- hs.notify card, and wires its own action button to openApprovalDialog.
function kahyaShowApproval(id)
  runKahya({ "approvals", "show", id, "--json" }, function(exitCode, stdOut, stdErr)
    if exitCode ~= 0 or stdOut == "" then
      hs.notify.new({
        title = "Kâhya",
        informativeText = "Onay bilgisi alınamadı: " .. id,
        autoWithdraw = true,
      }):send()
      return
    end
    local ok, detail = pcall(hs.json.decode, stdOut)
    if not ok or not detail or not detail.id then
      hs.notify.new({
        title = "Kâhya",
        informativeText = "Onay verisi çözümlenemedi: " .. id,
        autoWithdraw = true,
      }):send()
      return
    end

    local notification = hs.notify.new(function(note)
      if note:activationType() == hs.notify.activationTypes.actionButtonClicked then
        openApprovalDialog(detail)
      end
    end, {
      title = "Kâhya — onay bekliyor",
      informativeText = string.format("%s (%s)", detail.tool or "", detail.class or ""),
      hasActionButton = true,
      actionButtonTitle = "Görüntüle",
      autoWithdraw = false,
    })
    notification:send()
  end)
end

-- ---------------------------------------------------------------------
-- 3. Generic local notification path (background/scheduled task results)
-- ---------------------------------------------------------------------

-- kahyaNotify(payloadB64) is kahyad's own generic notification entry
-- point (kahyad/internal/ui.HSCli.Notify/SendNotification execs
-- `hs -c 'kahyaNotify("<base64 json>")'`; the JSON is base64-encoded so
-- kahyad never has to Lua-escape arbitrary Turkish/notification text
-- itself — see that Go function's own doc comment). payload is
-- {title, message, trace_id}; the FULL result always stays reachable via
-- `kahya log --trace <id>` regardless of whether this notification is
-- ever seen.
function kahyaNotify(payloadB64)
  local raw = hs.base64.decode(payloadB64)
  if not raw or raw == "" then
    return
  end
  local ok, payload = pcall(hs.json.decode, raw)
  if not ok or not payload then
    return
  end
  local traceLine = ""
  if payload.trace_id and payload.trace_id ~= "" then
    traceLine = "\n\niz: " .. payload.trace_id
  end
  hs.notify.new({
    title = payload.title or "Kâhya",
    informativeText = firstLines(payload.message or "", 5) .. traceLine,
    autoWithdraw = true,
  }):send()
end

-- ---------------------------------------------------------------------
-- 4. ⌥⎋ emergency halt (W6-03)
-- ---------------------------------------------------------------------

-- ⌥⎋ -> `kahyaBin halt` (no --task: halts EVERY non-terminal task) ->
-- notify "Acil durdurma — tüm görevler durduruldu" (task spec step 7,
-- byte-exact). Every actual halt decision — process-group SIGKILL, docker
-- kill, the terminal user_halted transition, outbox cancel, approval
-- invalidation + token revocation — happens server-side in kahyad
-- (kahyad/internal/halt.Executor); this binding only fires the CLI call
-- and shows the notification, exactly like every other binding in this
-- file.
--
-- Works while the palette (kahyaChooser) or an approval dialog
-- (hs.dialog.textPrompt/blockAlert in openApprovalDialog) is open:
-- hs.hotkey.bind registers a system-level event tap that Hammerspoon's
-- own run loop still services even while an NSAlert/NSTextField modal
-- session is pumping it — the same reason ⌥Space itself already works
-- regardless of which app is frontmost. No special-casing is needed here;
-- this is an ordinary hs.hotkey.bind exactly like the ⌥Space one above.
hs.hotkey.bind({ "alt" }, "escape", function()
  runKahya({ "halt" }, function(exitCode, stdOut, stdErr)
    hs.notify.new({
      title = "Kâhya",
      informativeText = "Acil durdurma — tüm görevler durduruldu",
      autoWithdraw = true,
    }):send()
    if exitCode ~= 0 then
      -- kahyaBin halt itself only ever exits 0 (task spec: "exit 0 both
      -- ways") - a non-zero exit here means the CLI could not even reach
      -- kahyad (dial failure), which the user should still know about
      -- alongside the notification above.
      hs.notify.new({
        title = "Kâhya",
        informativeText = "Durdurma komutu kahyad'a ulaşamadı: " .. (stdErr ~= "" and stdErr or stdOut),
        autoWithdraw = true,
      }):send()
    end
  end)
end)

-- ---------------------------------------------------------------------
-- CLI install (so `hs -c '...'` from kahyad's own exec bridge works)
-- ---------------------------------------------------------------------

-- Apple Silicon Homebrew prefix, matching config key ui.hs_cli's own
-- default (/opt/homebrew/bin/hs — kahyad/internal/ui.DefaultHsCliPath).
-- The parameterless hs.ipc.cliInstall() installs under /usr/local
-- instead, which would not match that default on an Apple Silicon Mac.
hs.ipc.cliInstall("/opt/homebrew")
