package w3gate

// selfprotect_test.go: FIX-B-4 over real bins — the deny list kahyad wires
// into fs_write is policy.yaml's globs PLUS the profile-aware self-protect
// globs (config.SelfProtectDenyGlobs). This harness boots KAHYA_ENV=dev
// with KAHYA_DATA_DIR/KAHYA_MEMORY_DIR under a temp HOME, none of which
// policy.yaml's prod-path globs name, so a refusal here proves main.go's
// wiring end to end (including the symlink-resolved form: the temp HOME
// lives under macOS's /var/folders -> /private/var/folders link).

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestB4SelfProtectDenyGlobsRefuseRuntimePathsOverRealBins(t *testing.T) {
	d := bootKahyad(t, daemonOpts{})
	db := d.openDB(t)

	cases := []struct{ name, path string }{
		{"dev data dir (KAHYA_DATA_DIR)", "~/data/zehir.txt"},
		{"dev memory dir (KAHYA_MEMORY_DIR)", "~/memory/zehir.md"},
		{"dev KahyaDir (~/Kahya-dev)", "~/Kahya-dev/memory/zehir.md"},
		{"a repo's .git config (mandatory policy glob)", "~/code/x/.git/config"},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			traceID := fmt.Sprintf("trace-b4-selfprotect-%04d", i)
			args := map[string]any{"path": c.path, "content_base64": base64.StdEncoding.EncodeToString([]byte("[core]\n\tfsmonitor = /tmp/x.sh\n"))}
			res := d.mcpCall(t, traceID, "task-b4-selfprotect", "fs_write", args)
			if !res.IsError {
				t.Fatalf("fs_write(%s) = %+v, want a deny-glob refusal", c.path, res)
			}
			if !waitForEvent(t, db, traceID, "fs_deny_glob_hit", 5*time.Second) {
				t.Fatalf("fs_write(%s): no fs_deny_glob_hit ledger row for %s", c.path, traceID)
			}
			if n := countEventsMatching(t, db, traceID, "policy_decision"); n != 0 {
				t.Fatalf("fs_write(%s): %d policy_decision rows, want 0 (refused before any policy decision)", c.path, n)
			}
		})
	}
	for _, p := range []string{filepath.Join(d.homeDir, "data", "zehir.txt"), filepath.Join(d.memDir, "zehir.md"), filepath.Join(d.homeDir, "Kahya-dev", "memory", "zehir.md")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s exists after a refused fs_write (stat err=%v)", p, err)
		}
	}
}
