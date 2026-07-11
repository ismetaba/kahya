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
