package osascript

import (
	"strings"
	"testing"
)

// TestScanRejectsDoShellScript is this task's spec step 6 fixture,
// verbatim: `do shell script "rm -rf ~"` rejected, WITH a shell_docker
// reroute suggestion (the body is PURELY the wrapper).
func TestScanRejectsDoShellScript(t *testing.T) {
	res := Scan([]byte(`do shell script "rm -rf ~"`))
	if !res.Rejected {
		t.Fatal("Scan(do shell script) = not rejected, want rejected")
	}
	if res.Reason != reasonShellShaped {
		t.Errorf("Reason = %q, want %q", res.Reason, reasonShellShaped)
	}
	if res.Reroute == nil {
		t.Fatal("Reroute = nil, want a shell_docker suggestion for a pure wrapper")
	}
	if res.Reroute.Tool != "shell_docker" {
		t.Errorf("Reroute.Tool = %q, want shell_docker", res.Reroute.Tool)
	}
	if res.Reroute.Command != "rm -rf ~" {
		t.Errorf("Reroute.Command = %q, want %q", res.Reroute.Command, "rm -rf ~")
	}
}

// TestScanRejectsDoShellScriptWhitespaceVariant is this task's spec step
// 6 fixture, verbatim: "do  shell   script" (extra whitespace) rejected.
func TestScanRejectsDoShellScriptWhitespaceVariant(t *testing.T) {
	res := Scan([]byte(`do  shell   script "rm -rf ~"`))
	if !res.Rejected {
		t.Fatal("Scan(whitespace-variant do shell script) = not rejected, want rejected")
	}
	if res.Reroute == nil || res.Reroute.Command != "rm -rf ~" {
		t.Errorf("Reroute = %+v, want a shell_docker suggestion with command %q", res.Reroute, "rm -rf ~")
	}
}

// TestScanRejectsDoShellScriptCaseVariant proves the case-insensitive
// half of "case-insensitive, whitespace-tolerant".
func TestScanRejectsDoShellScriptCaseVariant(t *testing.T) {
	res := Scan([]byte(`Do Shell Script "whoami"`))
	if !res.Rejected {
		t.Fatal("Scan(case-variant DO SHELL SCRIPT) = not rejected, want rejected")
	}
}

// TestScanRejectsEmbeddedDoShellScriptWithNoReroute proves a shell-shaped
// body that is NOT purely the wrapper (wrapped in a tell block) is still
// rejected, but gets NO reroute suggestion (this task's own instruction:
// "no silent auto-rerouting of code the user never saw").
func TestScanRejectsEmbeddedDoShellScriptWithNoReroute(t *testing.T) {
	script := `tell application "Finder"
	do shell script "whoami"
end tell`
	res := Scan([]byte(script))
	if !res.Rejected {
		t.Fatal("Scan(embedded do shell script) = not rejected, want rejected")
	}
	if res.Reroute != nil {
		t.Errorf("Reroute = %+v, want nil for a non-pure-wrapper body", res.Reroute)
	}
}

// TestScanPassesFinderTellApplication is this task's spec step 6 fixture,
// verbatim: `tell application "Finder" to get name of every window`
// passes scan.
func TestScanPassesFinderTellApplication(t *testing.T) {
	res := Scan([]byte(`tell application "Finder" to get name of every window`))
	if res.Rejected {
		t.Fatalf("Scan(finder tell) = rejected (%q), want pass", res.Reason)
	}
}

// TestScanRejectsJXADoShellScript is this task's spec step 6 fixture,
// verbatim: JXA `doShellScript` rejected.
func TestScanRejectsJXADoShellScript(t *testing.T) {
	res := Scan([]byte(`var app = Application.currentApplication(); app.doShellScript("ls -la");`))
	if !res.Rejected {
		t.Fatal("Scan(JXA doShellScript) = not rejected, want rejected")
	}
	if res.Reroute != nil {
		t.Errorf("Reroute = %+v, want nil (not a pure `do shell script \"...\"` wrapper shape)", res.Reroute)
	}
}

