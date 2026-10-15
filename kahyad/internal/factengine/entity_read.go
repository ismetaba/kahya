// entity_read.go is the read side of the entity layer (G13 MEM-2 phase 2):
// `kahya entity list [<ad>] [--provisional]` and `kahya entity show <id>`,
// over GET /v1/entity/list and GET /v1/entity/show/{id}. Read-only - the
// only writers of entities/entity_aliases stay ResolveOrCreateEntity,
// ResolveEntityForExtraction, AddAlias and Merge/SplitEntities. The output
// is a LOCAL surface (the CLI), never Telegram or a model prompt, so
// quarantined (unconfirmed agent_derived) facts are listed too, labelled.
package factengine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"

	"kahya/kahyad/internal/store/sqlcgen"
)

// Entity listing bounds.
const (
	DefaultEntityListLimit = 50
	MaxEntityListLimit     = 200
	// EntityShowFactLimit caps the facts ShowEntity returns (highest
	// confidence first).
	EntityShowFactLimit = 100
)

// ErrEntityNotFound is returned by ShowEntity for an unknown entity id.
var ErrEntityNotFound = errors.New("factengine: entity not found")

// EntitySummary is one `kahya entity list` row.
type EntitySummary struct {
	ID            int64    `json:"id"`
	CanonicalName string   `json:"canonical_name"`
	Kind          string   `json:"kind"`
	Status        string   `json:"status"`
	Provisional   bool     `json:"provisional"`
	Aliases       []string `json:"aliases"`
	ActiveFacts   int64    `json:"active_facts"`
}

// EntityFact is one fact on an entity, as `kahya entity show` prints it.
// ConfidenceP is the probability form of the log-odds confidence;
// Quarantined marks an agent_derived fact nobody confirmed yet (HANDOFF §5
// memory #1 - never injected, but the user may look at it).
type EntityFact struct {
	FactID        int64   `json:"fact_id"`
	Subject       string  `json:"subject"`
	Predicate     string  `json:"predicate"`
	Object        string  `json:"object"`
	SourceTier    string  `json:"source_tier"`
	Evidentiality string  `json:"evidentiality"`
	ConfidenceP   float64 `json:"confidence_p"`
	Quarantined   bool    `json:"quarantined"`
	ValidFrom     string  `json:"valid_from,omitempty"`
	ValidTo       string  `json:"valid_to,omitempty"`
}

// EntityDetail is `kahya entity show`'s payload.
type EntityDetail struct {
	EntitySummary
	Facts []EntityFact `json:"facts"`
}

// ListEntities lists active entities whose folded alias starts with
// AliasKey(prefix) ("" = every entity), at most limit (<= 0 =
// DefaultEntityListLimit, capped at MaxEntityListLimit), lowest id first;
// provisionalOnly keeps only provisional (suspected namesake) entities.
func (e *Engine) ListEntities(ctx context.Context, prefix string, provisionalOnly bool, limit int) ([]EntitySummary, error) {
	if limit <= 0 {
		limit = DefaultEntityListLimit
	}
	if limit > MaxEntityListLimit {
		limit = MaxEntityListLimit
	}
	like := sql.NullString{String: AliasKey(prefix) + "%", Valid: true}
	type row struct {
		id          int64
		name        string
		kind        sql.NullString
		status      string
		provisional int64
	}
	var rows []row
	if provisionalOnly {
		got, err := e.store.ListProvisionalEntitiesByFolded(ctx, sqlcgen.ListProvisionalEntitiesByFoldedParams{AliasFolded: like, Limit: int64(limit)})
		if err != nil {
			return nil, fmt.Errorf("factengine: list provisional entities: %w", err)
		}
		for _, r := range got {
			rows = append(rows, row{r.ID, r.CanonicalName, r.Kind, r.Status, r.Provisional})
		}
	} else {
		got, err := e.store.ListEntitiesByFolded(ctx, sqlcgen.ListEntitiesByFoldedParams{AliasFolded: like, Limit: int64(limit)})
		if err != nil {
			return nil, fmt.Errorf("factengine: list entities: %w", err)
		}
		for _, r := range got {
			rows = append(rows, row{r.ID, r.CanonicalName, r.Kind, r.Status, r.Provisional})
		}
	}
	out := make([]EntitySummary, 0, len(rows))
	for _, r := range rows {
		s, err := e.entitySummary(ctx, r.id, r.name, r.kind, r.status, r.provisional)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// ShowEntity returns entity id with its aliases and at most
// EntityShowFactLimit active facts touching it (as subject or object,
// highest confidence first), quarantined ones labelled. Any status is shown
// (a merged-away entity keeps its id). ErrEntityNotFound for an unknown id.
func (e *Engine) ShowEntity(ctx context.Context, id int64) (EntityDetail, error) {
	ent, err := e.store.GetEntity(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return EntityDetail{}, fmt.Errorf("%w: %d", ErrEntityNotFound, id)
	}
	if err != nil {
		return EntityDetail{}, fmt.Errorf("factengine: entity %d: %w", id, err)
	}
	s, err := e.entitySummary(ctx, ent.ID, ent.CanonicalName, ent.Kind, ent.Status, ent.Provisional)
	if err != nil {
		return EntityDetail{}, err
	}
	ref := sql.NullInt64{Int64: id, Valid: true}
	facts, err := e.store.ListFactsByEntityHop(ctx, sqlcgen.ListFactsByEntityHopParams{SubjectEntityID: ref, ObjectEntityID: ref, Limit: EntityShowFactLimit})
	if err != nil {
		return EntityDetail{}, fmt.Errorf("factengine: facts of entity %d: %w", id, err)
	}
	d := EntityDetail{EntitySummary: s, Facts: make([]EntityFact, 0, len(facts))}
	for _, f := range facts {
		d.Facts = append(d.Facts, EntityFact{
			FactID: f.ID, Subject: f.Subject, Predicate: f.Predicate, Object: f.Object,
			SourceTier: f.SourceTier, Evidentiality: f.Evidentiality,
			ConfidenceP: LogOddsToProbability(f.Confidence),
			Quarantined: f.SourceTier == TierAgentDerived && !f.ConfirmedAt.Valid,
			ValidFrom:   f.ValidFrom.String, ValidTo: f.ValidTo.String,
		})
	}
	return d, nil
}

func (e *Engine) entitySummary(ctx context.Context, id int64, name string, kind sql.NullString, status string, provisional int64) (EntitySummary, error) {
	aliases, err := e.store.ListEntityAliasesByEntity(ctx, id)
	if err != nil {
		return EntitySummary{}, fmt.Errorf("factengine: aliases of entity %d: %w", id, err)
	}
	names := make([]string, 0, len(aliases))
	for _, a := range aliases {
		names = append(names, a.Alias)
	}
	ref := sql.NullInt64{Int64: id, Valid: true}
	n, err := e.store.CountActiveFactsForEntity(ctx, sqlcgen.CountActiveFactsForEntityParams{SubjectEntityID: ref, ObjectEntityID: ref})
	if err != nil {
		return EntitySummary{}, fmt.Errorf("factengine: fact count of entity %d: %w", id, err)
	}
	return EntitySummary{
		ID: id, CanonicalName: name, Kind: kind.String, Status: status,
		Provisional: provisional != 0, Aliases: names, ActiveFacts: n,
	}, nil
}

// LogOddsToProbability is p = 1/(1+exp(-logOdds)) - the probability form
// of facts.confidence.
func LogOddsToProbability(logOdds float64) float64 {
	return 1 / (1 + math.Exp(-logOdds))
}
