// memory_extract_test.go covers POST /v1/memory/extract (G13 MEM-3).
package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/extract"
	"kahya/kahyad/internal/factengine"
	"kahya/kahyad/internal/memlane"
	"kahya/kahyad/internal/reader"
	"kahya/kahyad/internal/ritual"
	"kahya/kahyad/internal/router"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/store/sqlcgen"
	"kahya/kahyad/internal/taint"
)

// fakeMemoryExtractor records which entry point the route called.
type fakeMemoryExtractor struct {
	mu    sync.Mutex
	calls []string
	stats extract.Stats
	err   error
}

func (f *fakeMemoryExtractor) record(c string) (extract.Stats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	return f.stats, f.err
}

func (f *fakeMemoryExtractor) RunPending(ctx context.Context, traceID string) (extract.Stats, error) {
	return f.record("pending")
}

func (f *fakeMemoryExtractor) ExtractAll(ctx context.Context, traceID string) (extract.Stats, error) {
	return f.record("all")
}

func (f *fakeMemoryExtractor) ExtractEpisode(ctx context.Context, traceID string, id int64, prov factengine.Provenance) (extract.Stats, error) {
	return f.record(fmt.Sprintf("episode:%d:%s", id, prov))
}

func (f *fakeMemoryExtractor) ExtractFile(ctx context.Context, traceID, relPath string) (extract.Stats, error) {
	return f.record("file:" + relPath)
}

func newMemoryExtractTestServer(t *testing.T, memDir string, e MemoryExtractor) *http.Client {
	t.Helper()
	cfg := config.Config{Socket: filepath.Join(shortSocketDir(t), "k.sock"), MemoryDir: memDir}
	srv := New(cfg, testLogger(t), "v-memory-extract-test", healthyDB)
	if e != nil {
		srv.SetMemoryExtractor(e)
	}
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	go srv.Serve() //nolint:errcheck
	t.Cleanup(func() { srv.Shutdown() })
	return unixHTTPClient(cfg.Socket)
}

func postMemoryExtract(t *testing.T, client *http.Client, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://kahyad/v1/memory/extract", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Kahya-Trace-Id", "trace-memory-extract")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/memory/extract: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestMemoryExtractRouteUnwiredIs503(t *testing.T) {
	client := newMemoryExtractTestServer(t, t.TempDir(), nil)
	if status, _ := postMemoryExtract(t, client, `{"episode_id":1}`); status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", status)
	}
}

func TestMemoryExtractRouteSelectsTheEntryPoint(t *testing.T) {
	memDir := "/Users/u/Kahya/memory"
	for _, tc := range []struct {
		body, want string
	}{
		{``, "pending"},
		{`{}`, "pending"},
		{`{"episode_id":7}`, "episode:7:agent_derived"},
		{`{"file":"inbox/2026-10-08.md"}`, "file:inbox/2026-10-08.md"},
		{`{"file":"/Users/u/Kahya/memory/people/emre.md"}`, "file:people/emre.md"},
		{`{"all":true}`, "all"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			fake := &fakeMemoryExtractor{stats: extract.Stats{Episodes: 2, Facts: 3, Entities: 1, Skipped: 1}}
			client := newMemoryExtractTestServer(t, memDir, fake)
			status, out := postMemoryExtract(t, client, tc.body)
			if status != http.StatusOK || len(fake.calls) != 1 || fake.calls[0] != tc.want {
				t.Fatalf("status %d, calls %v; want 200 and %q", status, fake.calls, tc.want)
			}
			if out["episodes"] != 2.0 || out["facts"] != 3.0 || out["entities"] != 1.0 || out["skipped"] != 1.0 || out["trace_id"] != "trace-memory-extract" {
				t.Fatalf("response = %v", out)
			}
		})
	}
}

func TestMemoryExtractRouteRejectsBadSelectors(t *testing.T) {
	fake := &fakeMemoryExtractor{}
	client := newMemoryExtractTestServer(t, "/Users/u/Kahya/memory", fake)
	for _, body := range []string{
		`{"episode_id":1,"all":true}`,
		`{"file":"a.md","episode_id":2}`,
		`{"episode_id":-3}`,
		`{"file":"../outside.md"}`,
		`{"file":"/etc/passwd"}`,
		`{`,
	} {
		if status, _ := postMemoryExtract(t, client, body); status != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", body, status)
		}
	}
	if len(fake.calls) != 0 {
		t.Fatalf("extractor called %v for a rejected request", fake.calls)
	}
}

