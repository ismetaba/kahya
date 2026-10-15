package main

import (
	"os"
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
