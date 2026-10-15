package search

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Filter narrows SearchFiltered's candidate set (FIX-MEM-4) AFTER fusion and
// BEFORE the recency re-rank and the top-k cut, so k results survive it
// whenever enough candidates match. Every field's zero value means "any".
type Filter struct {
	// Since/Until bound episodes.created_at (when the note was first
	// indexed): Since inclusive, Until exclusive; zero = unbounded. A
	// created_at that does not parse never matches a bounded filter.
	Since, Until time.Time
	// Source is episodes.source, exact ("memory_file", "external_doc").
	Source string
	// PathPrefix is a byte-exact prefix of episodes.source_path (relative to
	// the memory dir, forward slashes, e.g. "people/").
	PathPrefix string
	// EntityID keeps only chunks of episodes an ACTIVE fact touching that
	// entity (subject or object) cites.
	EntityID int64
	// MinTier is the trust-lattice floor: user_edit > user_asserted >
	// external_doc > screen > agent_derived. An unknown value is an error.
	MinTier string
}

// IsZero reports whether f filters nothing.
func (f Filter) IsZero() bool {
	return f.Since.IsZero() && f.Until.IsZero() && f.Source == "" && f.PathPrefix == "" && f.EntityID == 0 && f.MinTier == ""
}

// tierRank is the source-trust lattice (HANDOFF §5 memory #1), highest
// first - the five tiers episodes.source_tier's CHECK constraint allows.
var tierRank = map[string]int{
	"user_edit":     5,
	"user_asserted": 4,
	"external_doc":  3,
	"screen":        2,
	"agent_derived": 1,
}

// ErrUnknownTier is returned for a Filter.MinTier outside the lattice.
var ErrUnknownTier = errors.New("search: min_tier must be one of user_edit, user_asserted, external_doc, screen, agent_derived")

// allowedTiers returns the tiers at or above MinTier (nil = every tier).
func (f Filter) allowedTiers() (map[string]bool, error) {
	if f.MinTier == "" {
		return nil, nil
	}
	floor, ok := tierRank[f.MinTier]
	if !ok {
		return nil, fmt.Errorf("%w (got %q)", ErrUnknownTier, f.MinTier)
	}
	out := map[string]bool{}
	for t, r := range tierRank {
		if r >= floor {
			out[t] = true
		}
	}
	return out, nil
}

// filterBatchSize keeps each IN (...) list well under SQLite's parameter
// limit.
const filterBatchSize = 2000

// applyFilter returns the ids (in their given order) whose chunk passes f.
func (s *Searcher) applyFilter(ctx context.Context, ids []int64, f Filter, tiers map[string]bool) ([]int64, error) {
	keep := make(map[int64]bool, len(ids))
	for start := 0; start < len(ids); start += filterBatchSize {
		end := min(start+filterBatchSize, len(ids))
		if err := s.filterBatch(ctx, ids[start:end], f, tiers, keep); err != nil {
			return nil, err
		}
	}
	out := make([]int64, 0, len(keep))
	for _, id := range ids {
		if keep[id] {
			out = append(out, id)
		}
	}
	return out, nil
}

func (s *Searcher) filterBatch(ctx context.Context, ids []int64, f Filter, tiers map[string]bool, keep map[int64]bool) error {
	placeholders := make([]string, len(ids))
	args := make([]any, 0, len(ids)+3)
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}
	// The entity half is the only one SQL decides (an EXISTS over the
	// evidence/facts join); the rest is compared in Go, where a timestamp
	// is parsed rather than string-compared (RFC3339Nano is not always
	// lexicographically ordered).
	args = append(args, f.EntityID, f.EntityID, f.EntityID)
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT c.id, e.created_at, e.source, e.source_path, e.source_tier
		FROM chunks c
		JOIN episodes e ON e.id = c.episode_id
		WHERE c.id IN (%s)
		  AND (? = 0 OR EXISTS (
		        SELECT 1 FROM evidence ev JOIN facts fa ON fa.id = ev.fact_id
		        WHERE ev.episode_id = e.id AND fa.status = 'active'
		          AND (fa.subject_entity_id = ? OR fa.object_entity_id = ?)))`, strings.Join(placeholders, ",")), args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var createdAt, path sql.NullString
		var source, tier string
		if err := rows.Scan(&id, &createdAt, &source, &path, &tier); err != nil {
			return err
		}
		if f.matches(createdAt.String, source, path.String, tier, tiers) {
			keep[id] = true
		}
	}
	return rows.Err()
}

// matches is the Go half of the filter.
func (f Filter) matches(createdAt, source, path, tier string, tiers map[string]bool) bool {
	if f.Source != "" && source != f.Source {
		return false
	}
	if f.PathPrefix != "" && !strings.HasPrefix(path, f.PathPrefix) {
		return false
	}
	if tiers != nil && !tiers[tier] {
		return false
	}
	if !f.Since.IsZero() || !f.Until.IsZero() {
		t, err := time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return false
		}
		if !f.Since.IsZero() && t.Before(f.Since) {
			return false
		}
		if !f.Until.IsZero() && !t.Before(f.Until) {
			return false
		}
	}
	return true
}
