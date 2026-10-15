package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// GraphReader is kahyad's graph-read seam for the two structured memory
// tools (FIX-MEM-4): kahyad/internal/server adapts kahyad/internal/graph
// behind it (the import boundary, see the package doc in render.go). Every
// result it returns has already been filtered for the caller's lane (a
// worker is a cloud session: a secret-path fact never comes back).
type GraphReader interface {
	LookupEntities(ctx context.Context, traceID, name string, k int) ([]EntityRecord, error)
	QueryFacts(ctx context.Context, traceID string, q FactQuery) ([]FactRecord, error)
}

// ErrGraphUnavailable is entity_lookup / fact_query's answer when no graph
// is wired (byte-exact Turkish). The tools are registered regardless, so
// tools/list never changes shape.
var ErrGraphUnavailable = errors.New("graf katmanı bu kurulumda kullanılamıyor")

// ErrFactQueryBadTime is fact_query's answer to a since/until/as_of/
// known_as_of that is not RFC3339.
var ErrFactQueryBadTime = errors.New("fact_query: since/until/as_of/known_as_of must be RFC3339")

// Tool result bounds.
const (
	DefaultEntityLookupK = 10
	MaxEntityLookupK     = 50
	DefaultFactQueryK    = 20
	MaxFactQueryK        = 100
)

// EntityLookupArgs is entity_lookup's input.
type EntityLookupArgs struct {
	Name string `json:"name" jsonschema:"varlik adi ya da takma adi (on-ek eslesir, buyuk/kucuk harf duyarsiz)"`
	K    int    `json:"k,omitempty" jsonschema:"en fazla kac varlik (varsayilan 10)"`
}

// EntityRecord is one entity_lookup result.
type EntityRecord struct {
	EntityID      int64    `json:"entity_id"`
	CanonicalName string   `json:"canonical_name"`
	Kind          string   `json:"kind"`
	Provisional   bool     `json:"provisional"`
	Aliases       []string `json:"aliases"`
	FactCount     int64    `json:"fact_count"`
	// TopFacts are at most 5 of the entity's injection-eligible facts
	// (never a quarantined one), highest confidence first.
	TopFacts []FactRecord `json:"top_facts"`
}

// EntityLookupOutput is entity_lookup's output.
type EntityLookupOutput struct {
	Entities []EntityRecord `json:"entities"`
}

// FactQueryArgs is fact_query's input. Every filter is optional; the
// times are RFC3339.
type FactQueryArgs struct {
	Subject   string `json:"subject,omitempty" jsonschema:"ozne (bayt-tam, bossa katlanmis esitlik; Turkce cekimler icin entity_id tercih et)"`
	Predicate string `json:"predicate,omitempty" jsonschema:"yuklem, orn. works_at"`
	Object    string `json:"object,omitempty" jsonschema:"nesne (bayt-tam, bossa katlanmis esitlik)"`
	EntityID  int64  `json:"entity_id,omitempty" jsonschema:"entity_lookup'tan gelen varlik id; ozne ya da nesne olarak o varligi anan olgular"`
	Since     string `json:"since,omitempty" jsonschema:"RFC3339; valid_from >= since"`
	Until     string `json:"until,omitempty" jsonschema:"RFC3339; valid_from < until"`
	AsOf      string `json:"as_of,omitempty" jsonschema:"RFC3339; bu anda GECERLI olan olgular (valid_from <= as_of < valid_to)"`
	KnownAsOf string `json:"known_as_of,omitempty" jsonschema:"RFC3339; bu tarihte BILINEN olgular (created_at <= known_as_of)"`
	MinTier   string `json:"min_tier,omitempty" jsonschema:"user_edit|user_asserted|external_doc|screen|agent_derived - en dusuk kabul edilen guven katmani"`
	// IncludeQuarantined (decision MEM-4.a) also returns the facts the
	// trust filter would hide - unconfirmed agent_derived ones and ones
	// under the injection threshold - each labelled Quarantined. The
	// results are a tool answer only, never written into <hafiza> or a
	// profile card (G00 H-6).
	IncludeQuarantined bool `json:"include_quarantined,omitempty" jsonschema:"true ise onaysiz ajan cikarimlari (agent_derived, karantina) da doner - etiketli"`
	K                  int  `json:"k,omitempty" jsonschema:"en fazla kac olgu (varsayilan 20, en cok 100)"`
}

