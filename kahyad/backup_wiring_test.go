package main

import (
	"go/ast"
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

// enclosingIfConds returns the source text of the condition of every if
// statement whose body (or else branch) contains the node at offset - an
// if statement's own Init clause does not count as "inside" it.
func enclosingIfConds(t *testing.T, offset int) []string {
	t.Helper()
	fset, f, src := mainSource(t)
	var conds []string
	var walk func(n ast.Node) bool
	walk = func(n ast.Node) bool {
		if n == nil {
			return false
		}
		start, end := fset.Position(n.Pos()).Offset, fset.Position(n.End()).Offset
		if offset < start || offset >= end {
			return false
		}
		if ifs, ok := n.(*ast.IfStmt); ok {
			inBody := func(b ast.Node) bool {
				return offset >= fset.Position(b.Pos()).Offset && offset < fset.Position(b.End()).Offset
			}
			if inBody(ifs.Body) || (ifs.Else != nil && inBody(ifs.Else)) {
				conds = append(conds, string(src[fset.Position(ifs.Cond.Pos()).Offset:fset.Position(ifs.Cond.End()).Offset]))
			}
		}
		return true
	}
	ast.Inspect(f, walk)
	return conds
}

// TestBackupDirIgnoredAtBootAndGuardedInConsolidation (FIX-P-1): kahyad
// makes sure ~/Kahya's .gitignore covers cfg.BackupDir at every boot - in
// every profile, never behind a cfg.Env gate - after the lifecycle context
// exists and before the backup-nightly snapshotter is built, and the nightly
// consolidation gets the same BackupDir so it re-checks before staging.
func TestBackupDirIgnoredAtBootAndGuardedInConsolidation(t *testing.T) {
	ensure := onlyCallSite(t, "backup", "EnsureBackupDirIgnored", "cfg.KahyaDir, cfg.BackupDir")
	notify := onlyCallSite(t, "signal", "NotifyContext", "")
	snap := onlyCallSite(t, "backup", "NewSnapshotter", "")
	if ensure.offset < notify.offset {
		t.Error("backup.EnsureBackupDirIgnored runs before the lifecycle context exists")
	}
	if ensure.offset > snap.offset {
		t.Error("backup.EnsureBackupDirIgnored must run before the backup-nightly snapshotter is built")
	}
	for _, cond := range enclosingIfConds(t, ensure.offset) {
		if strings.Contains(cond, "cfg.Env") {
			t.Errorf("backup.EnsureBackupDirIgnored is gated by %q; it must run in every profile", cond)
		}
	}
	cons := onlyCompositeLit(t, "consolidation", "Config")
	if cons["BackupDir"] != "cfg.BackupDir" {
		t.Errorf("consolidation.Config.BackupDir = %q, want cfg.BackupDir", cons["BackupDir"])
	}
	if cons["KahyaDir"] != "cfg.KahyaDir" || cons["MemoryDir"] != "cfg.MemoryDir" {
		t.Errorf("consolidation.Config KahyaDir/MemoryDir = %q/%q, want cfg.KahyaDir/cfg.MemoryDir", cons["KahyaDir"], cons["MemoryDir"])
	}
}