// TestScanRejectsNSTask covers the JXA ObjC-bridge NSTask escape hatch
// (this task's spec step 3: "scanner also rejects ObjC.import('Foundation')
// combined with NSTask").
func TestScanRejectsNSTask(t *testing.T) {
	script := `ObjC.import('Foundation'); var task = $.NSTask.alloc().init(); task.launch();`
	res := Scan([]byte(script))
	if !res.Rejected {
		t.Fatal("Scan(ObjC.import + NSTask) = not rejected, want rejected")
	}
}

// TestScanRejectsNSAppleScript covers the nested-interpreter reflection
// escape hatch this task's spec step 1 names.
func TestScanRejectsNSAppleScript(t *testing.T) {
	res := Scan([]byte(`current application's NSAppleScript.alloc().initWithSource_("do shell script \"ls\"")`))
	if !res.Rejected {
		t.Fatal("Scan(NSAppleScript) = not rejected, want rejected")
	}
}

// TestScanRejectsCurrentApplicationTaskCombo covers the "current
// application's" + "Task" combined heuristic WITHOUT the literal
// substring "NSTask" appearing anywhere, proving the combined rule is its
// own independent check, not merely a restatement of the NSTask rule.
func TestScanRejectsCurrentApplicationTaskCombo(t *testing.T) {
	script := `set t to current application's launchTask()`
	if strings.Contains(strings.ToLower(script), "nstask") {
		t.Fatal("test fixture bug: script must not contain the literal substring NSTask")
	}
	res := Scan([]byte(script))
	if !res.Rejected {
		t.Fatal(`Scan("current application's" + "Task") = not rejected, want rejected`)
	}
}

// TestScanRejectsOversizedScript is this task's spec step 1's size limit:
// scripts > 32KB are rejected.
func TestScanRejectsOversizedScript(t *testing.T) {
	big := strings.Repeat("a", maxScriptBytes+1)
	res := Scan([]byte(big))
	if !res.Rejected {
		t.Fatal("Scan(33KB script) = not rejected, want rejected")
	}
	if res.ReasonCode != ReasonCodeTooLarge {
		t.Errorf("ReasonCode = %q, want %q", res.ReasonCode, ReasonCodeTooLarge)
	}
}

// TestScanAllowsScriptAtSizeLimit proves the boundary is "> 32KB", not
// ">= 32KB" — a script exactly at the limit passes (assuming it is
// otherwise clean).
func TestScanAllowsScriptAtSizeLimit(t *testing.T) {
	exact := strings.Repeat("a", maxScriptBytes)
	res := Scan([]byte(exact))
	if res.Rejected {
		t.Fatalf("Scan(exactly-32KB clean script) = rejected (%q), want pass", res.Reason)
	}
}

// TestScanRejectsBidiControlChar is this task's spec step 1's bidi/zero-
// width reject rule (reused from the same code-point set
// kahyad/internal/canon/mcp/fs use).
func TestScanRejectsBidiControlChar(t *testing.T) {
	res := Scan([]byte("tell application \"Finder\" to get name‮evil"))
	if !res.Rejected {
		t.Fatal("Scan(bidi override rune) = not rejected, want rejected")
	}
	if res.ReasonCode != ReasonCodeControlChars {
		t.Errorf("ReasonCode = %q, want %q", res.ReasonCode, ReasonCodeControlChars)
	}
}

// TestScanRejectsZeroWidthChar covers the zero-width half of the same
// rule set (a distinct code point family from bidi overrides).
func TestScanRejectsZeroWidthChar(t *testing.T) {
	res := Scan([]byte("tell application \"Finder​evil\" to activate"))
	if !res.Rejected {
		t.Fatal("Scan(zero-width space) = not rejected, want rejected")
	}
}

