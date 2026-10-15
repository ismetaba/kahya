// factengine_test.go covers the entity read routes (G13 MEM-2 phase 2):
// GET /v1/entity/list and GET /v1/entity/show/{id}, over a real temp
// brain.db and the real fact engine.
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/factengine"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/store/sqlcgen"
	"kahya/kahyad/internal/taint"
	"kahya/kahyad/internal/task"
)

func newEntityRouteFixture(t *testing.T, wire bool) (*http.Client, *factengine.Engine, *store.Store) {
	t.Helper()
	_, client, fe, st := newFactRouteServer(t, wire)
	return client, fe, st
}

// newFactRouteServer is newEntityRouteFixture that also returns the running
// Server (for live-worker attribution tests).
func newFactRouteServer(t *testing.T, wire bool) (*Server, *http.Client, *factengine.Engine, *store.Store) {
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
	return srv, unixHTTPClient(cfg.Socket), fe, st
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

// ---- G13 MEM-5 phase 1: bulk fact confirm / deny and the fact list ----

// postFactJSON POSTs body to path and decodes the JSON answer into out (a
// map when out is nil); it returns the status.
func postFactJSON(t *testing.T, client *http.Client, path, body string, out any) int {
	t.Helper()
	resp, err := client.Post("http://kahyad"+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	if out == nil {
		var m map[string]any
		out = &m
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return resp.StatusCode
}

// bulkFactResponse mirrors the bulk confirm/deny answer.
type bulkFactResponse struct {
	OK        bool              `json:"ok"`
	ID        int64             `json:"id"`
	Confirmed []int64           `json:"confirmed"`
	Denied    []int64           `json:"denied"`
	Failed    map[string]string `json:"failed"`
	Error     string            `json:"error"`
}

// agentFact writes an unconfirmed agent_derived fact (quarantined), citing
// episodeID when non-zero.
func agentFact(t *testing.T, fe *factengine.Engine, subject, object string, episodeID int64) int64 {
	t.Helper()
	id, err := fe.WriteFact(context.Background(), "tr-seed", factengine.Candidate{
		Subject: subject, Predicate: "works_at", Object: object, ExtractorVer: "test_v1",
		EpisodeID: episodeID, SessionID: "seed-" + subject + "-" + object,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func confirmedAt(t *testing.T, fe *factengine.Engine, id int64) bool {
	t.Helper()
	f, err := fe.GetFact(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return f.ConfirmedAt.Valid
}

func seedEpisodeRow(t *testing.T, st *store.Store, relPath string) int64 {
	t.Helper()
	ep, err := st.Queries.InsertEpisode(context.Background(), sqlcgen.InsertEpisodeParams{
		Source: "memory_file", SourcePath: sql.NullString{String: relPath, Valid: true},
		SourceTier: "agent_derived", Status: "active", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatal(err)
	}
	return ep.ID
}

// TestFactConfirmRouteSingleIDStillWorks: the W5-04 body {"fact_id":N}
// keeps its {"ok":true,"id":N} answer (the CLI's factAction decodes it);
// an id that names no fact is now a 422, not a silent success.
func TestFactConfirmRouteSingleIDStillWorks(t *testing.T) {
	client, fe, _ := newEntityRouteFixture(t, true)
	id := agentFact(t, fe, "Emre", "Acme", 0)

	var raw map[string]any
	if status := postFactJSON(t, client, "/v1/fact/confirm", fmt.Sprintf(`{"fact_id":%d}`, id), &raw); status != http.StatusOK {
		t.Fatalf("status = %d (%v)", status, raw)
	}
	if len(raw) != 2 || raw["ok"] != true || raw["id"] != float64(id) {
		t.Fatalf("single-id answer = %v, want exactly {ok:true, id:%d}", raw, id)
	}
	if !confirmedAt(t, fe, id) {
		t.Fatal("fact not confirmed")
	}
	var bad bulkFactResponse
	if status := postFactJSON(t, client, "/v1/fact/confirm", `{"fact_id":424242}`, &bad); status != http.StatusUnprocessableEntity || bad.OK || bad.Error == "" {
		t.Fatalf("unknown id = %d %+v, want 422 ok:false with an error", status, bad)
	}
}

// TestFactConfirmRouteAcceptsManyIDs: {"fact_ids":[...]} confirms each id
// (one fact_confirmed ledger row each) and reports the ones that failed.
func TestFactConfirmRouteAcceptsManyIDs(t *testing.T) {
	client, fe, st := newEntityRouteFixture(t, true)
	a := agentFact(t, fe, "Emre", "Acme", 0)
	b := agentFact(t, fe, "Ayla", "Beta", 0)
	c := agentFact(t, fe, "Can", "Gama", 0)

	var res bulkFactResponse
	status := postFactJSON(t, client, "/v1/fact/confirm", fmt.Sprintf(`{"fact_ids":[%d,%d,%d,424242]}`, a, b, a), &res)
	if status != http.StatusOK || !res.OK {
		t.Fatalf("status = %d %+v", status, res)
	}
	if !reflect.DeepEqual(res.Confirmed, []int64{a, b}) {
		t.Fatalf("confirmed = %v, want [%d %d] (a repeated id once)", res.Confirmed, a, b)
	}
	if len(res.Failed) != 1 || res.Failed["424242"] == "" {
		t.Fatalf("failed = %v, want only 424242", res.Failed)
	}
	if !confirmedAt(t, fe, a) || !confirmedAt(t, fe, b) || confirmedAt(t, fe, c) {
		t.Fatal("confirmed set wrong")
	}
	if n := countEventsOfKind(t, st, factengine.EventFactConfirmed); n != 2 {
		t.Fatalf("fact_confirmed rows = %d, want 2", n)
	}
}

// TestFactConfirmRouteByEpisode: {"episode_id":E} confirms every active
// fact that episode evidences, nothing else.
func TestFactConfirmRouteByEpisode(t *testing.T) {
	client, fe, st := newEntityRouteFixture(t, true)
	ep := seedEpisodeRow(t, st, "inbox/2026-10-08.md")
	other := seedEpisodeRow(t, st, "inbox/2026-10-09.md")
	a := agentFact(t, fe, "Emre", "Acme", ep)
	b := agentFact(t, fe, "Ayla", "Beta", ep)
	c := agentFact(t, fe, "Can", "Gama", other)

	var res bulkFactResponse
	if status := postFactJSON(t, client, "/v1/fact/confirm", fmt.Sprintf(`{"episode_id":%d}`, ep), &res); status != http.StatusOK || !res.OK {
		t.Fatalf("status = %d %+v", status, res)
	}
	got := append([]int64(nil), res.Confirmed...)
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	if !reflect.DeepEqual(got, []int64{a, b}) || confirmedAt(t, fe, c) {
		t.Fatalf("confirmed = %v, want [%d %d] and not %d", res.Confirmed, a, b, c)
	}
}

// TestFactConfirmRouteAllQuarantinedRequiresSince: confirming "everything
// quarantined" needs an RFC3339 lower bound on created_at, and then
// confirms only the quarantined facts created at or after it.
func TestFactConfirmRouteAllQuarantinedRequiresSince(t *testing.T) {
	client, fe, st := newEntityRouteFixture(t, true)
	for _, body := range []string{`{"all_quarantined":true}`, `{"all_quarantined_since":"dün"}`, `{"all_quarantined_since":"2026-10-08"}`} {
		var res bulkFactResponse
		if status := postFactJSON(t, client, "/v1/fact/confirm", body, &res); status != http.StatusBadRequest {
			t.Fatalf("%s = %d %+v, want 400", body, status, res)
		}
	}
	base := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	fe.SetClock(func() time.Time { return base })
	old := agentFact(t, fe, "Emre", "Acme", 0)
	fe.SetClock(func() time.Time { return base.Add(2 * time.Hour) })
	fresh := agentFact(t, fe, "Ayla", "Beta", 0)
	if err := taint.New(st.Queries, st).InsertClean(context.Background(), "tr", "clean"); err != nil {
		t.Fatal(err)
	}
	trusted, err := fe.WriteFact(context.Background(), "tr", factengine.Candidate{
		Subject: "Can", Predicate: "lives_in", Object: "Kadıköy", Provenance: factengine.ProvenanceUserAsserted,
		SessionID: "clean", ExtractorVer: "test_v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	fe.SetClock(time.Now)

	var res bulkFactResponse
	since := base.Add(time.Hour).Format(time.RFC3339)
	if status := postFactJSON(t, client, "/v1/fact/confirm", `{"all_quarantined_since":"`+since+`"}`, &res); status != http.StatusOK || !res.OK {
		t.Fatalf("status = %d %+v", status, res)
	}
	if !reflect.DeepEqual(res.Confirmed, []int64{fresh}) || confirmedAt(t, fe, old) || confirmedAt(t, fe, trusted) {
		t.Fatalf("confirmed = %v, want only %d (old %d and the user_asserted %d untouched)", res.Confirmed, fresh, old, trusted)
	}
}

// TestFactConfirmRouteCapsAt500: one call confirms at most 500 facts -
// more ids (or more quarantined facts in the window) is a 400 and confirms
// nothing.
func TestFactConfirmRouteCapsAt500(t *testing.T) {
	client, fe, st := newEntityRouteFixture(t, true)
	ids := make([]string, 501)
	for i := range ids {
		ids[i] = strconv.Itoa(i + 1)
	}
	var res bulkFactResponse
	status := postFactJSON(t, client, "/v1/fact/confirm", `{"fact_ids":[`+strings.Join(ids, ",")+`]}`, &res)
	if status != http.StatusBadRequest || res.Error != "en fazla 500 olgu" {
		t.Fatalf("501 ids = %d %+v, want 400 %q", status, res, "en fazla 500 olgu")
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	for i := 0; i < 501; i++ {
		if _, err := st.Queries.InsertFact(context.Background(), sqlcgen.InsertFactParams{
			Subject: "s" + strconv.Itoa(i), Predicate: "works_at", Object: "o", SourceTier: "agent_derived",
			Evidentiality: "inferred", Status: "active", UpdatedAt: now, CreatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	status = postFactJSON(t, client, "/v1/fact/confirm", `{"all_quarantined_since":"2000-01-01T00:00:00Z"}`, &res)
	if status != http.StatusBadRequest || res.Error != "en fazla 500 olgu" {
		t.Fatalf("501 quarantined = %d %+v, want 400", status, res)
	}
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM facts WHERE confirmed_at IS NOT NULL`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("confirmed facts = %d (%v), want 0", n, err)
	}
	_ = fe
}

// TestFactDenyRouteLowersConfidence: `kahya fact deny <id>` records the
// fixed -2.94 user denial: an injection-eligible (confirmed external_doc,
// p=0.8) fact drops below the p=0.3 threshold; the same id twice in one
// call is one evidence row.
func TestFactDenyRouteLowersConfidence(t *testing.T) {
	client, fe, st := newEntityRouteFixture(t, true)
	if err := taint.New(st.Queries, st).InsertClean(context.Background(), "tr", "clean"); err != nil {
		t.Fatal(err)
	}
	id, err := fe.WriteFact(context.Background(), "tr", factengine.Candidate{
		Subject: "Emre", Predicate: "lives_in", Object: "Kadıköy", Provenance: factengine.ProvenanceExternalDoc,
		SessionID: "ingest:doc", ExtractorVer: "test_v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fe.ConfirmFact(context.Background(), "tr", id); err != nil {
		t.Fatal(err)
	}
	before, _ := fe.GetFact(context.Background(), id)
	if !factengine.InjectionEligible(before) {
		t.Fatal("seed fact should be injection-eligible")
	}
	var raw map[string]any
	if status := postFactJSON(t, client, "/v1/fact/deny", fmt.Sprintf(`{"fact_id":%d}`, id), &raw); status != http.StatusOK || raw["ok"] != true || raw["id"] != float64(id) {
		t.Fatalf("deny = %d %v, want 200 {ok:true,id:%d}", status, raw, id)
	}
	after, _ := fe.GetFact(context.Background(), id)
	if factengine.InjectionEligible(after) || after.Confidence >= factengine.InjectionThresholdLogOdds {
		t.Fatalf("after deny confidence = %v, want below %v (not injection-eligible)", after.Confidence, factengine.InjectionThresholdLogOdds)
	}

	other, err := fe.WriteFact(context.Background(), "tr", factengine.Candidate{
		Subject: "Ayla", Predicate: "lives_in", Object: "Moda", Provenance: factengine.ProvenanceUserAsserted,
		SessionID: "clean", ExtractorVer: "test_v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	var res bulkFactResponse
	if status := postFactJSON(t, client, "/v1/fact/deny", fmt.Sprintf(`{"fact_ids":[%d,%d,424242],"session_id":"cli:deny-once"}`, other, other), &res); status != http.StatusOK || !res.OK {
		t.Fatalf("bulk deny = %d %+v", status, res)
	}
	if !reflect.DeepEqual(res.Denied, []int64{other}) || res.Failed["424242"] == "" {
		t.Fatalf("bulk deny = %+v, want denied [%d] and 424242 failed", res, other)
	}
	var neg int
	if err := st.DB().QueryRow(`SELECT count(*) FROM evidence WHERE fact_id = ? AND polarity = -1`, other).Scan(&neg); err != nil || neg != 1 {
		t.Fatalf("negative evidence rows = %d (%v), want 1", neg, err)
	}
	if n := countEventsOfKind(t, st, factengine.EventFactDenied); n != 2 {
		t.Fatalf("fact_denied rows = %d, want 2", n)
	}
}

// factListReply mirrors GET /v1/fact/list.
type factListReply struct {
	Facts []struct {
		FactID       int64   `json:"fact_id"`
		Subject      string  `json:"subject"`
		SourceTier   string  `json:"source_tier"`
		ConfidenceP  float64 `json:"confidence_p"`
		Quarantined  bool    `json:"quarantined"`
		EvidencePath string  `json:"evidence_path"`
	} `json:"facts"`
	Truncated bool `json:"truncated"`
}

// TestFactListRouteQuarantinedOnly: quarantined=1 lists only the facts a
// human still has to confirm, newest first, each with its evidence path;
// without it every active fact; since and entity narrow the list.
func TestFactListRouteQuarantinedOnly(t *testing.T) {
	client, fe, st := newEntityRouteFixture(t, true)
	ctx := context.Background()
	ep := seedEpisodeRow(t, st, "inbox/2026-10-08.md")
	base := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	fe.SetClock(func() time.Time { return base })
	emre, _, _, err := fe.ResolveEntityForExtraction(ctx, "tr", "Emre", factengine.EntityKindPerson, false)
	if err != nil {
		t.Fatal(err)
	}
	q1, err := fe.WriteFact(ctx, "tr", factengine.Candidate{
		Subject: "Emre", Predicate: "works_at", Object: "Acme", ExtractorVer: "test_v1",
		EpisodeID: ep, SessionID: "s1", SubjectEntityID: emre,
	})
	if err != nil {
		t.Fatal(err)
	}
	fe.SetClock(func() time.Time { return base.Add(2 * time.Hour) })
	q2 := agentFact(t, fe, "Ayla", "Beta", 0)
	if err := taint.New(st.Queries, st).InsertClean(ctx, "tr", "clean"); err != nil {
		t.Fatal(err)
	}
	trusted, err := fe.WriteFact(ctx, "tr", factengine.Candidate{
		Subject: "Can", Predicate: "lives_in", Object: "Kadıköy", Provenance: factengine.ProvenanceUserAsserted,
		SessionID: "clean", ExtractorVer: "test_v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	fe.SetClock(time.Now)

	ids := func(r factListReply) []int64 {
		out := []int64{}
		for _, f := range r.Facts {
			out = append(out, f.FactID)
		}
		return out
	}
	var r factListReply
	if status := getEntityJSON(t, client, "/v1/fact/list?quarantined=1", &r); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if !reflect.DeepEqual(ids(r), []int64{q2, q1}) || !r.Facts[0].Quarantined || r.Facts[1].EvidencePath != "inbox/2026-10-08.md" || r.Facts[0].EvidencePath != "" {
		t.Fatalf("quarantined list = %+v, want [%d %d] newest first, %d citing its note", r, q2, q1, q1)
	}
	if status := getEntityJSON(t, client, "/v1/fact/list", &r); status != http.StatusOK || !reflect.DeepEqual(ids(r), []int64{trusted, q2, q1}) || r.Facts[0].Quarantined {
		t.Fatalf("full list = %d %+v, want [%d %d %d]", status, r, trusted, q2, q1)
	}
	since := base.Add(time.Hour).Format(time.RFC3339)
	if status := getEntityJSON(t, client, "/v1/fact/list?quarantined=1&since="+since, &r); status != http.StatusOK || !reflect.DeepEqual(ids(r), []int64{q2}) {
		t.Fatalf("since list = %d %+v, want [%d]", status, r, q2)
	}
	for _, e := range []string{"emre", itoa(emre)} {
		if status := getEntityJSON(t, client, "/v1/fact/list?entity="+e, &r); status != http.StatusOK || !reflect.DeepEqual(ids(r), []int64{q1}) {
			t.Fatalf("entity=%s list = %d %+v, want [%d]", e, status, r, q1)
		}
	}
	if status := getEntityJSON(t, client, "/v1/fact/list?limit=1", &r); status != http.StatusOK || len(r.Facts) != 1 || !r.Truncated {
		t.Fatalf("limit=1 = %d %+v, want one row, truncated", status, r)
	}
	for _, bad := range []string{"/v1/fact/list?since=dün", "/v1/fact/list?limit=0", "/v1/fact/list?limit=501"} {
		if status := getEntityJSON(t, client, bad, nil); status != http.StatusBadRequest {
			t.Fatalf("GET %s = %d, want 400", bad, status)
		}
	}
}

// TestFactReviewRoutesRefuseLiveWorker (G00 C-16): the human review routes
// are control routes - a caller inside a live worker's session is refused
// even WITH the control secret, and nothing changes: the quarantined fact
// stays unconfirmed and gets no denial (HANDOFF §5 memory #1: only a human
// lifts a quarantine).
func TestFactReviewRoutesRefuseLiveWorker(t *testing.T) {
	srv, _, fe, st := newFactRouteServer(t, true)
	id := agentFact(t, fe, "Emre", "Acme", 0)
	reg := task.NewLiveRegistry()
	srv.SetTaskLiveRegistry(reg)
	reg.RegisterWorker("t-live", "trace-live", os.Getpid(), testProcessSID(t))
	worker := unixHTTPClientNoSecret(srv.cfg.Socket)
	secret := readSecretFile(t, srv.cfg.Socket)
	for i, p := range []controlProbe{
		{http.MethodPost, "/v1/fact/confirm", fmt.Sprintf(`{"fact_ids":[%d]}`, id)},
		{http.MethodPost, "/v1/fact/confirm", fmt.Sprintf(`{"fact_id":%d}`, id)},
		{http.MethodPost, "/v1/fact/deny", fmt.Sprintf(`{"fact_id":%d}`, id)},
		{http.MethodGet, "/v1/fact/list?quarantined=1", ""},
	} {
		status, body := doRaw(t, worker, p.method, p.path, fmt.Sprintf("trace-fr-%d", i), secret, p.body)
		if status != http.StatusForbidden || body != MsgControlSecretRejected {
			t.Fatalf("%s %s from a live worker = %d %q, want 403", p.method, p.path, status, body)
		}
	}
	f, err := fe.GetFact(context.Background(), id)
	if err != nil || f.ConfirmedAt.Valid {
		t.Fatalf("fact after worker calls = %+v %v, want still unconfirmed", f, err)
	}
	var neg int
	if err := st.DB().QueryRow(`SELECT count(*) FROM evidence WHERE fact_id = ? AND polarity = -1`, id).Scan(&neg); err != nil || neg != 0 {
		t.Fatalf("negative evidence = %d (%v), want 0", neg, err)
	}
}
