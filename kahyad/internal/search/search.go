// search.go implements Search: kahyad's fused BM25 ranking over the FTS5
// dual index (HANDOFF §4 stack row ⚑ "FTS5 çift indeks"; §6 W1-2 gate:
// "'evlerimizden' sorgusu 'ev' içeren tohum notu buluyor"). See the package
// doc comment in ftswrite.go for the write side of this index.
package search

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/textnorm"
)

// ErrEmptyQuery is returned by Search when q is empty or all whitespace
// (W12-03 step 4: "empty query -> error, no panic").
var ErrEmptyQuery = errors.New("search: query must not be empty")

// DefaultK is the result count Search falls back to when the caller passes
// k <= 0 (W12-03 step 4: "k=0 -> default 8").
const DefaultK = 8

// Config holds Search's tunable, committed-default fusion parameters
// (HANDOFF §4 ⚑: BM25 fusion; W12-03 step 3c).
type Config struct {
	// TriWeight/UniWeight are the fusion weights: fused =
	// TriWeight*triScore + UniWeight*uniScore, each a per-chunk leg score
	// in [0,1] (a missing leg contributes 0). The unicode61 leg's score is
	// a plain min-max normalization of its raw bm25 rows. The trigram
	// leg's score is either a scan-floor hit (fixed at ScanFloorScore) or
	// a genuine MATCH hit, min-max normalized and then rescaled into
	// [2*ScanFloorScore, 1.0] - see ScanFloorScore below.
	TriWeight float64
	UniWeight float64

	// ScanFloorScore is the FIXED, post-normalization score given to a
	// trigram-leg hit found only by the Go substring scan below the
	// trigram tokenizer's 3-rune floor (W12-03 step 3b). It is injected
	// after normalization, never averaged into it, so a genuine bm25 MATCH
	// hit always outranks a scan-floor hit for the SAME token: the
	// trigram leg's real MATCH scores are rescaled into
	// [2*ScanFloorScore, 1.0] (see rescaleMatchScores), never down to the
	// plain min-max [0,1] range, so even the single worst MATCH hit still
	// scores strictly above every scan-floor hit.
	ScanFloorScore float64

	// UniLimit caps how many unicode61 MATCH rows are considered before
	// fusion (W12-03 step 3a: "take top 50").
	UniLimit int

	// LadderStartMax bounds the trigram MATCH relaxation ladder (BLOCKER
	// 4): after the full token is always tried once, further truncated
	// attempts start at min(len(token)-1, LadderStartMax) runes rather
	// than always len(token)-1. Without this bound, a single pathological
	// token (a URL, a hash, a pasted log line) issues one sequential MATCH
	// query per rune-length down to 3 - thousands of queries against a
	// single-connection db handle (SetMaxOpenConns(1)), serializing out
	// every other request for seconds. Real Turkish words are well under
	// this floor, so natural-language behavior is unchanged.
	LadderStartMax int
}

// DefaultConfig returns the committed-default fusion weights (HANDOFF §4:
// "fused = 0.6*tri + 0.4*uni").
func DefaultConfig() Config {
	return Config{
		TriWeight:      0.6,
		UniWeight:      0.4,
		ScanFloorScore: 0.1,
		UniLimit:       50,
		LadderStartMax: 32,
	}
}

// Hit is one ranked chunk returned by Search.
type Hit struct {
	ChunkID   int64
	EpisodeID int64
	Path      string
	// Seq is the chunk's 0-based sequence number within its episode
	// (chunks.seq). W12-05's <hafiza> renderer cites hits as
	// "[<path>#<seq>]", so Search must surface it alongside Path/Text.
	Seq        int64
	Text       string
	Score      float64
	SourceTier string
}

// Searcher runs fused BM25 search over chunks_fts_tri/chunks_fts_uni. Build
// one with New and reuse it - it is safe for concurrent use (every method
// only reads brain.db; the FTS5 writes live in ftswrite.go and always run
// inside the same transaction as the chunks write, elsewhere).
type Searcher struct {
	db  *sql.DB
	log *logx.Logger
	cfg Config
}