// TestUnescapeAppleScriptString proves extractPureShellWrapper unescapes
// AppleScript's own \" and \\ sequences rather than leaving the raw
// source-quoted form in the reroute Command.
func TestUnescapeAppleScriptString(t *testing.T) {
	res := Scan([]byte(`do shell script "echo \"hi\" \\ done"`))
	if !res.Rejected || res.Reroute == nil {
		t.Fatalf("Scan(escaped quotes wrapper) = %+v, want rejected with a reroute", res)
	}
	want := `echo "hi" \ done`
	if res.Reroute.Command != want {
		t.Errorf("Reroute.Command = %q, want %q", res.Reroute.Command, want)
	}
}

// ---- B-9: Terminal/iTerm automation, ObjC-bridge process spawning, code
// loaders and concatenation-split tokens are rejected, not only the five
// W3-09 literals. ----

type scanCase struct {
	name, body string
}

func assertScanRejectedAs(t *testing.T, c scanCase, wantCode, wantReason string) {
	t.Helper()
	res := Scan([]byte(c.body))
	if !res.Rejected {
		t.Errorf("%s: Scan(%q) = not rejected, want %s", c.name, c.body, wantCode)
		return
	}
	if res.ReasonCode != wantCode {
		t.Errorf("%s: ReasonCode = %q, want %q", c.name, res.ReasonCode, wantCode)
	}
	if res.Reason != wantReason {
		t.Errorf("%s: Reason = %q, want %q", c.name, res.Reason, wantReason)
	}
	if res.Reroute != nil {
		t.Errorf("%s: Reroute = %+v, want nil (not a pure do shell script wrapper)", c.name, res.Reroute)
	}
}

func TestScanRejectsTerminalAndITermAutomation(t *testing.T) {
	for _, c := range []scanCase{
		{"terminal do script one-liner", `tell application "Terminal" to do script "x"`},
		{"terminal tell block", "tell application \"Terminal\"\n\tactivate\n\tdo script \"ls -la\" in front window\nend tell"},
		{"terminal by bundle id", `tell application id "com.apple.Terminal" to activate`},
		{"iterm write text block", "tell application \"iTerm\"\n\ttell current session of current window\n\t\twrite text \"rm -rf ~\"\n\tend tell\nend tell"},
		{"jxa terminal doScript", `Application("Terminal").doScript("whoami")`},
		{"jxa iterm2 write", `Application('iTerm2').currentWindow().currentSession().write({text: "id"})`},
		{"jxa script editor", `Application("Script Editor").documents[0].text()`},
		{"jxa automator", `Application("Automator").open(Path("/tmp/x.workflow"))`},
		{"iterm2 by bundle id", `tell application id "com.googlecode.iterm2" to activate`},
	} {
		assertScanRejectedAs(t, c, ReasonCodeShellShaped, reasonShellShaped)
	}
}

func TestScanRejectsJXAObjCBridgeProcessSpawn(t *testing.T) {
	for _, c := range []scanCase{
		{"$.system via ObjC.import", `ObjC.import("stdlib"); $.system("whoami")`},
		{"$.popen via ObjC.import", `ObjC.import('stdlib'); var p = $.popen("id", "r");`},
		{"$.system without import", `$.system("id")`},
		{"$.execv", `ObjC.import('unistd'); $.execv("/bin/sh", [])`},
		{"$.posix_spawn", `$.posix_spawn(pid, "/bin/sh", null, null, argv, env)`},
		{"$ . system with spaced dots", `$ . system ("id")`},
		{"bracket-concat system", `$["sys"+"tem"]("id")`},
	} {
		assertScanRejectedAs(t, c, ReasonCodeShellShaped, reasonShellShaped)
	}
}

