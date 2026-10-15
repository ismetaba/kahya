package extract

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/factengine"
	"kahya/kahyad/internal/memlane"
	"kahya/kahyad/internal/reader"
	"kahya/kahyad/internal/ritual"
	"kahya/kahyad/internal/router"
	"kahya/kahyad/internal/secretlane"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/store/sqlcgen"
	"kahya/kahyad/internal/taint"
)

// readerCall records one RunExtract call the fake reader received.
type readerCall struct {
	chunks      []reader.ExtractChunk
	forceSecret bool
}

// fakeReader is a ReaderRunner double: respond builds each answer (nil =
// an empty answer on the normal lane).
type fakeReader struct {
	mu      sync.Mutex
	calls   []readerCall
	respond func(n int, chunks []reader.ExtractChunk, forceSecret bool) (reader.FactExtractV1, string, error)
}

func (f *fakeReader) RunExtract(ctx context.Context, chunks []reader.ExtractChunk, traceID string, forceSecret bool) (reader.FactExtractV1, string, error) {
	f.mu.Lock()
	n := len(f.calls)
	f.calls = append(f.calls, readerCall{chunks: append([]reader.ExtractChunk(nil), chunks...), forceSecret: forceSecret})
	respond := f.respond
	f.mu.Unlock()
	if respond == nil {
		lane := router.LaneNormal
		if forceSecret {
			lane = router.LaneSecret
		}
		return reader.FactExtractV1{}, lane, nil
	}
	return respond(n, chunks, forceSecret)
}

func (f *fakeReader) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeReader) call(i int) readerCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[i]
}

// laneOf is the lane a well-behaved reader would answer on.
func laneOf(forceSecret bool) string {
	if forceSecret {
		return router.LaneSecret
	}
	return router.LaneNormal
}

// emreAcme answers every call with Emre (person) works_at Acme (org),
// citing the call's first chunk.
func emreAcme(n int, chunks []reader.ExtractChunk, forceSecret bool) (reader.FactExtractV1, string, error) {
	return reader.FactExtractV1{
		Entities: []reader.ExtractEntityV1{
			{Name: "Emre", Kind: factengine.EntityKindPerson, Aliases: []string{"Emre Bey"}},
			{Name: "Acme", Kind: factengine.EntityKindOrg},
		},
		Facts: []reader.ExtractFactV1{{
			ChunkID: chunks[0].ID, Subject: "Emre", Predicate: "works_at", Object: "Acme",
			Evidentiality: factengine.Reported, Quote: "Emre Acme'de", Importance: 0.7,
		}},
	}, laneOf(forceSecret), nil
}

type fixture struct {
	t        *testing.T
	st       *store.Store
	engine   *factengine.Engine
	memDir   string
	reader   *fakeReader
	runner   *Runner
	guardMu  sync.Mutex
	guarded  []string
	secretOn string // guard classifier: text containing this is secret
}

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(config.Config{DBPath: filepath.Join(t.TempDir(), "brain.db")})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// newFixture wires a Runner over a real temp brain.db, a real factengine,
// the production path-glob matcher (secret glob <memDir>/finans*.md), a
// guard classifier whose verdict is secret only for text containing
// f.secretOn, and a fake reader.
func newFixture(t *testing.T, respond func(int, []reader.ExtractChunk, bool) (reader.FactExtractV1, string, error)) *fixture {
	t.Helper()
	st := newTestStore(t)
	f := &fixture{t: t, st: st, memDir: t.TempDir(), reader: &fakeReader{respond: respond}}
	f.engine = factengine.New(st.Queries, taint.New(st.Queries, st), st)
	classifier := secretlane.NewClassifier(secretlane.QwenClassifierFunc(func(ctx context.Context, text string) (secretlane.Verdict, error) {
		f.guardMu.Lock()
		f.guarded = append(f.guarded, text)
		secret := f.secretOn != "" && strings.Contains(text, f.secretOn)
		f.guardMu.Unlock()
		if secret {
			return secretlane.Verdict{SecretLane: true, Category: secretlane.CategorySaglik, Reason: "qwen"}, nil
		}
		return secretlane.Verdict{SecretLane: false, Category: secretlane.CategoryNone, Reason: "qwen"}, nil
	}))
	f.runner = &Runner{
		Store:  st.Queries,
		Reader: f.reader,
		Engine: f.engine,
		Lane: memlane.Classifier{
			Store: st.Queries, MemoryDir: f.memDir,
			SecretGlobs: []string{filepath.Join(f.memDir, "finans*.md")},
			Matcher:     ritual.PolicyGlobMatcher{},
		},
		Classifier: classifier,
		Ledger:     st,
		Cfg:        Config{Enabled: true, Location: time.UTC},
	}
	return f
}

