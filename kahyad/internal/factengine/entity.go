// entity.go implements HANDOFF S5 memory #2 verbatim:
//
//	Bolunebilir, kanit-kapili varlik birlestirme: isim benzerligiyle
//	asla oto-birlestirme (Turkce'de sayisiz Emre/Ahmet). En az bir
//	ayirt edici kanit sart. Merge-defteri + varlik-bolme operasyonu.
//	Supheli ayni-isim -> yeni gecici varlik.
//
// ResolveOrCreateEntity is the ONLY way a runtime candidate ever gets an
// entity row: a brand-new name creates a brand-new entity
// (provisional=0, nothing suspicious about the first-ever registration of
// a name); a name that ALREADY has an alias on file creates a SECOND,
// SEPARATE, provisional=1 entity instead of silently joining the
// existing one - name similarity alone never merges anything (Turkish
// namesakes: countless Emre/Ahmet).
//
// MergeEntities/SplitEntities are the only two operations that ever
// change that fact: Merge requires a real, existing fact id as its
// distinguishing evidence (never optional, never name-similarity alone)
// and records a merge_ledger row carrying a snapshot of exactly which
// alias rows moved; Split reads that SAME snapshot back and restores
// precisely those rows to the entity they came from, recording its own
// merge_ledger row (op='split') - a full round trip is therefore always
// exactly 2 merge_ledger rows (one merge, one split), never fewer, never
// a third.
package factengine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"kahya/kahyad/internal/store/sqlcgen"
)

// ErrMergeRequiresEvidence is returned by MergeEntities when
// evidenceFactID is 0 or does not name an existing fact - HANDOFF S5
// memory #2's "en az bir ayirt edici kanit sart" enforced in Go, not
// merely documented: there is no code path in this package that can
// merge two entities without a real fact backing the decision.
var ErrMergeRequiresEvidence = errors.New("factengine: entity merge requires >=1 distinguishing evidence (a real fact_id)")

// ErrMergeLedgerNotFound is returned by SplitEntities when
// mergeLedgerID does not exist.
var ErrMergeLedgerNotFound = errors.New("factengine: merge_ledger row not found")

// ErrNotAMergeRecord is returned by SplitEntities when the merge_ledger
// row named is itself a split (op='split') - only a merge can be split.
var ErrNotAMergeRecord = errors.New("factengine: merge_ledger row is not a merge (cannot split a split)")

// entityMergeSnapshot is merge_ledger.evidence's JSON payload for an
// op='merge' row: exactly which alias ROW IDS moved off SrcEntityID onto
// DstEntityID, so SplitEntities can move precisely that set back - a bulk
// "every alias DstEntityID currently owns" reassignment would be wrong
// once DstEntityID also carries its own pre-existing aliases, which a
// mere entity_id-keyed WHERE clause could no longer distinguish from the
// merged-in ones.
type entityMergeSnapshot struct {
	SrcEntityID    int64   `json:"src_entity_id"`
	DstEntityID    int64   `json:"dst_entity_id"`
	SrcAliasIDs    []int64 `json:"src_alias_ids"`
	EvidenceFactID int64   `json:"evidence_fact_id"`
}

// ResolveOrCreateEntity looks up name via entity_aliases; if NO entity is
// already registered under it, this is the first-ever sighting and gets a
// fresh, non-provisional entity. If one or more entities ALREADY carry
// this alias, HANDOFF S5 memory #2 forbids joining any of them on name
// alone - a NEW, provisional=1 entity is created instead, leaving
// disambiguation to a later MergeEntities call backed by real evidence.
func (e *Engine) ResolveOrCreateEntity(ctx context.Context, name, kind string) (entityID int64, provisional bool, err error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, false, errors.New("factengine: entity name is required")
	}

	existingIDs, err := e.store.ListEntityIDsByAlias(ctx, name)
	if err != nil {
		return 0, false, fmt.Errorf("factengine: lookup alias %q: %w", name, err)
	}
	prov := int64(0)
	if len(existingIDs) > 0 {
		prov = 1
	}

	now := e.nowRFC3339()
	ent, err := e.store.InsertEntity(ctx, sqlcgen.InsertEntityParams{
		CanonicalName: name, Kind: nullString(kind), Status: "active", Provisional: prov, CreatedAt: now,
	})
	if err != nil {
		return 0, false, fmt.Errorf("factengine: insert entity %q: %w", name, err)
	}
	if _, err := e.store.InsertEntityAlias(ctx, sqlcgen.InsertEntityAliasParams{
		EntityID: ent.ID, Alias: name, CreatedAt: now,
	}); err != nil {
		return 0, false, fmt.Errorf("factengine: insert alias for entity %d: %w", ent.ID, err)
	}
	return ent.ID, prov == 1, nil
}

