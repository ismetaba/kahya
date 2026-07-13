package restore

import (
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kahya/kahyad/internal/backup"
	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/indexer"
	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/store"
)

// equivalenceQuery is the drill's fixed equivalence query: 'evlerimizden'
// relaxes to the trigram 'ev' (W12-03 relaxation, NOT manual stemming) and
// retrieves the byte-exact seed note (tasks/README.md: preserve the
// 'evlerimizden' retrieval test byte-exact). searchK mirrors the k a hook
// would pass through /v1/memory/search before RenderKept's own DefaultTopK cut.
const (
	equivalenceQuery = "evlerimizden"
	searchK          = 8
)

func newLogger(t *testing.T) *logx.Logger {
	t.Helper()
	log, err := logx.New(t.TempDir(), "test-restore-boot-0000000000")
	if err != nil {
		t.Fatalf("logx.New: %v", err)
	}
	t.Cleanup(func() { log.Close() })
	return log
}

func openStore(t *testing.T, dbPath string) *store.Store {
	t.Helper()
	st, err := store.Open(config.Config{DBPath: dbPath})
	if err != nil {
		t.Fatalf("store.Open(%s): %v", dbPath, err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// copyTree recursively copies the testdata memory corpus into a fresh,
// NON-git temp dir so the indexer's git-author tier check is deterministic
// (no repo -> a stable default tier) and so a mutation test can edit its own
// private copy without touching the shipped fixture.
func copyTree(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o600)
	})
	if err != nil {
		t.Fatalf("copyTree %s: %v", src, err)
	}
	return dst
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatalf("open %s: %v", src, err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		t.Fatalf("create %s: %v", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		t.Fatalf("copy %s -> %s: %v", src, dst, err)
	}
	if err := out.Close(); err != nil {
		t.Fatalf("close %s: %v", dst, err)
	}
}

func integrityCheck(t *testing.T, dbPath string) string {
	t.Helper()
	db, err := sql.Open("sqlite3", dbPath+"?mode=ro")
	if err != nil {
		t.Fatalf("open ro %s: %v", dbPath, err)
	}
	defer db.Close()
	var result string
	if err := db.QueryRow("PRAGMA integrity_check;").Scan(&result); err != nil {
		t.Fatalf("integrity_check %s: %v", dbPath, err)
	}
	return result
}

// TestRestoreDrillEquivalenceAndLedgerSurvival is the hermetic W78-05 drill:
// it builds a fixture "production" brain.db from a synthetic markdown corpus
// plus non-markdown ledger/episode rows, VACUUMs a backup, restores that
// backup into a scratch profile, reindexes it (asserting an incremental
// no-op), and proves (a) the restored <hafiza> injection block is byte-
// identical to the reference (after Normalize) and (b) the ledger/episodes -
// which markdown cannot regenerate - survived the VACUUM copy. No network,
// no worker, no cloud, no MLX (FTS-only retrieval); no production path.
func TestRestoreDrillEquivalenceAndLedgerSurvival(t *testing.T) {
	ctx := context.Background()
	log := newLogger(t)

	// --- 1. BUILD a fixture "production" state -----------------------------
	memDir := copyTree(t, filepath.Join("testdata", "memory"))
	kahyaDir := t.TempDir() // stands in for ~/Kahya
	prodDBPath := filepath.Join(t.TempDir(), "brain.db")
	prod := openStore(t, prodDBPath)

	idx := indexer.New(prod.DB(), memDir, log)
	buildRes, err := idx.Reindex(ctx, "", false)
	if err != nil {
		t.Fatalf("initial index: %v", err)
	}
	if buildRes.FilesIndexed == 0 || buildRes.Chunks == 0 {
		t.Fatalf("fixture corpus indexed nothing: %+v", buildRes)
	}
	mdFileCount := buildRes.FilesIndexed

	// Seed ledger/episode rows that are NOT derivable from markdown, so the
	// "ledger survives" assertion is meaningful: a task_done event, a
	// hafiza_injected event (both append-only ledger rows), and a 'screen'
	// episode (an episode with no backing markdown file - it must never be
	// reindexed away, and it can only come back from the VACUUM copy).
	if err := prod.LogEvent(ctx, "trace-fixture-taskdone", "task_done", map[string]any{"task_id": "t-fixture-1"}); err != nil {
		t.Fatalf("seed task_done: %v", err)
	}
	if err := prod.LogEvent(ctx, "trace-fixture-hafiza", "hafiza_injected", map[string]any{"task_id": "t-fixture-1", "chunk_ids": []int64{1}}); err != nil {
		t.Fatalf("seed hafiza_injected: %v", err)
	}
	if _, err := prod.DB().ExecContext(ctx,
		`INSERT INTO episodes (source, source_tier, status, created_at) VALUES ('screen', 'screen', 'active', ?)`,
		time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("seed screen episode: %v", err)
	}

	// --- 2. Capture the REFERENCE through the REAL injection path ----------
	refInj, err := RunEquivalence(ctx, prod.DB(), log, equivalenceQuery, searchK)
	if err != nil {
		t.Fatalf("reference RunEquivalence: %v", err)
	}
	if strings.TrimSpace(refInj.Block) == "" {
		t.Fatalf("reference <hafiza> block is empty - the equivalence query retrieved nothing, so a later 'both empty' compare would pass vacuously")
	}
	refEvents, refEpisodes, err := LedgerCounts(ctx, prod.DB())
	if err != nil {
		t.Fatalf("reference LedgerCounts: %v", err)
	}

	// --- 3. BACKUP: VACUUM INTO brain-YYYYMMDD.db + integrity_check --------
	backupDir := filepath.Join(kahyaDir, "backups")
	snap := backup.NewSnapshotter(prod, nil, backupDir) // nil notifier: success path never touches it
	if err := snap.Run(ctx, "trace-fixture-backup"); err != nil {
		t.Fatalf("backup VACUUM INTO: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(backupDir, "brain-*.db"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected exactly one brain-*.db backup, got %v (err=%v)", matches, err)
	}
	backupFile := matches[0]
	if got := integrityCheck(t, backupFile); got != "ok" {
		t.Fatalf("backup integrity_check = %q, want \"ok\"", got)
	}

	// --- 4. RESTORE into a SCRATCH profile dir (never a prod path) ---------
	scratchDir := t.TempDir() // stands in for ~/Library/Application Support/Kahya-restore
	scratchDBPath := filepath.Join(scratchDir, "brain.db")
	if err := GuardNotProd(scratchDBPath); err != nil {
		t.Fatalf("scratch path wrongly flagged as prod: %v", err)
	}
	copyFile(t, backupFile, scratchDBPath)
	if got := integrityCheck(t, scratchDBPath); got != "ok" {
		t.Fatalf("restored db integrity_check = %q, want \"ok\"", got)
	}

	scratch := openStore(t, scratchDBPath) // migrations run at open
	if scratch.SchemaVersion() != prod.SchemaVersion() {
		t.Fatalf("restored schema version = %d, want %d (prod)", scratch.SchemaVersion(), prod.SchemaVersion())
	}
	var userVersion int64
	if err := scratch.DB().QueryRow("PRAGMA user_version").Scan(&userVersion); err != nil {
		t.Fatalf("read restored user_version: %v", err)
	}
	if userVersion != prod.SchemaVersion() {
		t.Fatalf("restored PRAGMA user_version = %d, want %d", userVersion, prod.SchemaVersion())
	}

	// Reindex the restored db from the SAME markdown - must be an incremental
	// no-op (clone and backup taken at the same point). A non-empty diff means
	// markdown<->index drift and FAILS the drill.
	scratchIdx := indexer.New(scratch.DB(), memDir, log)
	reRes, err := scratchIdx.Reindex(ctx, "", false)
	if err != nil {
		t.Fatalf("restore reindex: %v", err)
	}
	if reRes.FilesIndexed != 0 || reRes.FilesRemoved != 0 || reRes.Chunks != 0 || reRes.FilesErrored != 0 {
		t.Fatalf("restore reindex was NOT an incremental no-op: %+v (markdown<->index drift)", reRes)
	}
	if reRes.FilesUnchanged != mdFileCount {
		t.Fatalf("restore reindex FilesUnchanged = %d, want %d (all fixture files unchanged)", reRes.FilesUnchanged, mdFileCount)
	}

	// --- 5. COMPARE the restored injection block to the reference ----------
	restoredInj, err := RunEquivalence(ctx, scratch.DB(), log, equivalenceQuery, searchK)
	if err != nil {
		t.Fatalf("restored RunEquivalence: %v", err)
	}
	if Normalize(restoredInj.Block) != Normalize(refInj.Block) {
		t.Fatalf("restored <hafiza> block differs from reference after Normalize.\nREF:\n%s\nRESTORED:\n%s", refInj.Block, restoredInj.Block)
	}

	// --- 6. LEDGER SURVIVES ------------------------------------------------
	rEvents, rEpisodes, err := LedgerCounts(ctx, scratch.DB())
	if err != nil {
		t.Fatalf("restored LedgerCounts: %v", err)
	}
	if rEvents < refEvents {
		t.Fatalf("restored events count %d < reference %d - the ledger did not survive the VACUUM copy", rEvents, refEvents)
	}
	if rEpisodes < refEpisodes {
		t.Fatalf("restored episodes count %d < reference %d - episodes did not survive the VACUUM copy", rEpisodes, refEpisodes)
	}
	// Specifically prove the non-markdown rows came back (they cannot be
	// regenerated from the corpus): the two seeded ledger events and the
	// 'screen' episode.
	assertCount(t, scratch.DB(), `SELECT count(*) FROM events WHERE kind = 'task_done'`, 1)
	assertCount(t, scratch.DB(), `SELECT count(*) FROM events WHERE kind = 'hafiza_injected'`, 1)
	assertCount(t, scratch.DB(), `SELECT count(*) FROM episodes WHERE source = 'screen'`, 1)
}

func assertCount(t *testing.T, db *sql.DB, query string, wantAtLeast int) {
	t.Helper()
	var n int
	if err := db.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	if n < wantAtLeast {
		t.Fatalf("count query %q = %d, want >= %d", query, n, wantAtLeast)
	}
}

// TestNormalizeDoesNotMaskContentDiff is the NARROWNESS proof: mutating one
// ORDINARY prose line in the corpus (not a trace_id, not a timestamp) changes
// the injected block in a way Normalize does NOT mask, so the equivalence
// compare would correctly FAIL on real markdown<->index drift / a poisoned
// chunk. If Normalize were too broad, the two normalized blocks would collapse
// to equal and this test would fail.
func TestNormalizeDoesNotMaskContentDiff(t *testing.T) {
	ctx := context.Background()
	log := newLogger(t)

	refBlock := blockFromCorpus(t, ctx, log, copyTree(t, filepath.Join("testdata", "memory")))

	mutated := copyTree(t, filepath.Join("testdata", "memory"))
	notePath := filepath.Join(mutated, "ev-notlari.md")
	orig, err := os.ReadFile(notePath)
	if err != nil {
		t.Fatalf("read fixture note: %v", err)
	}
	// Change a prose word the equivalence query retrieves (Moda -> Beşiktaş).
	changed := strings.Replace(string(orig), "Moda'daki", "Beşiktaş'taki", 1)
	if changed == string(orig) {
		t.Fatalf("mutation did not change the fixture - the target substring was not found")
	}
	if err := os.WriteFile(notePath, []byte(changed), 0o600); err != nil {
		t.Fatalf("write mutated note: %v", err)
	}
	mutBlock := blockFromCorpus(t, ctx, log, mutated)

	if Normalize(refBlock) == Normalize(mutBlock) {
		t.Fatalf("Normalize masked a real content change - the blocks must differ.\nREF:\n%s\nMUT:\n%s", refBlock, mutBlock)
	}
}

// blockFromCorpus indexes memDir into a fresh temp store and returns the
// equivalence-query <hafiza> block.
func blockFromCorpus(t *testing.T, ctx context.Context, log *logx.Logger, memDir string) string {
	t.Helper()
	st := openStore(t, filepath.Join(t.TempDir(), "brain.db"))
	idx := indexer.New(st.DB(), memDir, log)
	if _, err := idx.Reindex(ctx, "", false); err != nil {
		t.Fatalf("index corpus: %v", err)
	}
	inj, err := RunEquivalence(ctx, st.DB(), log, equivalenceQuery, searchK)
	if err != nil {
		t.Fatalf("RunEquivalence: %v", err)
	}
	if strings.TrimSpace(inj.Block) == "" {
		t.Fatalf("empty block from corpus %s", memDir)
	}
	return inj.Block
}

// TestNormalizeMasksVolatileFields proves the two volatile shapes ARE masked:
// two strings that differ ONLY in a trace_id and a timestamp normalize equal,
// and the placeholders actually appear.
func TestNormalizeMasksVolatileFields(t *testing.T) {
	a := "block trace=0123456789abcdef0123456789abcdef at 2026-07-12T03:30:00Z end"
	b := "block trace=fedcba9876543210fedcba9876543210fedc at 2026-07-13T09:15:42.123456789Z end"
	// b's trace is 36 hex; trim to a valid 32-hex to keep the "differ only in
	// volatile fields" premise exact.
	b = "block trace=fedcba9876543210fedcba9876543210 at 2026-07-13T09:15:42.123456789Z end"

	na, nb := Normalize(a), Normalize(b)
	if na != nb {
		t.Fatalf("strings differing only in trace_id/timestamp did not normalize equal:\n%q\n%q", na, nb)
	}
	if !strings.Contains(na, TraceIDPlaceholder) || !strings.Contains(na, TimestampPlaceholder) {
		t.Fatalf("normalized string missing placeholders: %q", na)
	}
}

// TestNormalizeKeepsCitationDate proves a bare calendar date inside a citation
// path (e.g. [inbox/2026-07-10.md#0]) is NOT masked - it is stable content,
// not a volatile RFC3339 timestamp (no "T HH:MM:SS").
func TestNormalizeKeepsCitationDate(t *testing.T) {
	in := "<hafiza>\n- [inbox/2026-07-10.md#0] not\n</hafiza>"
	if got := Normalize(in); got != in {
		t.Fatalf("Normalize altered a citation date: %q", got)
	}
}

// TestGuardNotProdRefusesProdPath proves GuardNotProd fails closed on the
// production brain.db path and allows a scratch path.
func TestGuardNotProdRefusesProdPath(t *testing.T) {
	prod, err := config.ProdDBPath()
	if err != nil {
		t.Fatalf("ProdDBPath: %v", err)
	}
	if err := GuardNotProd(prod); err == nil {
		t.Fatalf("GuardNotProd(%s) = nil, want refusal", prod)
	}
	scratch := filepath.Join(t.TempDir(), "brain.db")
	if err := GuardNotProd(scratch); err != nil {
		t.Fatalf("GuardNotProd(%s) = %v, want nil", scratch, err)
	}
}
