package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// bootLines returns every line of src that launches the kahyad binary
// (a variable named *KAHYAD_BIN* run as a command) under envAssign.
func bootLines(src, envAssign string) []string {
	var out []string
	for _, l := range strings.Split(src, "\n") {
		if strings.Contains(l, envAssign) && strings.Contains(l, "KAHYAD_BIN") && !strings.HasPrefix(strings.TrimSpace(l), "#") {
			out = append(out, l)
		}
	}
	return out
}

// TestSideBySideProfilesGetTheirOwnEgressPort (FIX-FINAL ops-restore-
// drill-egress-port-collision): every profile reads prod's config.yaml, so
// a second-profile kahyad inherited prod's egress_port (default 3128) and
// exited egress_proxy_start_failed (address already in use) while prod ran:
// `make restore-drill` (which requires prod up) always failed, and so did
// kahya-dev-env.sh's provisioning boot. Each such boot now passes its own
// KAHYA_EGRESS_PORT, and the dev launchd plist carries one too.
func TestSideBySideProfilesGetTheirOwnEgressPort(t *testing.T) {
	drill := readRepoFile(t, "scripts/restore-drill.sh")
	lines := bootLines(drill, "KAHYA_ENV=restore")
	if len(lines) != 1 || !strings.Contains(lines[0], `KAHYA_EGRESS_PORT="$RESTORE_EGRESS_PORT"`) {
		t.Fatalf("restore-drill.sh scratch boot lines = %q, want one with KAHYA_EGRESS_PORT=\"$RESTORE_EGRESS_PORT\"", lines)
	}
	if !strings.Contains(drill, `[ "$RESTORE_EGRESS_PORT" = "$PROD_EGRESS_PORT" ]`) {
		t.Error("restore-drill.sh must refuse a scratch egress port equal to production's")
	}
	dev := readRepoFile(t, "scripts/kahya-dev-env.sh")
	lines = bootLines(dev, "KAHYA_ENV=dev")
	if len(lines) != 1 || !strings.Contains(lines[0], `KAHYA_EGRESS_PORT="${DEV_EGRESS_PORT}"`) {
		t.Fatalf("kahya-dev-env.sh provisioning boot lines = %q, want one with KAHYA_EGRESS_PORT=\"${DEV_EGRESS_PORT}\"", lines)
	}
	if !strings.Contains(dev, `-e "s#__KAHYA_DEV_EGRESS_PORT__#${DEV_EGRESS_PORT}#g"`) {
		t.Error("kahya-dev-env.sh does not render the dev plist's egress port")
	}
	plist := readRepoFile(t, "dev/launchd/com.kahya.dev.plist")
	if !strings.Contains(plist, "<key>KAHYA_EGRESS_PORT</key>\n\t\t<string>__KAHYA_DEV_EGRESS_PORT__</string>") {
		t.Error("the dev launchd plist does not set KAHYA_EGRESS_PORT")
	}
}

// TestDevEnvScriptRefusesANonNumericEgressPort (FIX-FINAL ops-config-r2-
// env-overrides-silently-ignore-bad-values): kahya-dev-env.sh checks
// KAHYA_DEV_EGRESS_PORT right after reading it - before its prod guard,
// before anything is created or rendered - so "31a9" never reaches the dev
// plist (where config.Load would now refuse it at every boot). The guard is
// run on its own here: the lines from the assignment through its esac.
func TestDevEnvScriptRefusesANonNumericEgressPort(t *testing.T) {
	dev := readRepoFile(t, "scripts/kahya-dev-env.sh")
	const assign = `DEV_EGRESS_PORT="${KAHYA_DEV_EGRESS_PORT:-3129}"`
	start := strings.Index(dev, assign)
	if start < 0 {
		t.Fatalf("kahya-dev-env.sh no longer assigns %s", assign)
	}
	end := strings.Index(dev[start:], "\nesac\n")
	if end < 0 {
		t.Fatal("kahya-dev-env.sh has no case guard after the DEV_EGRESS_PORT assignment")
	}
	guard := dev[start : start+end+len("\nesac\n")]
	if before := dev[:start]; strings.Contains(before, "${DEV_EGRESS_PORT}") || strings.Contains(before, "$DEV_EGRESS_PORT") {
		t.Fatal("DEV_EGRESS_PORT is expanded before its assignment and guard")
	}
	if rest := dev[start+len(guard):]; !strings.Contains(rest, "hard guard: never operate on the production profile") {
		t.Fatal("the port guard must come before the script's prod guard and every side effect")
	}
	for _, tc := range []struct {
		value   string
		set     bool
		wantErr bool
	}{
		{"", false, false},
		{"3129", true, false},
		{"4001", true, false},
		{"31a9", true, true},
		{"abc", true, true},
		{"12.5", true, true},
		{" 3129", true, true},
		{"-1", true, true},
	} {
		cmd := exec.Command("bash", "-c", "set -euo pipefail\n"+guard+"echo port=${DEV_EGRESS_PORT}\n")
		cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
		if tc.set {
			cmd.Env = append(cmd.Env, "KAHYA_DEV_EGRESS_PORT="+tc.value)
		}
		out, err := cmd.CombinedOutput()
		if tc.wantErr {
			if err == nil || !strings.Contains(string(out), "is not a port number") || strings.Contains(string(out), "port=") {
				t.Errorf("KAHYA_DEV_EGRESS_PORT=%q: err=%v out=%q, want exit 1 with the refusal and nothing after it", tc.value, err, out)
			}
			continue
		}
		want := "port=" + tc.value
		if !tc.set {
			want = "port=3129"
		}
		if err != nil || strings.TrimSpace(string(out)) != want {
			t.Errorf("KAHYA_DEV_EGRESS_PORT=%q (set=%v): err=%v out=%q, want %q", tc.value, tc.set, err, out, want)
		}
	}
}