// MergeEntities merges srcEntityID into dstEntityID: every alias row
// currently owned by srcEntityID moves onto dstEntityID, srcEntityID is
// marked status='merged' (never deleted), and one merge_ledger row
// (op='merge') is written carrying a restorable snapshot. evidenceFactID
// MUST name an existing fact - HANDOFF S5 memory #2's distinguishing-
// evidence requirement (shared unique attribute, unambiguous co-
// reference, ...) is asserted by the CALLER choosing which fact to cite;
// this function's own job is only to refuse a merge with none at all.
func (e *Engine) MergeEntities(ctx context.Context, traceID string, dstEntityID, srcEntityID, evidenceFactID int64, actor string) (mergeLedgerID int64, err error) {
	if evidenceFactID == 0 {
		return 0, ErrMergeRequiresEvidence
	}
	if dstEntityID == srcEntityID {
		return 0, errors.New("factengine: cannot merge an entity into itself")
	}
	if _, err := e.store.GetFact(ctx, evidenceFactID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrMergeRequiresEvidence
		}
		return 0, fmt.Errorf("factengine: merge evidence lookup: %w", err)
	}
	if _, err := e.store.GetEntity(ctx, dstEntityID); err != nil {
		return 0, fmt.Errorf("factengine: merge: entity %d: %w", dstEntityID, err)
	}
	if _, err := e.store.GetEntity(ctx, srcEntityID); err != nil {
		return 0, fmt.Errorf("factengine: merge: entity %d: %w", srcEntityID, err)
	}

	srcAliasRows, err := e.store.ListEntityAliasesByEntity(ctx, srcEntityID)
	if err != nil {
		return 0, fmt.Errorf("factengine: list aliases for entity %d: %w", srcEntityID, err)
	}
	srcAliasIDs := make([]int64, len(srcAliasRows))
	for i, a := range srcAliasRows {
		srcAliasIDs[i] = a.ID
	}

	for _, aliasID := range srcAliasIDs {
		if err := e.store.UpdateEntityAliasEntityByID(ctx, sqlcgen.UpdateEntityAliasEntityByIDParams{
			EntityID: dstEntityID, ID: aliasID,
		}); err != nil {
			return 0, fmt.Errorf("factengine: reassign alias %d to entity %d: %w", aliasID, dstEntityID, err)
		}
	}
	if err := e.store.UpdateEntityStatus(ctx, sqlcgen.UpdateEntityStatusParams{Status: "merged", ID: srcEntityID}); err != nil {
		return 0, fmt.Errorf("factengine: mark entity %d merged: %w", srcEntityID, err)
	}

	snap := entityMergeSnapshot{
		SrcEntityID: srcEntityID, DstEntityID: dstEntityID,
		SrcAliasIDs: srcAliasIDs, EvidenceFactID: evidenceFactID,
	}
	snapBytes, err := json.Marshal(snap)
	if err != nil {
		return 0, fmt.Errorf("factengine: marshal merge snapshot: %w", err)
	}

	row, err := e.store.InsertMergeLedger(ctx, sqlcgen.InsertMergeLedgerParams{
		Op:          "merge",
		SrcEntityID: sql.NullInt64{Int64: srcEntityID, Valid: true},
		DstEntityID: sql.NullInt64{Int64: dstEntityID, Valid: true},
		Evidence:    sql.NullString{String: string(snapBytes), Valid: true},
		Actor:       actor,
		CreatedAt:   e.nowRFC3339(),
	})
	if err != nil {
		return 0, fmt.Errorf("factengine: insert merge_ledger: %w", err)
	}

	if e.ledger != nil {
		_ = e.ledger.LogEvent(ctx, traceID, EventEntityMerged, map[string]any{
			"merge_ledger_id": row.ID, "src_entity_id": srcEntityID, "dst_entity_id": dstEntityID,
			"evidence_fact_id": evidenceFactID, "actor": actor,
		})
	}
	return row.ID, nil
}

