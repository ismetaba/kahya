package main

import (
	"strings"
	"testing"
)

// TestTimeMachineWiringReadsLedgerAndReexcludesNightly (FIX-P-18): the
// prod TimeMachine is built with st.Queries (so the no-offsite alarm's
// 24h limit survives a restart), and the backup-nightly handler re-applies
// the exclusions after its snapshot (decision G09-5).
func TestTimeMachineWiringReadsLedgerAndReexcludesNightly(t *testing.T) {
	onlyCallSite(t, "backup", "NewTimeMachine", "backup.NewExecTMRunner(), governorNotifier, backup.RealClock{}, st.Queries, log")

	var nightlyStart, nightlyEnd int = -1, -1
	for _, c := range mainCallSites(t, "sched", "RegisterHandler") {
		switch {
		case strings.HasPrefix(c.args, `"backup-nightly"`):
			nightlyStart = c.offset
		case strings.HasPrefix(c.args, `"memory-push"`):
			nightlyEnd = c.offset
		}
	}
	if nightlyStart < 0 || nightlyEnd < nightlyStart {
		t.Fatalf("backup-nightly handler at %d, memory-push at %d; want backup-nightly registered first", nightlyStart, nightlyEnd)
	}
	snapshot := onlyCallSite(t, "snapshotter", "Run", "")
	sites := mainCallSites(t, "tm", "EnsureExclusions")
	if len(sites) != 2 {
		t.Fatalf("tm.EnsureExclusions calls = %+v, want 2 (boot + nightly)", sites)
	}
	if sites[0].args != "tmCtx, cfg.DBPath, cfg.BackupDir" || sites[0].offset > nightlyStart {
		t.Errorf("boot tm.EnsureExclusions = %+v, want (tmCtx, cfg.DBPath, cfg.BackupDir) before the job handlers", sites[0])
	}
	nightly := sites[1]
	if nightly.args != "ctx, cfg.DBPath, cfg.BackupDir" || nightly.offset < snapshot.offset || nightly.offset > nightlyEnd {
		t.Errorf("nightly tm.EnsureExclusions = %+v, want (ctx, cfg.DBPath, cfg.BackupDir) inside backup-nightly after snapshotter.Run", nightly)
	}
}
