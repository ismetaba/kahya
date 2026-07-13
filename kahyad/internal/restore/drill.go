// Package restore implements the W78-05 backup restore drill: the reusable
// logic that proves a restored brain.db yields the SAME <hafiza> injection
// for the same query as production, and that the ledger (events) / episodes -
// which are NOT derivable from the markdown corpus (tasks/README.md, HANDOFF
// S5 memory #3) - survived the VACUUM copy.
//
// This package deliberately owns only the PURE, side-effect-free pieces the
// drill drives:
//
//   - RunEquivalence: replicates kahyad/internal/server.handleMemorySearch's
//     for_injection=true path in-process (search.Searcher over brain.db ->
//     factengine.TierInjectionEligible filter -> mcp/memory.RenderKept), so a
//     test (drill_test.go) can build the whole BUILD -> BACKUP -> RESTORE ->
//     REINDEX -> COMPARE cycle hermetically over temp/scratch dbs without
//     booting a child kahyad. The block RunEquivalence returns is byte-exact
//     what the server's /v1/memory/search for_injection response carries and
//     ledgers (HANDOFF S5 safety #4), because it goes through the SAME
//     search+memory packages the server does.
//   - Normalize: the NARROW volatile-field masker (trace_id + RFC3339/Nano
//     timestamps only) applied to both blocks before the byte-compare, so a
//     spurious volatile difference never fails the drill while any REAL
//     content difference (a changed/added/removed chunk) still does.
//   - LedgerCounts: the events/episodes row counts the "ledger survives"
//     assertion compares.
//   - GuardNotProd: the fail-closed rule the live scripts/restore-drill.sh
//     mirrors in bash - refuse to touch the production brain.db path.
//
// kahyad remains the ONLY writer of the production brain.db (HANDOFF S4/S5):
// nothing in this package writes the production database. The live drill
// reports its result to the running production kahyad over the UDS
// (POST /v1/restore/drill-result), which records EventRestoreDrillResult via
// kahyad's own event ledger. The hermetic test asserts only that endpoint's
// payload SHAPE (kahyad/internal/server/restoredrill_test.go).
package restore

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/factengine"
	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/search"

	"kahya/mcp/memory"
)

// EventRestoreDrillResult is the PRODUCTION-ledger event kind the live drill
// (scripts/restore-drill.sh) reports to the running production kahyad over its
// UDS after a successful run (kahyad/internal/server's POST
// /v1/restore/drill-result handler records it). Its payload carries only
// counts/hashes/flags (ok, ref_query_sha, backup_file, trace_id) - NEVER any
// memory content - and kahyad, the sole brain.db writer, is what appends it.
// This row is the evidence W78-06 readiness reads.
const EventRestoreDrillResult = "restore.drill.result"

// traceIDPattern matches this codebase's trace_id shape EXACTLY: 32 lowercase
// hex characters (kahyad/internal/traceid.New: "16 random bytes, rendered as
// 32 lowercase hex characters"), bounded by \b so it never nibbles a longer
// hex run or a hex substring inside a larger token. Deliberately narrow: a
// real memory chunk essentially never contains a bare 32-hex token, and the
// narrowness proof (drill_test.go TestNormalizeDoesNotMaskContentDiff) mutates
// ORDINARY Turkish prose - which this pattern leaves untouched - to confirm a
// genuine content change still fails the compare.
var traceIDPattern = regexp.MustCompile(`\b[0-9a-f]{32}\b`)