// SplitEntities reverses the merge recorded at mergeLedgerID: every alias
// row the original merge snapshot moved is moved back onto its original
// (source) entity, that entity is reactivated (status='active'), and a
// NEW merge_ledger row (op='split') is written - the merge row itself is
// never modified (append-only, matching every other ledger in this
// codebase). A round trip (merge then split) therefore always leaves
// exactly 2 merge_ledger rows total.
func (e *Engine) SplitEntities(ctx context.Context, traceID string, mergeLedgerID int64, actor string) (splitLedgerID int64, err error) {
	row, err := e.store.GetMergeLedger(ctx, mergeLedgerID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrMergeLedgerNotFound
		}
		return 0, fmt.Errorf("factengine: get merge_ledger %d: %w", mergeLedgerID, err)
	}
	if row.Op != "merge" {
		return 0, ErrNotAMergeRecord
	}
	if !row.Evidence.Valid || row.Evidence.String == "" {
		return 0, fmt.Errorf("factengine: merge_ledger %d has no snapshot to restore", mergeLedgerID)
	}
	var snap entityMergeSnapshot
	if err := json.Unmarshal([]byte(row.Evidence.String), &snap); err != nil {
		return 0, fmt.Errorf("factengine: decode merge_ledger %d snapshot: %w", mergeLedgerID, err)
	}

	for _, aliasID := range snap.SrcAliasIDs {
		if err := e.store.UpdateEntityAliasEntityByID(ctx, sqlcgen.UpdateEntityAliasEntityByIDParams{
			EntityID: snap.SrcEntityID, ID: aliasID,
		}); err != nil {
			return 0, fmt.Errorf("factengine: restore alias %d to entity %d: %w", aliasID, snap.SrcEntityID, err)
		}
	}
	if err := e.store.UpdateEntityStatus(ctx, sqlcgen.UpdateEntityStatusParams{Status: "active", ID: snap.SrcEntityID}); err != nil {
		return 0, fmt.Errorf("factengine: reactivate entity %d: %w", snap.SrcEntityID, err)
	}

	splitEvidence, err := json.Marshal(map[string]int64{"restored_merge_ledger_id": mergeLedgerID})
	if err != nil {
		return 0, fmt.Errorf("factengine: marshal split evidence: %w", err)
	}
	splitRow, err := e.store.InsertMergeLedger(ctx, sqlcgen.InsertMergeLedgerParams{
		Op:          "split",
		SrcEntityID: sql.NullInt64{Int64: snap.DstEntityID, Valid: true},
		DstEntityID: sql.NullInt64{Int64: snap.SrcEntityID, Valid: true},
		Evidence:    sql.NullString{String: string(splitEvidence), Valid: true},
		Actor:       actor,
		CreatedAt:   e.nowRFC3339(),
	})
	if err != nil {
		return 0, fmt.Errorf("factengine: insert split merge_ledger row: %w", err)
	}

	if e.ledger != nil {
		_ = e.ledger.LogEvent(ctx, traceID, EventEntitySplit, map[string]any{
			"merge_ledger_id": splitRow.ID, "restored_merge_ledger_id": mergeLedgerID,
			"src_entity_id": snap.DstEntityID, "dst_entity_id": snap.SrcEntityID, "actor": actor,
		})
	}
	return splitRow.ID, nil
}