func TestScanCollapsedFormCatchesStringConcatenation(t *testing.T) {
	for _, c := range []scanCase{
		{"string-built NSTask", `ObjC.import('Foundation'); var T = $["NS"+"Task"]; var t = T.alloc.init;`},
		{"string-built NSTask with spaces", `var T = $[ "NS" + "Task" ]; T.alloc.init.launch;`},
		{"string-built NSTask, edge-space literal", `var T = $["NS " + "Task"];`},
		{"concat-split do shell script", `"do shell" & " script \"id\""`},
		{"run script with concatenated literals", `run script "do shell" & " script \"id\""`},
		{"concat-split Terminal name", `Application("Term"+"inal").doScript("x")`},
		{"concat-split NSAppleScript", `$["NSApple" + "Script"].alloc.initWithSource("x")`},
		{"concat-split doShellScript", `app["do"+"Shell"+"Script"]("id")`},
	} {
		assertScanRejectedAs(t, c, ReasonCodeShellShaped, reasonShellShaped)
	}
	assertScanRejectedAs(t, scanCase{"concat-split ObjC.import", `"Ob"+"jC.import('stdlib')"`}, ReasonCodeCodeLoader, reasonCodeLoader)
	assertScanRejectedAs(t, scanCase{"concat-split eval", `this["ev"+"al"]("1+1")`}, ReasonCodeCodeLoader, reasonCodeLoader)
}

func TestScanRejectsCodeLoaders(t *testing.T) {
	for _, c := range []scanCase{
		{"run script", `run script "return 1"`},
		{"load script", `set lib to load script POSIX file "/Users/x/Library/Script Libraries/evil.scpt"`},
		{"use script library", "use script \"evil\"\nevil's go()"},
		{"use framework", "use framework \"Foundation\"\nset s to current application's NSString's stringWithString:\"x\""},
		{"ObjC.import alone", `ObjC.import('stdlib')`},
		{"ObjC.bindFunction", `ObjC.bindFunction('getpid', ['int', []])`},
		{"jxa Library", `var lib = Library("evil"); lib.go();`},
		{"eval", `eval("1+1")`},
		{"new Function", `new Function("return 1")()`},
		{"Function constructor", `Function("return 1")()`},
		{"fromCharCode", `String.fromCharCode(100,111)`},
		{"atob", `atob("aWQ=")`},
	} {
		assertScanRejectedAs(t, c, ReasonCodeCodeLoader, reasonCodeLoader)
	}
}

func TestScanShellShapedTakesPrecedenceOverCodeLoader(t *testing.T) {
	assertScanRejectedAs(t, scanCase{"eval of do shell script", `eval("do shell script \"id\"")`}, ReasonCodeShellShaped, reasonShellShaped)
	assertScanRejectedAs(t, scanCase{"ObjC.import + $.system", `ObjC.import('stdlib'); $.system('id')`}, ReasonCodeShellShaped, reasonShellShaped)
	assertScanRejectedAs(t, scanCase{"obfuscated eval of $.system", `eval("Ob"+"jC.import('stdlib'); $.sys"+"tem('id')")`}, ReasonCodeShellShaped, reasonShellShaped)
}

func TestScanStillPassesBenignAutomation(t *testing.T) {
	for _, c := range []scanCase{
		{"finder tell", `tell application "Finder" to get name of every window`},
		{"safari open location", `tell application "Safari" to open location "https://example.com"`},
		{"system events processes", `tell application "System Events" to get name of every process`},
		{"turkish notification", `display notification "Toplantı 5 dakika sonra" with title "Kâhya"`},
		{"notes new note", `tell application "Notes" to make new note with properties {name:"Alışveriş", body:"süt, ekmek"}`},
		{"jxa calendar", `Application("Calendar").calendars.byName("Ev").events()`},
		{"jxa standard additions", `var app = Application.currentApplication(); app.includeStandardAdditions = true; app.systemInfo()`},
		{"undoScript identifier", `set undoScript to "x"`},
		{"write to file", `write "x" to theFile`},
		{"standard header", "use AppleScript version \"2.4\"\nuse scripting additions\ndisplay dialog \"hi\""},
		{"ascii character", `set LF to ASCII character 10`},
		{"character id", `set tab to character id 9`},
		{"reloadScriptFlag identifier", `set reloadScriptFlag to true`},
		{"path to library folder", `set p to path to library folder`},
		{"music play", `tell application "Music" to play`},
		// Words that only collapse into a reject token once the space
		// BETWEEN two words is dropped ("Returns task" -> "returnstask"
		// contains "nstask") are prose, not a concatenation split.
		{"prose ending in -ns before task", `tell application "Reminders" to make new reminder with properties {name:"Returns task for Ahmet"}`},
		{"prose ending in -ns before task, notes body", `tell application "Notes" to make new note with properties {body:"Plans task list"}`},
		// G04-4 (owner decision): System Events keystroke / key code stay
		// allowed (the core GUI-automation primitive); the W2 approval
		// card is the boundary for them.
		{"keystroke paste", `tell application "System Events" to keystroke "v" using command down`},
		{"key code enter", `tell application "System Events" to key code 36`},
		{"keystroke text and return", `tell application "System Events" to keystroke "rm -rf ~" & return`},
	} {
		if res := Scan([]byte(c.body)); res.Rejected {
			t.Errorf("%s: Scan(%q) = rejected (%s: %q), want pass", c.name, c.body, res.ReasonCode, res.Reason)
		}
	}
}