func TestMemoryExtractRouteMapsErrors(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{extract.ErrInProgress, http.StatusConflict},
		{fmt.Errorf("%w: id 9", extract.ErrEpisodeNotFound), http.StatusNotFound},
		{fmt.Errorf("extract: list pending episodes: disk I/O error"), http.StatusInternalServerError},
	} {
		client := newMemoryExtractTestServer(t, t.TempDir(), &fakeMemoryExtractor{err: tc.err})
		status, out := postMemoryExtract(t, client, `{}`)
		if status != tc.want || out["error"] != tc.err.Error() {
			t.Errorf("err %v: status %d body %v, want %d with the error text", tc.err, status, out, tc.want)
		}
	}
}

// routeTestReader answers every call with Emre works_at Acme (citing the
// call's first chunk) on the lane the caller allowed.
type routeTestReader struct{}

func (routeTestReader) RunExtract(ctx context.Context, chunks []reader.ExtractChunk, traceID string, forceSecret bool) (reader.FactExtractV1, string, error) {
	lane := router.LaneNormal
	if forceSecret {
		lane = router.LaneSecret
	}
	return reader.FactExtractV1{
		Entities: []reader.ExtractEntityV1{{Name: "Emre", Kind: factengine.EntityKindPerson}, {Name: "Acme", Kind: factengine.EntityKindOrg}},
		Facts: []reader.ExtractFactV1{{
			ChunkID: chunks[0].ID, Subject: "Emre", Predicate: "works_at", Object: "Acme", Evidentiality: factengine.Witnessed,
		}},
	}, lane, nil
}

// TestMemoryExtractRouteByEpisode drives the route with a real
// extract.Runner over a real temp brain.db: one episode's fact and two
// entities are written and the episode is stamped.
func TestMemoryExtractRouteByEpisode(t *testing.T) {
	st, err := store.Open(config.Config{DBPath: filepath.Join(t.TempDir(), "brain.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	ep, err := st.Queries.InsertEpisode(ctx, sqlcgen.InsertEpisodeParams{
		Source: extract.SourceMemoryFile, SourcePath: sql.NullString{String: "inbox/a.md", Valid: true}, SourceHash: sql.NullString{String: "h", Valid: true},
		SourceTier: factengine.TierAgentDerived, Status: "active", CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Queries.InsertChunk(ctx, sqlcgen.InsertChunkParams{EpisodeID: ep.ID, Text: "Emre Acme'de.", ContentHash: "c", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	memDir := t.TempDir()
	runner := &extract.Runner{
		Store: st.Queries, Reader: routeTestReader{},
		Engine: factengine.New(st.Queries, taint.New(st.Queries, st), st),
		Lane:   memlane.Classifier{Store: st.Queries, MemoryDir: memDir, Matcher: ritual.PolicyGlobMatcher{}},
		Ledger: st,
	}
	client := newMemoryExtractTestServer(t, memDir, runner)
	status, out := postMemoryExtract(t, client, fmt.Sprintf(`{"episode_id":%d}`, ep.ID))
	if status != http.StatusOK || out["episodes"] != 1.0 || out["facts"] != 1.0 || out["entities"] != 2.0 {
		t.Fatalf("status %d, body %v; want 1 episode, 1 fact, 2 entities", status, out)
	}
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM facts WHERE extractor_ver = ? AND source_tier = 'agent_derived'`, extract.ExtractorVersion).Scan(&n); err != nil || n != 1 {
		t.Fatalf("extracted agent_derived facts = %d (%v), want 1", n, err)
	}
	if got, err := st.Queries.GetEpisodeByID(ctx, ep.ID); err != nil || !got.ExtractedAt.Valid {
		t.Fatalf("episode not stamped: %+v, %v", got.ExtractedAt, err)
	}
	if status, _ := postMemoryExtract(t, client, `{"episode_id":424242}`); status != http.StatusNotFound {
		t.Fatalf("unknown episode status = %d, want 404", status)
	}
}