func (f *fixture) guardCount() int {
	f.guardMu.Lock()
	defer f.guardMu.Unlock()
	return len(f.guarded)
}

func ns(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

// seedEpisode inserts an active memory_file episode at relPath ("" = no
// source_path) with one chunk per text, returning the episode and chunk ids.
func (f *fixture) seedEpisode(relPath string, texts ...string) (int64, []int64) {
	f.t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	ep, err := f.st.Queries.InsertEpisode(ctx, sqlcgen.InsertEpisodeParams{
		Source: SourceMemoryFile, SourcePath: ns(relPath), SourceHash: ns("h:" + relPath + strconv.Itoa(len(texts))),
		SourceTier: factengine.TierAgentDerived, Status: "active", CreatedAt: now,
	})
	if err != nil {
		f.t.Fatalf("InsertEpisode: %v", err)
	}
	ids := make([]int64, 0, len(texts))
	for i, text := range texts {
		ch, err := f.st.Queries.InsertChunk(ctx, sqlcgen.InsertChunkParams{
			EpisodeID: ep.ID, Seq: int64(i), Text: text, ContentHash: fmt.Sprintf("c%d-%d", ep.ID, i), CreatedAt: now,
		})
		if err != nil {
			f.t.Fatalf("InsertChunk: %v", err)
		}
		ids = append(ids, ch.ID)
	}
	return ep.ID, ids
}

func (f *fixture) episode(id int64) sqlcgen.Episode {
	f.t.Helper()
	ep, err := f.st.Queries.GetEpisodeByID(context.Background(), id)
	if err != nil {
		f.t.Fatalf("GetEpisodeByID(%d): %v", id, err)
	}
	return ep
}

func (f *fixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.st.DB().QueryRow(query, args...).Scan(&n); err != nil {
		f.t.Fatalf("%s: %v", query, err)
	}
	return n
}

func (f *fixture) events(kind string) []map[string]string {
	f.t.Helper()
	rows, err := f.st.DB().Query(`SELECT payload FROM events WHERE kind = ? ORDER BY id`, kind)
	if err != nil {
		f.t.Fatalf("events %s: %v", kind, err)
	}
	defer rows.Close()
	var out []map[string]string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, flatPayload(p))
	}
	return out
}

// flatPayload decodes the top-level fields of a JSON payload as strings
// (numbers and bools as fmt prints them).
func flatPayload(p string) map[string]string {
	var raw map[string]any
	if err := json.Unmarshal([]byte(p), &raw); err != nil {
		return map[string]string{"_decode_error": err.Error()}
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		out[k] = fmt.Sprint(v)
	}
	return out
}

func (f *fixture) onlyFact() sqlcgen.Fact {
	f.t.Helper()
	var id int64
	if n := f.count(`SELECT count(*) FROM facts`); n != 1 {
		f.t.Fatalf("facts rows = %d, want 1", n)
	}
	if err := f.st.DB().QueryRow(`SELECT id FROM facts`).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	fact, err := f.st.Queries.GetFact(context.Background(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return fact
}