// New constructs a Searcher. log is the boot-scoped logger; each Search
// call scopes a child logger to that call's trace_id via
// kahyad/internal/logx.Logger.With.
func New(db *sql.DB, log *logx.Logger, cfg Config) *Searcher {
	return &Searcher{db: db, log: log, cfg: cfg}
}

// Search returns up to k chunks ranked by fused BM25 score for q (W12-03
// step 3). traceID scopes this call's JSONL log line ("" mints a fresh
// one, matching kahyad/internal/logx.Logger.With's own fallback). The
// query TEXT is only ever logged at debug level - the "memory_search" info
// line carries query length, never the query itself (HANDOFF §4 ⚑ logging
// invariant: this endpoint may see sensitive content).
func (s *Searcher) Search(ctx context.Context, traceID, q string, k int) ([]Hit, error) {
	start := time.Now()
	log := s.log.With(traceID)

	if strings.TrimSpace(q) == "" {
		return nil, ErrEmptyQuery
	}
	if k <= 0 {
		k = DefaultK
	}

	log.Debug("memory_search_query", "query", q)

	uniScores, uniHits, err := s.uniLeg(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("search: unicode61 leg: %w", err)
	}

	triScores, matchHits, scanHits, err := s.triLeg(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("search: trigram leg: %w", err)
	}

	fused := fuse(s.cfg, triScores, uniScores)

	ids := make([]int64, 0, len(fused))
	for id := range fused {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		si, sj := fused[ids[i]], fused[ids[j]]
		if si != sj {
			return si > sj
		}
		return ids[i] > ids[j] // tie-break: chunks.id desc
	})
	if len(ids) > k {
		ids = ids[:k]
	}

	hits, err := s.loadHits(ctx, ids, fused)
	if err != nil {
		return nil, fmt.Errorf("search: load hit rows: %w", err)
	}

	log.Info("memory_search",
		"query_len", len([]rune(q)),
		"k", k,
		"uni_hits", uniHits,
		"tri_match_hits", matchHits,
		"tri_scan_hits", scanHits,
		"result_count", len(hits),
		"duration_ms", time.Since(start).Milliseconds(),
	)
	return hits, nil
}

