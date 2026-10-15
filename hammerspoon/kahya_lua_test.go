// Package hammerspoon holds only tests: hammerspoon/kahya.lua is installed
// by `make hammerspoon-install` and has no Lua interpreter in CI, so its
// safety-relevant structure is pinned by text-structure tests here
// (tests/invariants/parse.go is the repo's precedent for parsing a
// non-Go file).
package hammerspoon

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// haltBanner is W6-03's byte-exact success banner (task spec step 7).
const haltBanner = "Acil durdurma — tüm görevler durduruldu"

// haltNotHaltedPrefix is FIX-P-21's byte-exact sticky warning prefix for a
// `kahya halt` that did not exit 0 (nothing was halted server-side).
const haltNotHaltedPrefix = "⛔ Durdurma komutu kahyad'a ulaşamadı — görevler DURDURULMADI: "

func readLua(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("kahya.lua")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// haltHandler returns the ⌥⎋ binding's source, from its hs.hotkey.bind
// line to the first top-level "end)" that closes it.
func haltHandler(t *testing.T, src string) string {
	t.Helper()
	start := strings.Index(src, `hs.hotkey.bind({ "alt" }, "escape"`)
	if start < 0 {
		t.Fatal("alt-escape binding not found in kahya.lua")
	}
	handler := src[start:]
	if end := strings.Index(handler, "\nend)"); end > 0 {
		handler = handler[:end]
	}
	return handler
}

// haltBranchesRE splits the handler into the `if exitCode == 0 then`
// branch and its `else` branch. Line-anchored else/end on purpose: the
// branch bodies contain "}):send()", whose "end" substring would stop a
// lazy `(.*?)end` match early.
var haltBranchesRE = regexp.MustCompile(`(?s)if exitCode == 0 then\n(.*?)\n\s*else\n(.*?)\n\s*end\n`)

// TestHaltBannerOnlyOnExitZero (FIX-P-21): the byte-exact W6-03 banner
// appears exactly once in kahya.lua, inside the ⌥⎋ handler, and only under
// `if exitCode == 0 then` - a `kahya halt` that could not reach kahyad
// (exit 1) or was misinvoked (exit 2) halted NOTHING, so it must never
// show "tüm görevler durduruldu". The else branch carries the sticky
// (autoWithdraw = false) not-halted warning instead. Decision G09-7: the
// success banner stays one line (no `kahya halt` stdout appended).
func TestHaltBannerOnlyOnExitZero(t *testing.T) {
	src := readLua(t)
	if n := strings.Count(src, haltBanner); n != 1 {
		t.Fatalf("banner occurrences = %d, want 1", n)
	}
	m := haltBranchesRE.FindStringSubmatch(haltHandler(t, src))
	if m == nil {
		t.Fatal("the alt-escape handler has no `if exitCode == 0 then ... else ... end` block")
	}
	success, failure := m[1], m[2]
	if !strings.Contains(success, haltBanner) {
		t.Errorf("the banner is not inside the exitCode == 0 branch:\n%s", success)
	}
	if strings.Contains(failure, haltBanner) {
		t.Errorf("the banner leaked into the else branch:\n%s", failure)
	}
	if !strings.Contains(failure, `"`+haltNotHaltedPrefix+`"`) {
		t.Errorf("the else branch must carry the byte-exact not-halted warning %q:\n%s", haltNotHaltedPrefix, failure)
	}
	if !strings.Contains(failure, "autoWithdraw = false") {
		t.Errorf("the not-halted warning must be sticky (autoWithdraw = false):\n%s", failure)
	}
}

// TestHaltSuccessBannerIsOneLine (FIX-P-21, decision G09-7): the success
// branch sends exactly one notification and appends nothing from the
// CLI's stdout/stderr to the byte-exact banner.
func TestHaltSuccessBannerIsOneLine(t *testing.T) {
	m := haltBranchesRE.FindStringSubmatch(haltHandler(t, readLua(t)))
	if m == nil {
		t.Fatal("the alt-escape handler has no `if exitCode == 0 then ... else ... end` block")
	}
	success := m[1]
	if n := strings.Count(success, "hs.notify.new("); n != 1 {
		t.Fatalf("success branch sends %d notifications, want 1", n)
	}
	if !strings.Contains(success, `informativeText = "`+haltBanner+`",`) {
		t.Errorf("success banner must be the bare byte-exact literal:\n%s", success)
	}
	if strings.Contains(success, "stdOut") || strings.Contains(success, "stdErr") {
		t.Errorf("success branch must not append CLI output to the banner:\n%s", success)
	}
}

// approvalDecisionFn returns sendApprovalDecision's source, from its
// `local function` line to the first top-level "end" that closes it.
func approvalDecisionFn(t *testing.T, src string) string {
	t.Helper()
	start := strings.Index(src, "local function sendApprovalDecision(")
	if start < 0 {
		t.Fatal("sendApprovalDecision not found in kahya.lua")
	}
	fn := src[start:]
	if end := strings.Index(fn, "\nend\n"); end > 0 {
		fn = fn[:end]
	}
	return fn
}

// TestApprovalDecisionRejectExitOneIsDelivered (FIX-FINAL
// hammerspoon-approval-cards-broken): `kahya approvals decide <id>
// --reject` exits 1 on a DELIVERED rejection (kahya approve's 0/1/2
// convention), so the callback must not report "Onay kararı iletilemedi"
// for that exit code; it must still report it for every other non-zero
// exit. The argv keeps the id-first shape the CLI now accepts.
func TestApprovalDecisionRejectExitOneIsDelivered(t *testing.T) {
	fn := approvalDecisionFn(t, readLua(t))
	if !strings.Contains(fn, `local args = { "approvals", "decide", id }`) {
		t.Errorf("sendApprovalDecision argv changed shape:\n%s", fn)
	}
	if strings.Contains(fn, "if exitCode ~= 0 then") {
		t.Errorf("sendApprovalDecision treats every non-zero exit (incl. a delivered --reject's 1) as a failure:\n%s", fn)
	}
	if !strings.Contains(fn, "local delivered = (exitCode == 0) or ((not approve) and exitCode == 1)") {
		t.Errorf("sendApprovalDecision lacks the reject-exit-1-is-delivered rule:\n%s", fn)
	}
	if !strings.Contains(fn, "if not delivered then") || !strings.Contains(fn, `"Onay kararı iletilemedi: "`) {
		t.Errorf("the not-delivered notice must stay, gated on `not delivered`:\n%s", fn)
	}
}
