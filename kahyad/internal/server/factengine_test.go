// factengine_test.go covers the entity read routes (G13 MEM-2 phase 2):
// GET /v1/entity/list and GET /v1/entity/show/{id}, over a real temp
// brain.db and the real fact engine.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/factengine"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/taint"
)

func newEntityRouteFixture(t *testing.T, wire bool) (*http.Client, *factengine.Engine, *store.Store) {
	t.Helper()
	st, err := store.Open(config.Config{DBPath: filepath.Join(t.TempDir(), "brain.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fe := factengine.New(st.Queries, taint.New(st.Queries, st), st)
	cfg := config.Config{Socket: filepath.Join(shortSocketDir(t), "k.sock")}
	srv := New(cfg, testLogger(t), "v-entity-test", healthyDB)
	srv.SetFactEngine(fe)
	if wire {
		srv.SetEntityReader(fe)
	}
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	go srv.Serve() //nolint:errcheck
	t.Cleanup(func() { srv.Shutdown() })
	return unixHTTPClient(cfg.Socket), fe, st
}

func getEntityJSON(t *testing.T, client *http.Client, path string, out any) int {
	t.Helper()
	resp, err := client.Get("http://kahyad" + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
	}
	return resp.StatusCode
}

// TestEntityListPrefixAndShow: two entities, prefix "em" lists one; show
// returns its aliases and facts (the unconfirmed agent_derived one labelled
// quarantined).
func TestEntityListPrefixAndShow(t *testing.T) {
	client, fe, st := newEntityRouteFixture(t, true)
	ctx := context.Background()
	emre, _, _, err := fe.ResolveEntityForExtraction(ctx, "tr", "Emre", factengine.EntityKindPerson, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fe.AddAlias(ctx, "tr", emre, "Emre Bey", false); err != nil {
		t.Fatal(err)
	}
	acme, _, _, err := fe.ResolveEntityForExtraction(ctx, "tr", "Acme", factengine.EntityKindOrg, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := taint.New(st.Queries, st).InsertClean(ctx, "tr", "clean"); err != nil {
		t.Fatal(err)
	}
	if _, err := fe.WriteFact(ctx, "tr", factengine.Candidate{
		Subject: "Emre", Predicate: "works_at", Object: "Acme", Provenance: factengine.ProvenanceUserAsserted,
		SessionID: "clean", ExtractorVer: "test_v1", SubjectEntityID: emre, ObjectEntityID: acme,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fe.WriteFact(ctx, "tr", factengine.Candidate{
		Subject: "Emre", Predicate: "lives_in", Object: "Kadıköy", ExtractorVer: "test_v1", SubjectEntityID: emre,
	}); err != nil {
		t.Fatal(err)
	}

	var list entityListResponse
	if status := getEntityJSON(t, client, "/v1/entity/list?q=em", &list); status != http.StatusOK {
		t.Fatalf("list status = %d", status)
	}
	if len(list.Entities) != 1 || list.Entities[0].ID != emre || list.Entities[0].ActiveFacts != 2 || len(list.Entities[0].Aliases) != 2 {
		t.Fatalf("list em = %+v, want Emre with 2 aliases and 2 facts", list.Entities)
	}
	if status := getEntityJSON(t, client, "/v1/entity/list", &list); status != http.StatusOK || len(list.Entities) != 2 {
		t.Fatalf("list all = %d %+v", status, list.Entities)
	}
	if status := getEntityJSON(t, client, "/v1/entity/list?provisional=1", &list); status != http.StatusOK || len(list.Entities) != 0 || list.Entities == nil {
		t.Fatalf("provisional list = %d %+v, want an empty (non-null) list", status, list.Entities)
	}

	var d factengine.EntityDetail
	if status := getEntityJSON(t, client, "/v1/entity/show/"+itoa(emre), &d); status != http.StatusOK {
		t.Fatalf("show status = %d", status)
	}
	if d.CanonicalName != "Emre" || len(d.Facts) != 2 || d.Facts[0].Predicate != "works_at" || d.Facts[0].Quarantined || !d.Facts[1].Quarantined {
		t.Fatalf("show = %+v", d)
	}
}

func TestEntityRoutesUnwiredAre503(t *testing.T) {
	client, _, _ := newEntityRouteFixture(t, false)
	for _, p := range []string{"/v1/entity/list", "/v1/entity/show/1"} {
		if status := getEntityJSON(t, client, p, nil); status != http.StatusServiceUnavailable {
			t.Errorf("GET %s unwired = %d, want 503", p, status)
		}
	}
}

func TestEntityShowBadIDAndNotFound(t *testing.T) {
	client, _, _ := newEntityRouteFixture(t, true)
	for path, want := range map[string]int{
		"/v1/entity/show/abc":     http.StatusBadRequest,
		"/v1/entity/show/0":       http.StatusBadRequest,
		"/v1/entity/show/424242":  http.StatusNotFound,
		"/v1/entity/list?limit=0": http.StatusBadRequest,
		"/v1/entity/list?limit=x": http.StatusBadRequest,
	} {
		if status := getEntityJSON(t, client, path, nil); status != want {
			t.Errorf("GET %s = %d, want %d", path, status, want)
		}
	}
	// The merge/split routes keep working next to the new patterns.
	resp, err := client.Post("http://kahyad/v1/entity/merge", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST /v1/entity/merge without a body = %d, want 400 (route still mounted)", resp.StatusCode)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
