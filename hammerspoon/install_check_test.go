package hammerspoon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot is the checkout this package lives in.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// runCheck runs `make hammerspoon-check` with HS_DIR pointed at hsDir - the
// Makefile's own seam - and returns what it printed. The target only reads:
// it never touches the real ~/.hammerspoon here, and the test asserts it
// never writes into hsDir either.
func runCheck(t *testing.T, hsDir string) string {
	t.Helper()
	cmd := exec.Command("make", "--no-print-directory", "hammerspoon-check", "HS_DIR="+hsDir)
	cmd.Dir = repoRoot(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make hammerspoon-check failed (it must never fail an install): %v\n%s", err, out)
	}
	return string(out)
}

// rendered is what `make hammerspoon-install` writes: kahya.lua with the
// @KAHYA_BIN@ placeholder replaced by the checkout's absolute bin/kahya.
func rendered(t *testing.T) string {
	t.Helper()
	src := readLua(t)
	if !strings.Contains(src, "@KAHYA_BIN@") {
		t.Fatal("hammerspoon/kahya.lua no longer carries the @KAHYA_BIN@ placeholder")
	}
	return strings.ReplaceAll(src, "@KAHYA_BIN@", filepath.Join(repoRoot(t), "bin", "kahya"))
}

// TestHammerspoonCheckDetectsAStaleInstalledCopy (FIX-FINAL ops-config-r2-
// installed-kahya-lua-never-refreshed): ~/.hammerspoon/kahya.lua is a
// rendered copy only `make hammerspoon-install` writes, so an upgrade left
// the old Lua running (an old ⌥⎋ handler, an old approval-decision
// handler) and nothing said so. `make hammerspoon-check`, which `make
// install` and `make install-agent` now end with, says nothing alarming
// when there is no installed copy or it matches what would be installed
// now, and prints a warning naming `make hammerspoon-install` when it
// differs. It never writes the file.
func TestHammerspoonCheckDetectsAStaleInstalledCopy(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not on PATH")
	}
	current := rendered(t)
	stale := strings.Replace(current, "hs.hotkey.bind", "-- pre-upgrade copy\nhs.hotkey.bind", 1)
	if stale == current {
		t.Fatal("fixture: could not derive a stale copy")
	}
	otherCheckout := strings.ReplaceAll(readLua(t), "@KAHYA_BIN@", "/somewhere/else/bin/kahya")

	for _, tc := range []struct {
		name      string
		installed *string
		want      string
		wantWarn  bool
	}{
		{"not installed", nil, "is not installed (optional: make hammerspoon-install)", false},
		{"up to date", &current, "is up to date", false},
		{"stale", &stale, "is STALE", true},
		{"rendered for another checkout", &otherCheckout, "is STALE", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hsDir := t.TempDir()
			lua := filepath.Join(hsDir, "kahya.lua")
			if tc.installed != nil {
				if err := os.WriteFile(lua, []byte(*tc.installed), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			out := runCheck(t, hsDir)
			if !strings.Contains(out, lua) || !strings.Contains(out, tc.want) {
				t.Fatalf("output = %q, want it to name %s and say %q", out, lua, tc.want)
			}
			if got := strings.Contains(out, "WARNING:"); got != tc.wantWarn {
				t.Fatalf("WARNING present = %v, want %v; output = %q", got, tc.wantWarn, out)
			}
			if tc.wantWarn && !strings.Contains(out, "make hammerspoon-install") {
				t.Fatalf("the stale warning must name the command to run; output = %q", out)
			}
			// The check never writes: the file is exactly as it was (or
			// still absent), and nothing else appeared next to it.
			entries, err := os.ReadDir(hsDir)
			if err != nil {
				t.Fatal(err)
			}
			if tc.installed == nil {
				if len(entries) != 0 {
					t.Fatalf("the check created %v in an empty Hammerspoon dir", entries)
				}
				return
			}
			after, err := os.ReadFile(lua)
			if err != nil || string(after) != *tc.installed || len(entries) != 1 {
				t.Fatalf("the check changed the Hammerspoon dir (entries %v, read err %v)", entries, err)
			}
		})
	}
}

// makeRecipe returns target's recipe lines from the Makefile, trimmed.
func makeRecipe(t *testing.T, mk, header string) []string {
	t.Helper()
	start := strings.Index(mk, "\n"+header+"\n")
	if start < 0 {
		t.Fatalf("Makefile has no %q target", header)
	}
	var recipe []string
	for _, l := range strings.Split(mk[start+1:], "\n")[1:] {
		if !strings.HasPrefix(l, "\t") {
			break
		}
		recipe = append(recipe, strings.TrimSpace(l))
	}
	return recipe
}

// TestInstallTargetsEndWithTheHammerspoonCheck: both upgrade paths run the
// check as their LAST step (so its warning is the last thing on screen),
// and hammerspoon-install writes the very rendering the check compares
// against (HS_RENDER) into the very file it reads (HS_LUA).
func TestInstallTargetsEndWithTheHammerspoonCheck(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	mk := string(b)
	const check = "@$(MAKE) --no-print-directory hammerspoon-check"
	for _, header := range []string{"install: codesign", "install-agent: build"} {
		recipe := makeRecipe(t, mk, header)
		if len(recipe) == 0 || recipe[len(recipe)-1] != check {
			t.Errorf("%q recipe = %q, want it to end with %q", header, recipe, check)
		}
	}
	install := strings.Join(makeRecipe(t, mk, "hammerspoon-install: build"), "\n")
	if !strings.Contains(install, `$(HS_RENDER) > "$(HS_LUA)"`) {
		t.Errorf("hammerspoon-install recipe = %q, want it to write $(HS_RENDER) into $(HS_LUA)", install)
	}
	checkRecipe := strings.Join(makeRecipe(t, mk, "hammerspoon-check:"), "\n")
	if !strings.Contains(checkRecipe, `$(HS_RENDER) | cmp -s - "$(HS_LUA)"`) {
		t.Errorf("hammerspoon-check recipe = %q, want it to compare $(HS_RENDER) with $(HS_LUA)", checkRecipe)
	}
	for _, write := range []string{`> "`, ">>", "tee ", "cp ", "install "} {
		if strings.Contains(checkRecipe, write) {
			t.Errorf("hammerspoon-check must not write any file (found %q); recipe = %q", write, checkRecipe)
		}
	}
	for _, def := range []string{"HS_DIR ?= $(HOME)/.hammerspoon", "HS_LUA = $(HS_DIR)/kahya.lua"} {
		if !strings.Contains(mk, "\n"+def+"\n") {
			t.Errorf("Makefile no longer defines %q", def)
		}
	}
}