// FactQuery is a validated fact_query: the args with parsed times.
type FactQuery struct {
	Subject, Predicate, Object string
	EntityID                   int64
	Since, Until, AsOf         time.Time
	KnownAsOf                  time.Time
	MinTier                    string
	IncludeQuarantined         bool
	K                          int
}

// FactRecord is one fact as the structured tools return it.
type FactRecord struct {
	FactID        int64   `json:"fact_id"`
	Subject       string  `json:"subject"`
	Predicate     string  `json:"predicate"`
	Object        string  `json:"object"`
	SourceTier    string  `json:"source_tier"`
	Evidentiality string  `json:"evidentiality"`
	ConfidenceP   float64 `json:"confidence_p"`
	// Quarantined labels a fact the injection trust filter excludes
	// (unconfirmed agent_derived, or under p=0.3); only an
	// include_quarantined query returns one.
	Quarantined bool   `json:"quarantined"`
	ValidFrom   string `json:"valid_from,omitempty"`
	ValidTo     string `json:"valid_to,omitempty"`
	// KnownSince is facts.created_at (transaction time).
	KnownSince      string `json:"known_since"`
	Quote           string `json:"quote,omitempty"`
	Evidence        string `json:"evidence,omitempty"`
	SubjectEntityID int64  `json:"subject_entity_id,omitempty"`
	ObjectEntityID  int64  `json:"object_entity_id,omitempty"`
}

// FactQueryOutput is fact_query's output.
type FactQueryOutput struct {
	Facts []FactRecord `json:"facts"`
}

// SetGraph wires the structured tools' graph reader (kahyad: in
// buildMCPHandler). nil = both tools answer ErrGraphUnavailable.
func (s *Server) SetGraph(g GraphReader) { s.Graph = g }

// HandleEntityLookup implements entity_lookup.
func (s *Server) HandleEntityLookup(ctx context.Context, traceID string, args EntityLookupArgs) (EntityLookupOutput, error) {
	if s.Graph == nil {
		return EntityLookupOutput{}, ErrGraphUnavailable
	}
	name := strings.TrimSpace(args.Name)
	if name == "" {
		return EntityLookupOutput{}, errors.New("entity_lookup: name must not be empty")
	}
	k := clampK(args.K, DefaultEntityLookupK, MaxEntityLookupK)
	ents, err := s.Graph.LookupEntities(ctx, traceID, name, k)
	if err != nil {
		return EntityLookupOutput{}, fmt.Errorf("entity_lookup: %w", err)
	}
	if ents == nil {
		ents = []EntityRecord{}
	}
	return EntityLookupOutput{Entities: ents}, nil
}

// HandleFactQuery implements fact_query.
func (s *Server) HandleFactQuery(ctx context.Context, traceID string, args FactQueryArgs) (FactQueryOutput, error) {
	if s.Graph == nil {
		return FactQueryOutput{}, ErrGraphUnavailable
	}
	q := FactQuery{
		Subject: strings.TrimSpace(args.Subject), Predicate: strings.TrimSpace(args.Predicate), Object: strings.TrimSpace(args.Object),
		EntityID: args.EntityID, MinTier: strings.TrimSpace(args.MinTier), IncludeQuarantined: args.IncludeQuarantined,
		K: clampK(args.K, DefaultFactQueryK, MaxFactQueryK),
	}
	for _, tf := range []struct {
		raw string
		dst *time.Time
	}{{args.Since, &q.Since}, {args.Until, &q.Until}, {args.AsOf, &q.AsOf}, {args.KnownAsOf, &q.KnownAsOf}} {
		raw := strings.TrimSpace(tf.raw)
		if raw == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return FactQueryOutput{}, ErrFactQueryBadTime
		}
		*tf.dst = t
	}
	facts, err := s.Graph.QueryFacts(ctx, traceID, q)
	if err != nil {
		return FactQueryOutput{}, fmt.Errorf("fact_query: %w", err)
	}
	if facts == nil {
		facts = []FactRecord{}
	}
	return FactQueryOutput{Facts: facts}, nil
}

func clampK(k, def, max int) int {
	if k <= 0 {
		return def
	}
	if k > max {
		return max
	}
	return k
}