// timestampPattern matches an RFC3339 / RFC3339Nano timestamp
// (2006-01-02T15:04:05Z, ...T15:04:05.999999999Z, or with a numeric +hh:mm /
// -hh:mm zone) - the exact shape every ledger writer in this codebase emits
// (kahyad/internal/store.InsertEventWithDigest uses time.RFC3339Nano). The
// mandatory literal "T" plus "HH:MM:SS" is what keeps this from matching a
// bare calendar date such as the "2026-07-10" inside a citation path
// ([inbox/2026-07-10.md#0]) - those are stable content, not volatile fields,
// and must survive normalization intact.
var timestampPattern = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})`)

// TraceIDPlaceholder / TimestampPlaceholder are the fixed tokens Normalize
// substitutes the two volatile shapes with. Exported so a test can assert the
// masking happened (rather than re-deriving the literal).
const (
	TraceIDPlaceholder   = "<TRACE_ID>"
	TimestampPlaceholder = "<TS>"
)

// Normalize replaces ONLY the two volatile field shapes - RFC3339/Nano
// timestamps and 32-hex trace_ids - in block with fixed placeholders, so the
// drill can byte-compare a restored <hafiza> block against the production
// reference without a spurious timestamp/trace_id difference failing it. It
// masks NOTHING else: any change to a chunk's path, seq, or prose text remains
// visible through Normalize, so a real markdown<->index drift (or a poisoned/
// altered chunk) still fails the equivalence compare (the whole point of the
// drill). Timestamps are masked before trace_ids purely for readability of the
// two independent, non-overlapping substitutions; order does not affect the
// result.
func Normalize(block string) string {
	b := timestampPattern.ReplaceAllString(block, TimestampPlaceholder)
	b = traceIDPattern.ReplaceAllString(b, TraceIDPlaceholder)
	return b
}

// InjectionResult is RunEquivalence's output: the exact <hafiza> block the
// server's for_injection path would return/ledger for the same query, plus the
// chunk ids that actually survived RenderKept's top-k/budget trim (the same
// subset the server records in a hafiza_injected event's chunk_ids field).
type InjectionResult struct {
	Block    string
	ChunkIDs []int64
}

// RunEquivalence runs the equivalence query through brain.db's REAL injection
// path, in-process, exactly as kahyad/internal/server.handleMemorySearch does
// for a for_injection:true request:
//
//  1. search.New(db, log, search.DefaultConfig()).Search(query, k) - with NO
//     embedder wired, so the vector leg degrades to FTS-only (the documented
//     degrade when the MLX embedder is unwired - this drill needs no worker,
//     no cloud, no MLX; the block is PURE retrieval);
//  2. drop every hit that is NOT injection-eligible by tier
//     (factengine.TierInjectionEligible with confirmed=false - a chunk has no
//     per-item human confirmation, only facts do), i.e. quarantine
//     'agent_derived' episodes (HANDOFF S5 memory #1);
//  3. mcp/memory.RenderKept(hits, memory.DefaultTopK) - the identical renderer
//     (DefaultTopK=6, MaxBlockRunes=4000) the server uses.
//
// The returned Block is therefore byte-identical to what the server would
// inject/ledger for the same query against the same db, which is what makes a
// production-vs-restored byte-compare (after Normalize) a faithful equivalence
// proof. traceID "" is fine - it only scopes a JSONL log line here, never the
// block content.
func RunEquivalence(ctx context.Context, db *sql.DB, log *logx.Logger, query string, k int) (InjectionResult, error) {
	searcher := search.New(db, log, search.DefaultConfig())
	hits, err := searcher.Search(ctx, "", query, k)
	if err != nil {
		return InjectionResult{}, fmt.Errorf("restore: equivalence search %q: %w", query, err)
	}

	memHits := make([]memory.Hit, 0, len(hits))
	for _, h := range hits {
		if !factengine.TierInjectionEligible(h.SourceTier, false) {
			continue
		}
		memHits = append(memHits, memory.Hit{
			ChunkID:    h.ChunkID,
			Path:       h.Path,
			Seq:        h.Seq,
			Text:       h.Text,
			Score:      h.Score,
			SourceTier: h.SourceTier,
		})
	}

	block, kept := memory.RenderKept(memHits, memory.DefaultTopK)
	chunkIDs := make([]int64, len(kept))
	for i, h := range kept {
		chunkIDs[i] = h.ChunkID
	}
	return InjectionResult{Block: block, ChunkIDs: chunkIDs}, nil
}

// LedgerCounts returns the total events and episodes row counts on db - the
// two figures the drill's "ledger survives" assertion compares (restored >=
// reference). These tables are the ONE part of brain.db that cannot be
// rebuilt from the markdown corpus (tasks/README.md: "the ledger (events) and
// episodes are the exception - they exist only in brain.db"), so their
// survival through the VACUUM copy is exactly what a restore drill must prove.
func LedgerCounts(ctx context.Context, db *sql.DB) (events, episodes int64, err error) {
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM events`).Scan(&events); err != nil {
		return 0, 0, fmt.Errorf("restore: count events: %w", err)
	}
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM episodes`).Scan(&episodes); err != nil {
		return 0, 0, fmt.Errorf("restore: count episodes: %w", err)
	}
	return events, episodes, nil
}

// GuardNotProd fails closed when dbPath is the production brain.db path (the
// exact path a KAHYA_ENV=prod Load resolves to, via config.ProdDBPath). The
// drill NEVER opens the production database - it works only against a scratch
// restore profile (KAHYA_ENV=restore -> ~/Library/Application Support/
// Kahya-restore/brain.db) - so a resolved target equal to prod is a
// misconfiguration that must abort before any open/reindex clobbers real data.
// scripts/restore-drill.sh mirrors this exact rule in bash for the live path;
// this Go copy is the unit-tested statement of the rule (drill_test.go
// TestGuardNotProdRefusesProdPath) and is reusable by any future in-process
// caller. config.refuseNonProdProfileOpeningProdDB already enforces the same
// invariant inside config.Load for a non-prod KAHYA_ENV; this is the belt to
// that suspenders.
func GuardNotProd(dbPath string) error {
	prod, err := config.ProdDBPath()
	if err != nil {
		return fmt.Errorf("restore: resolve prod db path for guard: %w", err)
	}
	if dbPath == prod {
		return fmt.Errorf("restore: refusing to run the drill against the production brain.db (%s) - the drill must target a KAHYA_ENV=restore scratch profile only", prod)
	}
	return nil
}