// uniLeg runs the unicode61 leg (W12-03 step 3a): every whitespace token of
// q, individually phrase-quoted and ANDed together (FTS5's default boolean
// between space-separated MATCH terms), against the top cfg.UniLimit rows
// by bm25. Returns the negated (higher=better), min-max normalized score
// per chunk_id plus the raw row count for logging. q is used as-is
// (unfolded): unicode61 is the EXACT term/code leg (HANDOFF §4: "kesin
// terim/kod") - Turkish I/ı folding is the trigram leg's job.
func (s *Searcher) uniLeg(ctx context.Context, q string) (map[int64]float64, int, error) {
	tokens := strings.Fields(q)
	if len(tokens) == 0 {
		return map[int64]float64{}, 0, nil
	}
	phrase := quotePhraseTokens(tokens)

	rows, err := s.db.QueryContext(ctx,
		`SELECT rowid, bm25(chunks_fts_uni) FROM chunks_fts_uni
		 WHERE chunks_fts_uni MATCH ?
		 ORDER BY bm25(chunks_fts_uni) ASC LIMIT ?`,
		phrase, s.cfg.UniLimit)
	if err != nil {
		return nil, 0, fmt.Errorf("query chunks_fts_uni: %w", err)
	}
	defer rows.Close()

	raw := map[int64]float64{}
	for rows.Next() {
		var id int64
		var bm float64
		if err := rows.Scan(&id, &bm); err != nil {
			return nil, 0, err
		}
		raw[id] = -bm // bm25() is lower=better; negate so higher=better throughout
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	return minMaxNormalize(raw), len(raw), nil
}

// chunkText is the (id, byte-exact text, once-folded text) triple loaded
// for the trigram leg's Go substring-scan fallback. textFolded is computed
// once here, at load time, rather than inside scanSubstring on every call
// (MINOR 7): a query whose tokens all fall through to the scan floor would
// otherwise re-fold the whole corpus once per token instead of once per
// Search call.
type chunkText struct {
	id         int64
	text       string
	textFolded string
}

// triLeg runs the trigram leg (W12-03 step 3b) over fold(q): for each
// whitespace token of len >= 3 runes, the full token is always tried as a
// MATCH first (so exact long-token matches - a full URL, a hash - still
// hit even when truncation is bounded below); if that finds nothing, a
// relaxation ladder truncates one trailing rune at a time down to 3 runes
// (the trigram tokenizer's floor), starting no higher than
// cfg.LadderStartMax runes below the full length (BLOCKER 4: bounds a
// pathological single-token input to ~cfg.LadderStartMax queries instead
// of thousands; real Turkish words are well under this floor so natural-
// language behavior is unchanged). If the MATCH ladder never finds a row
// (or the token started under 3 runes, where trigram MATCH is impossible),
// relaxation continues as a single Go substring scan against the token's
// first 2 runes (BLOCKER 2: a 1-rune token never scans), stopping at the
// first length with >= 1 hit. This is the mechanism that makes
// 'evlerimizden' find a chunk containing only 'ev' (HANDOFF §6 W1-2 gate)
// without any Turkish suffix table or morphological analyzer
// (tasks/README.md: no manual stemming - only character truncation).
//
// Returns the per-chunk score ready for fusion (real MATCH rows min-max
// normalized and then rescaled into [2*cfg.ScanFloorScore, 1.0] - see
// rescaleMatchScores - so a genuine MATCH hit always outranks a scan-only
// hit for the same token; scan-only rows fixed at cfg.ScanFloorScore) plus
// raw hit counts for logging.
func (s *Searcher) triLeg(ctx context.Context, q string) (scores map[int64]float64, matchHitCount, scanHitCount int, err error) {
	fq := textnorm.Fold(q)
	tokens := strings.Fields(fq)
	if len(tokens) == 0 {
		return map[int64]float64{}, 0, 0, nil
	}

	matchRaw := map[int64]float64{} // chunk_id -> best negated bm25 across tokens (real MATCH hits)
	scanOnly := map[int64]bool{}    // chunk_id -> found ONLY via the Go substring scan

	var allChunks []chunkText // lazily loaded at most once, only if some token needs the scan floor
	loadAllChunks := func() ([]chunkText, error) {
		if allChunks != nil {
			return allChunks, nil
		}
		cs, err := s.loadAllChunkText(ctx)
		if err != nil {
			return nil, err
		}
		allChunks = cs
		return allChunks, nil
	}

	for _, t := range tokens {
		runes := []rune(t)
		found := false

		if len(runes) >= 3 {
			// Always try the full token once first (BLOCKER 4): even when
			// the ladder below is bounded, an exact full-length match must
			// still hit.
			rows, err := s.triMatch(ctx, string(runes))
			if err != nil {
				return nil, 0, 0, fmt.Errorf("query chunks_fts_tri: %w", err)
			}
			if len(rows) > 0 {
				mergeMatchRows(matchRaw, rows)
				found = true
			} else {
				startFrom := len(runes) - 1
				if startFrom > s.cfg.LadderStartMax {
					startFrom = s.cfg.LadderStartMax
				}
				for length := startFrom; length >= 3; length-- {
					stem := string(runes[:length])
					rows, err := s.triMatch(ctx, stem)
					if err != nil {
						return nil, 0, 0, fmt.Errorf("query chunks_fts_tri: %w", err)
					}
					if len(rows) > 0 {
						mergeMatchRows(matchRaw, rows)
						found = true
						break
					}
				}
			}
		}
		if found {
			continue
		}

		// Below the trigram floor: fall back to a Go substring scan
		// against the token's first 2 runes (language-agnostic relaxation,
		// not stemming). The scan start is fixed at 2 runes whenever the
		// token itself has >= 2 runes (BLOCKER 2: the old
		// `min(len(runes), 3) - 1` start collapsed to 1 or 0 for tokens
		// shorter than 3 runes, and the loop's own `>= 2` floor then
		// silently skipped the scan for exactly the short tokens it exists
		// to serve, e.g. a bare "ev" query). A 1-rune token still never
		// scans.
		if len(runes) >= 2 {
			stem := string(runes[:2])
			cs, err := loadAllChunks()
			if err != nil {
				return nil, 0, 0, fmt.Errorf("load chunks for scan: %w", err)
			}
			hitIDs := scanSubstring(cs, stem)
			for _, id := range hitIDs {
				if _, already := matchRaw[id]; !already {
					scanOnly[id] = true
				}
			}
		}
	}

	normMatch := rescaleMatchScores(matchRaw, matchFloorScore(s.cfg))
	final := make(map[int64]float64, len(normMatch)+len(scanOnly))
	for id, v := range normMatch {
		final[id] = v
	}
	for id := range scanOnly {
		if _, already := final[id]; !already {
			final[id] = s.cfg.ScanFloorScore
		}
	}

	return final, len(matchRaw), len(scanOnly), nil
}

// triMatch runs ONE trigram MATCH attempt for stem (a folded token or a
// truncation of one), returning raw bm25 (lower=better) per matching
// chunk_id. A stem under 3 runes, or one the trigram tokenizer simply
// cannot find, legitimately returns zero rows without any SQL error - the
// caller (triLeg) is what decides to fall through to the substring scan.
func (s *Searcher) triMatch(ctx context.Context, stem string) (map[int64]float64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT rowid, bm25(chunks_fts_tri) FROM chunks_fts_tri WHERE chunks_fts_tri MATCH ?`,
		quotePhrase(stem))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[int64]float64{}
	for rows.Next() {
		var id int64
		var bm float64
		if err := rows.Scan(&id, &bm); err != nil {
			return nil, err
		}
		out[id] = bm
	}
	return out, rows.Err()
}

// mergeMatchRows folds one triMatch call's raw bm25 rows into dst
// (chunk_id -> best negated bm25 seen across every token/length tried so
// far), negating so higher=better throughout, keeping the best (max)
// negated score per chunk_id when a chunk is hit by more than one token.
func mergeMatchRows(dst, rows map[int64]float64) {
	for id, bm := range rows {
		neg := -bm
		if cur, ok := dst[id]; !ok || neg > cur {
			dst[id] = neg
		}
	}
}

// loadAllChunkText loads every chunk's id+text (plus its folded text, see
// chunkText) for the trigram leg's Go substring-scan fallback (W12-03 step
// 3b: "Corpus <= ~100k chunks per §4 - brute force is in-budget"). Called
// at most once per Search call, lazily, only once some token's MATCH
// ladder has bottomed out with zero rows.
func (s *Searcher) loadAllChunkText(ctx context.Context) ([]chunkText, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, text FROM chunks`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []chunkText
	for rows.Next() {
		var c chunkText
		if err := rows.Scan(&c.id, &c.text); err != nil {
			return nil, err
		}
		c.textFolded = textnorm.Fold(c.text)
		out = append(out, c)
	}
	return out, rows.Err()
}

// scanSubstring returns the ids of every chunk whose FOLDED text contains
// stem, using each chunk's textFolded computed once by loadAllChunkText
// (MINOR 7) rather than re-folding c.text here on every call - a query
// whose N tokens all fall through to the scan floor previously re-folded
// the whole corpus N times over.
func scanSubstring(chunks []chunkText, stem string) []int64 {
	var ids []int64
	for _, c := range chunks {
		if strings.Contains(c.textFolded, stem) {
			ids = append(ids, c.id)
		}
	}
	return ids
}

// fuse combines the two legs' normalized per-chunk scores (W12-03 step 3c):
// fused = TriWeight*tri + UniWeight*uni. A chunk missing from a leg simply
// is not a key in that leg's map, so Go's zero-value map lookup already
// gives "missing leg contributes 0" for free.
func fuse(cfg Config, tri, uni map[int64]float64) map[int64]float64 {
	out := make(map[int64]float64, len(tri)+len(uni))
	for id := range tri {
		out[id] = 0
	}
	for id := range uni {
		out[id] = 0
	}
	for id := range out {
		out[id] = cfg.TriWeight*tri[id] + cfg.UniWeight*uni[id]
	}
	return out
}

// minMaxNormalize scales raw (higher=better) into [0,1]. A single-element
// (or all-equal) input maps to 1.0: with nothing to compare against, the
// only candidate IS the best one in its leg, and there is no meaningful
// "worst" score to anchor at 0.
func minMaxNormalize(raw map[int64]float64) map[int64]float64 {
	out := make(map[int64]float64, len(raw))
	if len(raw) == 0 {
		return out
	}

	lo, hi := math.Inf(1), math.Inf(-1)
	for _, v := range raw {
		if v < lo {
			lo = v
		}
		if v > hi {
			hi = v
		}
	}

	if hi == lo {
		for id := range raw {
			out[id] = 1.0
		}
		return out
	}
	for id, v := range raw {
		out[id] = (v - lo) / (hi - lo)
	}
	return out
}

// matchFloorScore returns the minimum post-normalization score a genuine
// trigram MATCH hit may receive: 2*cfg.ScanFloorScore (0.2 with defaults).
// This is strictly above cfg.ScanFloorScore itself, so the worst MATCH hit
// still always outranks a scan-only hit for the same token (BLOCKER 1).
func matchFloorScore(cfg Config) float64 {
	return 2 * cfg.ScanFloorScore
}

// rescaleMatchScores min-max normalizes raw trigram MATCH scores (see
// minMaxNormalize) and then rescales the [0,1] result into [floor, 1.0]
// via `floor + (1-floor)*minmax` (BLOCKER 1). Plain min-max normalization
// maps the single worst MATCH hit to 0.0 - at or below cfg.ScanFloorScore -
// which would invert the required invariant that a genuine MATCH hit
// always outranks a scan-floor hit. A single-hit (or all-equal) input
// still maps to 1.0: minMaxNormalize's hi==lo branch already yields 1.0,
// and floor + (1-floor)*1.0 == 1.0 regardless of floor.
func rescaleMatchScores(raw map[int64]float64, floor float64) map[int64]float64 {
	minmax := minMaxNormalize(raw)
	out := make(map[int64]float64, len(minmax))
	for id, v := range minmax {
		out[id] = floor + (1-floor)*v
	}
	return out
}

// loadHits joins the top-k chunk ids (already ranked by the caller) with
// chunks/episodes for text/path/source_tier, preserving the caller's order
// and attaching each chunk's already-computed fused score.
func (s *Searcher) loadHits(ctx context.Context, ids []int64, fused map[int64]float64) ([]Hit, error) {
	if len(ids) == 0 {
		return []Hit{}, nil
	}

	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	query := fmt.Sprintf(`
		SELECT c.id, c.episode_id, c.seq, c.text, e.source_path, e.source_tier
		FROM chunks c
		JOIN episodes e ON e.id = c.episode_id
		WHERE c.id IN (%s)`, strings.Join(placeholders, ","))

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byID := make(map[int64]Hit, len(ids))
	for rows.Next() {
		var h Hit
		var path sql.NullString
		if err := rows.Scan(&h.ChunkID, &h.EpisodeID, &h.Seq, &h.Text, &path, &h.SourceTier); err != nil {
			return nil, err
		}
		h.Path = path.String
		h.Score = fused[h.ChunkID]
		byID[h.ChunkID] = h
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]Hit, 0, len(ids))
	for _, id := range ids {
		if h, ok := byID[id]; ok {
			out = append(out, h)
		}
	}
	return out, nil
}

// quotePhraseTokens turns whitespace tokens into individually
// phrase-quoted FTS5 MATCH terms, ANDed by FTS5's default space-separated
// boolean (W12-03 step 3a: "query tokens quoted as phrases").
func quotePhraseTokens(tokens []string) string {
	parts := make([]string, len(tokens))
	for i, t := range tokens {
		parts[i] = quotePhrase(t)
	}
	return strings.Join(parts, " ")
}

// quotePhrase wraps t as a single FTS5 phrase, escaping any embedded double
// quote by doubling it (the standard FTS5 string-literal escape) so a
// token containing a literal '"' cannot break the MATCH query's syntax.
func quotePhrase(t string) string {
	return `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
}
