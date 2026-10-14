//go:build acceptance

package w6gate

import (
	"strings"
	"testing"
)

// TestBootstrapSeedsMemorySearchL1OnceAcrossRestart is B-1's first-run
// autonomy acceptance over the real bin/kahyad + bin/kahya (owner decision
// G01-2 option A, config autonomy_bootstrap_levels, default
// {"memory_search/R/global": 1}): a fresh brain.db boots with
// memory_search/R/global at L1 (ledgered autonomy_bootstrapped, shown by
// `kahya autonomy`); a restart seeds nothing new; and a row later demoted
// back to L0 stays L0 across another restart - the bootstrap never
// overrides an existing row.
func TestBootstrapSeedsMemorySearchL1OnceAcrossRestart(t *testing.T) {
	d := bootKahyad(t, daemonOpts{})
	db := d.openDB(t)

	bootstrapped := func() int {
		t.Helper()
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM events WHERE kind = 'autonomy_bootstrapped'
			AND json_extract(payload, '$.tool') = 'memory_search' AND json_extract(payload, '$.class') = 'R'
			AND json_extract(payload, '$.scope') = 'global' AND json_extract(payload, '$.level') = 1`).Scan(&n); err != nil {
			t.Fatalf("count autonomy_bootstrapped: %v", err)
		}
		return n
	}
	level := func() int {
		t.Helper()
		var lvl int
		if err := db.QueryRow(`SELECT level FROM autonomy_state WHERE tool = 'memory_search' AND class = 'R' AND scope = 'global'`).Scan(&lvl); err != nil {
			t.Fatalf("read memory_search autonomy_state: %v", err)
		}
		return lvl
	}

	if n := bootstrapped(); n != 1 {
		t.Fatalf("autonomy_bootstrapped(memory_search/R/global/1) on a fresh brain.db = %d, want 1", n)
	}
	if got := level(); got != 1 {
		t.Fatalf("memory_search/R/global level after first boot = %d, want 1", got)
	}
	stdout, stderr, code := d.runCLI(t, "autonomy")
	if code != 0 {
		t.Fatalf("kahya autonomy exited %d: %s", code, stderr)
	}
	want := []string{"memory_search", "R", "global", "L1", "(0", "ardışık", "onay)"}
	found := false
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= len(want) && strings.Join(fields[:len(want)], " ") == strings.Join(want, " ") {
			found = true
		}
	}
	if !found {
		t.Fatalf("kahya autonomy output lacks the bootstrapped row %v:\n%s", want, stdout)
	}

	// Second boot on the same brain.db: nothing new.
	d.stop()
	d.start()
	if n := bootstrapped(); n != 1 {
		t.Fatalf("autonomy_bootstrapped after a restart = %d, want still 1", n)
	}

	// A demotion back to L0 (simulated while kahyad is down - this test's
	// connection is then the only writer) survives the next boot.
	d.stop()
	if _, err := db.Exec(`UPDATE autonomy_state SET level = 0 WHERE tool = 'memory_search' AND class = 'R' AND scope = 'global'`); err != nil {
		t.Fatalf("demote memory_search: %v", err)
	}
	d.start()
	if got := level(); got != 0 {
		t.Fatalf("memory_search level after a demotion + restart = %d, want 0 (bootstrap never overrides an existing row)", got)
	}
	if n := bootstrapped(); n != 1 {
		t.Fatalf("autonomy_bootstrapped after the demoted restart = %d, want still 1", n)
	}
}