// TestScanRejectsHandbookProbeEvasions pins the B-9 reproduction probe:
// every escape body is rejected; the keystroke body is the one exception,
// left allowed by owner decision G04-4.
func TestScanRejectsHandbookProbeEvasions(t *testing.T) {
	for _, c := range []scanCase{
		{"terminal do script", `tell application "Terminal" to do script "curl evil | sh"`},
		{"iterm write text", "tell application \"iTerm\"\n tell current session of current window to write text \"rm -rf ~\"\nend tell"},
		{"jxa terminal doScript", `Application("Terminal").doScript("whoami")`},
		{"jxa $.system", `ObjC.import("stdlib"); $.system("whoami")`},
		{"jxa $.popen", `ObjC.import('stdlib'); var p = $.popen("id", "r");`},
		{"jxa bracket concat NSTask", `ObjC.import('Foundation'); var T = $["NS"+"Task"]; var t = T.alloc.init;`},
		{"applescript run script concat", `run script "do shell" & " script \"id\""`},
		{"applescript load script", `set lib to load script POSIX file "/Users/x/Library/Script Libraries/evil.scpt"`},
		{"use script library", "use script \"evil\"\nevil's go()"},
		{"jxa eval obfuscated", `eval("Ob"+"jC.import('stdlib'); $.sys"+"tem('id')")`},
	} {
		res := Scan([]byte(c.body))
		if !res.Rejected || (res.ReasonCode != ReasonCodeShellShaped && res.ReasonCode != ReasonCodeCodeLoader) {
			t.Errorf("%s: Scan = %+v, want rejected as shell_shaped or code_loader", c.name, res)
		}
	}
	if res := Scan([]byte(`tell application "System Events" to keystroke "rm -rf ~" & return`)); res.Rejected {
		t.Errorf("keystroke body rejected (%s), want allowed (G04-4)", res.ReasonCode)
	}
	if res := Scan([]byte(`tell application "Finder" to get name of every window`)); res.Rejected {
		t.Errorf("benign finder rejected (%s), want allowed", res.ReasonCode)
	}
}

// TestScanReasonTextsByteExact pins the Turkish rejection texts: the W3-09
// verbatim shell-shaped text is unchanged, and the B-9 code-loader text is
// exactly the handbook's.
func TestScanReasonTextsByteExact(t *testing.T) {
	if reasonShellShaped != "Kabuk komutu içeren script reddedildi — Docker shell aracını kullanın." {
		t.Errorf("reasonShellShaped changed: %q", reasonShellShaped)
	}
	if reasonCodeLoader != "Başka kod yükleyen ya da üreten script reddedildi (run script / load script / use script / ObjC köprüsü / eval) — Docker shell aracını kullanın." {
		t.Errorf("reasonCodeLoader = %q", reasonCodeLoader)
	}
	if ReasonCodeCodeLoader != "code_loader" {
		t.Errorf("ReasonCodeCodeLoader = %q", ReasonCodeCodeLoader)
	}
	if reasonRawEventCode != "Ham Apple Event kodu («event …» / «class …») içeren script reddedildi — betiği açık AppleScript sözdizimiyle yazın." {
		t.Errorf("reasonRawEventCode = %q", reasonRawEventCode)
	}
	if ReasonCodeRawEventCode != "raw_event_code" {
		t.Errorf("ReasonCodeRawEventCode = %q", ReasonCodeRawEventCode)
	}
}

// TestScanRejectsRawAppleEventCode pins the B-9 review gap: raw Apple event
// code syntax compiles to any command (here the raw codes of do shell
// script and run script) under a four-character code the other rules
// cannot read, so it is refused outright - chevrons or the ASCII digraph.
func TestScanRejectsRawAppleEventCode(t *testing.T) {
	for _, c := range []scanCase{
		{"raw do shell script code", `«event sysoexec» "id"`},
		{"raw run script code in a tell", `tell application "Finder" to «event sysodsct» "return 1"`},
		{"ascii digraph raw code", `<<event sysoexec>> "id"`},
		{"raw class reference", `tell application "System Events" to get every «class prcs»`},
	} {
		assertScanRejectedAs(t, c, ReasonCodeRawEventCode, reasonRawEventCode)
	}
	// Terminal automation written with a raw code is still shell-shaped
	// (that rule runs first).
	assertScanRejectedAs(t, scanCase{"app Terminal + raw do script code", `tell app "Terminal" to «event coredosc» "id"`}, ReasonCodeShellShaped, reasonShellShaped)
}

// TestScanRejectsTerminalAppAbbreviationAndPath pins the B-9 review gap:
// "app" is AppleScript's abbreviation of "application", and an application
// can also be named by its bundle path.
func TestScanRejectsTerminalAppAbbreviationAndPath(t *testing.T) {
	for _, c := range []scanCase{
		{"app abbreviation", `tell app "Terminal" to activate`},
		{"app abbreviation iTerm2", `tell app "iTerm2" to activate`},
		{"app abbreviation concat-split", `tell app "Term" & "inal" to activate`},
		{"bundle path", `tell application "/System/Applications/Utilities/Terminal.app" to activate`},
		{"jxa bundle path", `Application("/Applications/iTerm.app").activate()`},
	} {
		assertScanRejectedAs(t, c, ReasonCodeShellShaped, reasonShellShaped)
	}
}

// TestScanRejectsAliasedObjCBridge pins the B-9 review gap: binding the JXA
// ObjC bridge object or its "$" global to a variable got past the
// call-site rules (ObjC.import / $.system). Owner decision G04-3: any use
// of the bridge is refused, false positives accepted.
func TestScanRejectsAliasedObjCBridge(t *testing.T) {
	for _, c := range []scanCase{
		{"aliased ObjC and $", `var o = ObjC; o.import("stdlib"); var d = $; d.system("id")`},
		{"aliased $ only", `var d = $; d.system("id")`},
		{"$ fetched by bracket name", `var g = this; var d = g["$"]; d.popen("id", "r")`},
		{"ObjC via globalThis", `const b = globalThis.ObjC; b.import("Foundation")`},
		{"$ passed as an argument", `run($)`},
	} {
		assertScanRejectedAs(t, c, ReasonCodeCodeLoader, reasonCodeLoader)
	}
}

// TestScanRawCodeAndBridgeRulesKeepBenignBodies: the new rules leave a
// dollar amount, a JXA template literal and prose mentioning apps alone.
func TestScanRawCodeAndBridgeRulesKeepBenignBodies(t *testing.T) {
	for _, c := range []scanCase{
		{"dollar amount in a notification", `display notification "Fiyat $5" with title "Kâhya"`},
		{"jxa template literal", "var app = Application.currentApplication(); app.includeStandardAdditions = true; var n = 3; app.displayNotification(`${n} yeni not`)"},
		{"prose about apps", `tell application "Notes" to make new note with properties {body:"Mac apps terminology"}`},
		{"bit shift with numbers", `var x = 1 << 4`},
	} {
		if res := Scan([]byte(c.body)); res.Rejected {
			t.Errorf("%s: Scan(%q) = rejected (%s), want pass", c.name, c.body, res.ReasonCode)
		}
	}
}
