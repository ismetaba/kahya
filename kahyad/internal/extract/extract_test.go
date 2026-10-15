package extract

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"kahya/kahyad/internal/factengine"
	"kahya/kahyad/internal/reader"
	"kahya/kahyad/internal/router"
	"kahya/kahyad/internal/secretlane"
	"kahya/kahyad/internal/taint"
)

func TestExtractEpisodeWritesLinkedAgentDerivedFacts(t *testing.T) {
	f := newFixture(t, emreAcme)
	epID, chunkIDs := f.seedEpisode("inbox/2026-10-08.md", "Emre Acme'de işe başladı.")
	ctx := context.Background()

	st, err := f.runner.ExtractEpisode(ctx, "tr-extract", epID, factengine.ProvenanceAgentDerived)
	if err != nil {
		t.Fatalf("ExtractEpisode: %v", err)
	}
	if st != (Stats{Episodes: 1, Facts: 1, Entities: 2}) {
		t.Fatalf("stats = %+v, want 1 episode, 1 fact, 2 entities", st)
	}
	fact := f.onlyFact()
	if fact.SourceTier != factengine.TierAgentDerived || fact.ConfirmedAt.Valid {
		t.Fatalf("fact tier/confirmed = %q/%v, want agent_derived quarantined", fact.SourceTier, fact.ConfirmedAt.Valid)
	}
	if !fact.SubjectEntityID.Valid || !fact.ObjectEntityID.Valid || fact.SubjectEntityID.Int64 == fact.ObjectEntityID.Int64 {
		t.Fatalf("entity links = %+v / %+v, want two distinct entities", fact.SubjectEntityID, fact.ObjectEntityID)
	}
	if want := fmt.Sprintf("episode:%d,chunk:%d", epID, chunkIDs[0]); fact.Evidence.String != want {
		t.Fatalf("facts.evidence = %q, want %q (raw evidence, never a summary)", fact.Evidence.String, want)
	}
	if fact.Quote.String != "Emre Acme'de" || fact.ExtractorVer.String != ExtractorVersion || fact.Evidentiality != factengine.Reported {
		t.Fatalf("quote/extractor_ver/evidentiality = %q/%q/%q", fact.Quote.String, fact.ExtractorVer.String, fact.Evidentiality)
	}
	if n := f.count(`SELECT count(*) FROM evidence WHERE fact_id = ? AND episode_id = ? AND session_id = ?`, fact.ID, epID, SessionIDForEpisode(epID)); n != 1 {
		t.Fatalf("evidence rows citing the episode under its session = %d, want 1", n)
	}
	ep := f.episode(epID)
	if !ep.ExtractedAt.Valid || ep.ExtractorVer.String != ExtractorVersion {
		t.Fatalf("episode marker = %+v / %+v, want stamped", ep.ExtractedAt, ep.ExtractorVer)
	}
	rows := f.events(EventEpisodeExtracted)
	if len(rows) != 1 || rows[0]["episode_id"] != fmt.Sprint(epID) || rows[0]["lane"] != router.LaneNormal || rows[0]["facts_written"] != "1" || rows[0]["entities_created"] != "2" {
		t.Fatalf("extract.episode_extracted rows = %v", rows)
	}
	if c := f.reader.call(0); c.forceSecret || len(c.chunks) != 1 || c.chunks[0].ID != chunkIDs[0] {
		t.Fatalf("reader call = %+v, want one normal-path call with the episode's chunk", c)
	}
	// The alias the model listed is on the entity.
	if n := f.count(`SELECT count(*) FROM entity_aliases WHERE entity_id = ? AND alias = 'Emre Bey'`, fact.SubjectEntityID.Int64); n != 1 {
		t.Fatalf("alias rows = %d, want the extracted alias on Emre", n)
	}
}

// recordingEngine passes every call through to a real engine and records
// the candidates the extractor built.
type recordingEngine struct {
	*factengine.Engine
	mu         sync.Mutex
	candidates []factengine.Candidate
}

func (e *recordingEngine) WriteFact(ctx context.Context, traceID string, c factengine.Candidate) (int64, error) {
	e.mu.Lock()
	e.candidates = append(e.candidates, c)
	e.mu.Unlock()
	return e.Engine.WriteFact(ctx, traceID, c)
}

// TestExtractEpisodeTierClaimCannotMintTrust: the extractor's candidates
// carry the call site's provenance and no claimed tier; and even the same
// candidate with a forged ClaimedSourceTier stays agent_derived and is
// ledgered factengine.tier_clamped (HANDOFF §5 memory #1).
func TestExtractEpisodeTierClaimCannotMintTrust(t *testing.T) {
	f := newFixture(t, emreAcme)
	rec := &recordingEngine{Engine: f.engine}
	f.runner.Engine = rec
	epID, _ := f.seedEpisode("inbox/a.md", "Emre Acme'de çalışıyor.")
	if _, err := f.runner.ExtractEpisode(context.Background(), "tr-tier", epID, factengine.ProvenanceAgentDerived); err != nil {
		t.Fatalf("ExtractEpisode: %v", err)
	}
	if len(rec.candidates) != 1 {
		t.Fatalf("candidates = %d, want 1", len(rec.candidates))
	}
	c := rec.candidates[0]
	if c.ClaimedSourceTier != "" || c.Provenance != factengine.ProvenanceAgentDerived || c.ExtractorVer != ExtractorVersion {
		t.Fatalf("candidate = %+v, want no claimed tier, agent_derived provenance", c)
	}
	forged := c
	forged.ClaimedSourceTier = factengine.TierUserAsserted
	forged.SessionID = "forged-session"
	id, err := f.engine.WriteFact(context.Background(), "tr-tier", forged)
	if err != nil {
		t.Fatalf("WriteFact(forged): %v", err)
	}
	got, err := f.engine.GetFact(context.Background(), id)
	if err != nil || got.SourceTier != factengine.TierAgentDerived {
		t.Fatalf("forged fact tier = %q (%v), want agent_derived", got.SourceTier, err)
	}
	if rows := f.events(factengine.EventTierClamped); len(rows) != 1 || rows[0]["reason"] != "extractor_claimed_tier_ignored" {
		t.Fatalf("tier_clamped rows = %v", rows)
	}
}

func TestExtractEpisodeIdempotentSameSessionNoConfidenceInflation(t *testing.T) {
	f := newFixture(t, emreAcme)
	epID, _ := f.seedEpisode("inbox/a.md", "Emre Acme'de işe başladı.")
	ctx := context.Background()
	if _, err := f.runner.ExtractEpisode(ctx, "tr-1", epID, factengine.ProvenanceAgentDerived); err != nil {
		t.Fatal(err)
	}
	first := f.onlyFact()
	for i := 0; i < 3; i++ {
		st, err := f.runner.ExtractEpisode(ctx, "tr-again", epID, factengine.ProvenanceAgentDerived)
		if err != nil || st.Episodes != 1 {
			t.Fatalf("re-extraction %d = %+v, %v", i, st, err)
		}
	}
	again := f.onlyFact()
	if again.ID != first.ID || again.Confidence != first.Confidence {
		t.Fatalf("after re-extraction fact %d conf %v, first %d conf %v; want the same row and confidence", again.ID, again.Confidence, first.ID, first.Confidence)
	}
	if n := f.count(`SELECT count(*) FROM evidence WHERE fact_id = ?`, first.ID); n != 1 {
		t.Fatalf("evidence rows = %d, want 1 (same-session repeat is one evidence)", n)
	}
	if n := f.count(`SELECT count(*) FROM entities`); n != 2 {
		t.Fatalf("entities = %d, want 2 (an unambiguous exact alias is reused, MEM-2.a)", n)
	}
}

func TestExtractEpisodeSecretPathForcesLocal(t *testing.T) {
	f := newFixture(t, emreAcme)
	epID, _ := f.seedEpisode("finans-notlar.md", "Emre Acme'de muhasebeci.")
	if _, err := f.runner.ExtractEpisode(context.Background(), "tr-secret", epID, factengine.ProvenanceAgentDerived); err != nil {
		t.Fatalf("ExtractEpisode: %v", err)
	}
	if f.reader.callCount() != 1 || !f.reader.call(0).forceSecret {
		t.Fatalf("reader calls = %d (forceSecret %v), want one forced-local call", f.reader.callCount(), f.reader.callCount() > 0 && f.reader.call(0).forceSecret)
	}
	if f.guardCount() != 0 {
		t.Fatalf("guard classifier called %d times for a secret-path episode, want 0 (the path decides first)", f.guardCount())
	}
	if rows := f.events(EventEpisodeExtracted); len(rows) != 1 || rows[0]["lane"] != router.LaneSecret {
		t.Fatalf("episode_extracted rows = %v, want lane secret", rows)
	}
	// The fact keeps its episode citation; the path glob keeps it off the cloud.
	fact := f.onlyFact()
	if ok, err := f.runner.Lane.CloudEligible(context.Background(), fact); err != nil || ok {
		t.Fatalf("CloudEligible = %v, %v; want false for a secret-path fact", ok, err)
	}
}

func TestExtractEpisodeWithoutPathForcesLocal(t *testing.T) {
	f := newFixture(t, emreAcme)
	epID, _ := f.seedEpisode("", "Emre Acme'de.")
	if _, err := f.runner.ExtractEpisode(context.Background(), "tr-nopath", epID, factengine.ProvenanceAgentDerived); err != nil {
		t.Fatalf("ExtractEpisode: %v", err)
	}
	if !f.reader.call(0).forceSecret || f.guardCount() != 0 {
		t.Fatalf("forceSecret = %v, guard calls = %d; an unclassifiable path must stay local", f.reader.call(0).forceSecret, f.guardCount())
	}
}

// localAnswer is a reader.LocalModel that answers every call with Emre
// works_at Acme citing the first "### CHUNK <id>" of its input.
type localAnswer struct{ calls int }

func (l *localAnswer) Read(ctx context.Context, systemPrompt, rawText string) (string, error) {
	l.calls++
	var id int64
	if _, err := fmt.Sscanf(rawText, "### CHUNK %d", &id); err != nil {
		return "", err
	}
	return fmt.Sprintf(`{"entities":[{"name":"Emre","kind":"person","aliases":[]}],"facts":[{"chunk_id":%d,"subject":"Emre","predicate":"works_at","object":"Acme","evidentiality":"witnessed","quote":"Emre","importance":0.5}]}`, id), nil
}

// TestExtractEpisodeSecretPathNeverReachesCloudModel drives the REAL
// reader.Runner: a classifier that would say "normal" and a cloud model
// that fails the test - a secret-path note is extracted by the local model.
func TestExtractEpisodeSecretPathNeverReachesCloudModel(t *testing.T) {
	f := newFixture(t, nil)
	local := &localAnswer{}
	normal := secretlane.NewClassifier(secretlane.QwenClassifierFunc(func(ctx context.Context, text string) (secretlane.Verdict, error) {
		return secretlane.Verdict{SecretLane: false, Category: secretlane.CategoryNone, Reason: "qwen"}, nil
	}))
	cloud := reader.CloudModelFunc(func(ctx context.Context, jobType string, rawBytes []byte, traceID string) (string, error) {
		t.Fatalf("cloud model reached for a secret-path episode (%d bytes)", len(rawBytes))
		return "", nil
	})
	f.runner.Reader = reader.NewRunner(normal, local, cloud, taint.New(f.st.Queries, f.st), f.st)
	epID, _ := f.seedEpisode("finans-2026.md", "Emre maaşını aldı.", "Acme ödeme yaptı.")
	st, err := f.runner.ExtractEpisode(context.Background(), "tr-real-reader", epID, factengine.ProvenanceAgentDerived)
	if err != nil || st.Episodes != 1 || local.calls != 1 {
		t.Fatalf("stats %+v, err %v, local calls %d; want one local extraction", st, err, local.calls)
	}
}

func TestExtractEpisodeRetractionClosesActiveFact(t *testing.T) {
	retract := false
	f := newFixture(t, func(n int, chunks []reader.ExtractChunk, forceSecret bool) (reader.FactExtractV1, string, error) {
		out, lane, err := emreAcme(n, chunks, forceSecret)
		out.Facts[0].Retraction = retract
		return out, lane, err
	})
	epID, _ := f.seedEpisode("inbox/a.md", "Emre Acme'de.")
	ctx := context.Background()
	if _, err := f.runner.ExtractEpisode(ctx, "tr-1", epID, factengine.ProvenanceAgentDerived); err != nil {
		t.Fatal(err)
	}
	retract = true
	st, err := f.runner.ExtractEpisode(ctx, "tr-2", epID, factengine.ProvenanceAgentDerived)
	if err != nil || st.Retractions != 1 {
		t.Fatalf("retraction run = %+v, %v; want one retraction", st, err)
	}
	fact := f.onlyFact()
	if fact.Status != "retracted" || !fact.ValidTo.Valid {
		t.Fatalf("fact status/valid_to = %q/%v, want retracted with valid_to", fact.Status, fact.ValidTo)
	}
	// Re-running the retraction is a no-op (nothing active any more).
	if st, err := f.runner.ExtractEpisode(ctx, "tr-3", epID, factengine.ProvenanceAgentDerived); err != nil || st.Retractions != 0 || st.Episodes != 1 {
		t.Fatalf("second retraction run = %+v, %v", st, err)
	}
}

// TestExtractEpisodeRetractionNeverClosesHigherTrustFact: a model reading
// an (agent_derived) note never closes a user_asserted fact, nor an
// agent_derived one a user confirmed - extract.retraction_skipped instead.
func TestExtractEpisodeRetractionNeverClosesHigherTrustFact(t *testing.T) {
	f := newFixture(t, func(n int, chunks []reader.ExtractChunk, forceSecret bool) (reader.FactExtractV1, string, error) {
		return reader.FactExtractV1{Facts: []reader.ExtractFactV1{
			{ChunkID: chunks[0].ID, Subject: "Emre", Predicate: "works_at", Object: "Acme", Evidentiality: factengine.Witnessed, Retraction: true},
			{ChunkID: chunks[0].ID, Subject: "Emre", Predicate: "lives_in", Object: "Kadıköy", Evidentiality: factengine.Witnessed, Retraction: true},
		}}, laneOf(forceSecret), nil
	})
	ctx := context.Background()
	tracker := taint.New(f.st.Queries, f.st)
	if err := tracker.InsertClean(ctx, "tr-user", "user-session"); err != nil {
		t.Fatal(err)
	}
	userFact, err := f.engine.WriteFact(ctx, "tr-user", factengine.Candidate{
		Subject: "Emre", Predicate: "works_at", Object: "Acme", Provenance: factengine.ProvenanceUserAsserted,
		SessionID: "user-session", ExtractorVer: "user_direct_v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	confirmed, err := f.engine.WriteFact(ctx, "tr-agent", factengine.Candidate{
		Subject: "Emre", Predicate: "lives_in", Object: "Kadıköy", ExtractorVer: "test_v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.engine.ConfirmFact(ctx, "tr-agent", confirmed); err != nil {
		t.Fatal(err)
	}

	epID, _ := f.seedEpisode("inbox/a.md", "Emre artık Acme'de değil, Kadıköy'den de taşındı.")
	st, err := f.runner.ExtractEpisode(ctx, "tr-retract", epID, factengine.ProvenanceAgentDerived)
	if err != nil || st.Retractions != 0 || st.Episodes != 1 {
		t.Fatalf("stats = %+v, %v; want no retraction", st, err)
	}
	for _, id := range []int64{userFact, confirmed} {
		got, err := f.engine.GetFact(ctx, id)
		if err != nil || got.Status != "active" {
			t.Fatalf("fact %d status = %q (%v), want still active", id, got.Status, err)
		}
	}
	rows := f.events(EventRetractionSkipped)
	if len(rows) != 2 || rows[0]["source_tier"] != factengine.TierUserAsserted || rows[1]["confirmed"] != "true" {
		t.Fatalf("retraction_skipped rows = %v", rows)
	}
}

func TestRunPendingSkipsOnLocalUnavailableAndLeavesMarkerNull(t *testing.T) {
	var f *fixture
	f = newFixture(t, func(n int, chunks []reader.ExtractChunk, forceSecret bool) (reader.FactExtractV1, string, error) {
		if forceSecret {
			return reader.FactExtractV1{}, router.LaneSecret, fmt.Errorf("%w: memory pressure", reader.ErrLocalUnavailable)
		}
		return emreAcme(n, chunks, forceSecret)
	})
	secretEp, _ := f.seedEpisode("finans-a.md", "Banka notu.")
	normalEp, _ := f.seedEpisode("inbox/b.md", "Emre Acme'de.")

	st, err := f.runner.RunPending(context.Background(), "tr-nightly")
	if err != nil {
		t.Fatalf("RunPending: %v", err)
	}
	if st.Episodes != 1 || st.Skipped != 1 {
		t.Fatalf("stats = %+v, want 1 extracted + 1 skipped (the batch goes on)", st)
	}
	if ep := f.episode(secretEp); ep.ExtractedAt.Valid {
		t.Fatalf("skipped episode stamped: %+v", ep.ExtractedAt)
	}
	if ep := f.episode(normalEp); !ep.ExtractedAt.Valid {
		t.Fatal("the next episode of the batch was not extracted")
	}
	rows := f.events(EventExtractionSkipped)
	if len(rows) != 1 || rows[0]["reason"] != SkipLocalUnavailable || rows[0]["episode_id"] != fmt.Sprint(secretEp) || rows[0]["lane"] != router.LaneSecret {
		t.Fatalf("extract.skipped rows = %v", rows)
	}
	// Only the forced-local call was made for the secret episode - no
	// second (cloud) attempt.
	secretCalls := 0
	for i := 0; i < f.reader.callCount(); i++ {
		if f.reader.call(i).forceSecret {
			secretCalls++
		}
	}
	if secretCalls != 1 || f.reader.callCount() != 2 {
		t.Fatalf("reader calls = %d (forced-local %d), want 2 (1)", f.reader.callCount(), secretCalls)
	}
}

func TestRunPendingRespectsBatchLimit(t *testing.T) {
	f := newFixture(t, emreAcme)
	f.runner.Cfg.MaxEpisodesPerRun = 2
	var ids []int64
	for i := 0; i < 5; i++ {
		id, _ := f.seedEpisode(fmt.Sprintf("inbox/%d.md", i), "Emre Acme'de.")
		ids = append(ids, id)
	}
	ctx := context.Background()
	for run, want := range [][]bool{{true, true, false, false, false}, {true, true, true, true, false}, {true, true, true, true, true}} {
		st, err := f.runner.RunPending(ctx, fmt.Sprintf("tr-run-%d", run))
		if err != nil || st.Episodes > 2 {
			t.Fatalf("run %d = %+v, %v; want at most 2 episodes", run, st, err)
		}
		for i, id := range ids {
			if got := f.episode(id).ExtractedAt.Valid; got != want[i] {
				t.Fatalf("after run %d episode %d stamped=%v, want %v", run, i, got, want[i])
			}
		}
	}
	if st, err := f.runner.RunPending(ctx, "tr-run-idle"); err != nil || st != (Stats{}) {
		t.Fatalf("idle run = %+v, %v; want nothing to do", st, err)
	}
}

// TestRunPendingRollsPastPersistentFailures: episodes whose answer is
// always rejected cannot hold every batch slot - the next run starts after
// the last episode the previous run processed, wrapping around.
func TestRunPendingRollsPastPersistentFailures(t *testing.T) {
	var bad map[int64]bool
	f := newFixture(t, func(n int, chunks []reader.ExtractChunk, forceSecret bool) (reader.FactExtractV1, string, error) {
		if bad[chunks[0].ID] {
			return reader.FactExtractV1{}, laneOf(forceSecret), fmt.Errorf("%w: quote not in chunk", reader.ErrRejected)
		}
		return emreAcme(n, chunks, forceSecret)
	})
	f.runner.Cfg.MaxEpisodesPerRun = 2
	bad = map[int64]bool{}
	var good int64
	for i := 0; i < 3; i++ {
		_, chunkIDs := f.seedEpisode(fmt.Sprintf("inbox/bad%d.md", i), "Bozuk.")
		bad[chunkIDs[0]] = true
	}
	good, _ = f.seedEpisode("inbox/good.md", "Emre Acme'de.")
	ctx := context.Background()
	if _, err := f.runner.RunPending(ctx, "tr-1"); err != nil {
		t.Fatal(err)
	}
	if f.episode(good).ExtractedAt.Valid {
		t.Fatal("first run reached the 4th episode with a batch of 2")
	}
	if _, err := f.runner.RunPending(ctx, "tr-2"); err != nil {
		t.Fatal(err)
	}
	if !f.episode(good).ExtractedAt.Valid {
		t.Fatal("second run did not get past the persistently rejected episodes")
	}
	if rows := f.events(EventExtractionSkipped); len(rows) != 3 || rows[0]["reason"] != SkipRejected {
		t.Fatalf("skipped rows = %v, want 3 rejected", rows)
	}
	// The third run wraps around to the first rejected episodes again.
	before := len(f.events(EventExtractionSkipped))
	if _, err := f.runner.RunPending(ctx, "tr-3"); err != nil {
		t.Fatal(err)
	}
	if after := len(f.events(EventExtractionSkipped)); after != before+2 {
		t.Fatalf("third run skipped %d episodes, want 2 (wrapped to the lowest ids)", after-before)
	}
}

func TestExtractEpisodeValidFromCoalescesOnReassert(t *testing.T) {
	validFrom := ""
	f := newFixture(t, func(n int, chunks []reader.ExtractChunk, forceSecret bool) (reader.FactExtractV1, string, error) {
		out, lane, err := emreAcme(n, chunks, forceSecret)
		out.Facts[0].ValidFrom = validFrom
		return out, lane, err
	})
	epID, _ := f.seedEpisode("inbox/a.md", "Emre Acme'de.")
	ctx := context.Background()
	for _, vf := range []string{"", "2026-05-01T00:00:00+03:00", "2027-01-01T00:00:00Z"} {
		validFrom = vf
		if _, err := f.runner.ExtractEpisode(ctx, "tr", epID, factengine.ProvenanceAgentDerived); err != nil {
			t.Fatal(err)
		}
	}
	fact := f.onlyFact()
	want, _ := time.Parse(time.RFC3339, "2026-05-01T00:00:00+03:00")
	if fact.ValidFrom.String != want.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("valid_from = %q, want the first value given %q (in UTC), never overwritten", fact.ValidFrom.String, want.UTC().Format(time.RFC3339Nano))
	}
}

// TestExtractEpisodeContentSecretFactsNeverCloudEligible: a call whose TEXT
// is secret (the path is ordinary) runs on the local model, and its facts
// carry no episode citation, so memlane never lets them into a cloud block.
func TestExtractEpisodeContentSecretFactsNeverCloudEligible(t *testing.T) {
	f := newFixture(t, emreAcme)
	// A marker outside the deterministic lexicon, so the local classifier
	// (the fixture's Qwen fake) is the one that says secret.
	f.secretOn = "ozel-isaret-77"
	epID, chunkIDs := f.seedEpisode("inbox/a.md", "Emre Acme'de; ozel-isaret-77 geldi.")
	if _, err := f.runner.ExtractEpisode(context.Background(), "tr-content", epID, factengine.ProvenanceAgentDerived); err != nil {
		t.Fatalf("ExtractEpisode: %v", err)
	}
	if f.guardCount() != 1 || !f.reader.call(0).forceSecret {
		t.Fatalf("guard calls %d, forceSecret %v; want the guard to keep the call local", f.guardCount(), f.reader.call(0).forceSecret)
	}
	if rows := f.events(EventCloudGuardBlocked); len(rows) != 1 || rows[0]["chunk_id"] != fmt.Sprint(chunkIDs[0]) {
		t.Fatalf("cloud_guard_blocked rows = %v", rows)
	}
	fact := f.onlyFact()
	if n := f.count(`SELECT count(*) FROM evidence WHERE fact_id = ? AND episode_id IS NULL`, fact.ID); n != 1 {
		t.Fatalf("evidence rows without an episode citation = %d, want 1", n)
	}
	if ok, err := f.runner.Lane.CloudEligible(context.Background(), fact); err != nil || ok {
		t.Fatalf("CloudEligible = %v, %v; want false", ok, err)
	}
}

// TestExtractEpisodeGuardClassifiesTheBytesTheReaderSends: the guard sees
// exactly reader.RenderExtractInput(call) for every call (no byte of a call
// reaches the Reader's cloud path unguarded), in ChunksPerCall groups.
func TestExtractEpisodeGuardClassifiesTheBytesTheReaderSends(t *testing.T) {
	f := newFixture(t, emreAcme)
	f.runner.Cfg.ChunksPerCall = 3
	texts := []string{"bir", "iki", "üç", "dört", "beş", "altı", "yedi"}
	epID, _ := f.seedEpisode("inbox/a.md", texts...)
	if _, err := f.runner.ExtractEpisode(context.Background(), "tr-groups", epID, factengine.ProvenanceAgentDerived); err != nil {
		t.Fatal(err)
	}
	if f.reader.callCount() != 3 || f.guardCount() != 3 {
		t.Fatalf("reader calls %d, guard calls %d; want 3 each (3+3+1 chunks)", f.reader.callCount(), f.guardCount())
	}
	for i, size := range []int{3, 3, 1} {
		c := f.reader.call(i)
		if len(c.chunks) != size {
			t.Fatalf("call %d has %d chunks, want %d", i, len(c.chunks), size)
		}
		if f.guarded[i] != reader.RenderExtractInput(c.chunks) {
			t.Fatalf("guard %d classified %q, the reader got %q", i, f.guarded[i], reader.RenderExtractInput(c.chunks))
		}
	}
}

func TestExtractEpisodeTooLargeIsSkipped(t *testing.T) {
	f := newFixture(t, emreAcme)
	f.runner.Cfg.MaxChunksPerEpisode = 2
	epID, _ := f.seedEpisode("inbox/big.md", "a", "b", "c")
	st, err := f.runner.ExtractEpisode(context.Background(), "tr-big", epID, factengine.ProvenanceAgentDerived)
	if err != nil || st != (Stats{Skipped: 1}) || f.reader.callCount() != 0 {
		t.Fatalf("stats %+v, err %v, reader calls %d; want skipped with no model call", st, err, f.reader.callCount())
	}
	if rows := f.events(EventExtractionSkipped); len(rows) != 1 || rows[0]["reason"] != SkipTooLarge {
		t.Fatalf("skipped rows = %v", rows)
	}
}

func TestExtractEpisodeCloudFailureIsSkippedNotRetriedLocally(t *testing.T) {
	f := newFixture(t, func(n int, chunks []reader.ExtractChunk, forceSecret bool) (reader.FactExtractV1, string, error) {
		return reader.FactExtractV1{}, router.LaneNormal, errors.New("reader: cloud model call failed: 401")
	})
	epID, _ := f.seedEpisode("inbox/a.md", "Emre.")
	st, err := f.runner.ExtractEpisode(context.Background(), "tr-cloud", epID, factengine.ProvenanceAgentDerived)
	if err != nil || st.Skipped != 1 || f.reader.callCount() != 1 {
		t.Fatalf("stats %+v, err %v, calls %d", st, err, f.reader.callCount())
	}
	if rows := f.events(EventExtractionSkipped); len(rows) != 1 || rows[0]["reason"] != SkipCloudUnavailable {
		t.Fatalf("skipped rows = %v", rows)
	}
	if f.episode(epID).ExtractedAt.Valid {
		t.Fatal("a failed episode was stamped")
	}
}

// TestExtractEpisodeLinksOnlyExactNamesInTheSameAnswer (MEM-2 step 2): a
// subject/object is linked only when it is byte-equal to a name or alias of
// the same answer, and a string two entities claim is left unlinked.
func TestExtractEpisodeLinksOnlyExactNamesInTheSameAnswer(t *testing.T) {
	f := newFixture(t, func(n int, chunks []reader.ExtractChunk, forceSecret bool) (reader.FactExtractV1, string, error) {
		id := chunks[0].ID
		return reader.FactExtractV1{
			Entities: []reader.ExtractEntityV1{
				{Name: "Emre Yılmaz", Kind: factengine.EntityKindPerson, Aliases: []string{"EY"}},
				{Name: "Acme", Kind: factengine.EntityKindOrg, Aliases: []string{"EY"}},
			},
			Facts: []reader.ExtractFactV1{
				{ChunkID: id, Subject: "Emre", Predicate: "works_at", Object: "Acme", Evidentiality: factengine.Witnessed},
				{ChunkID: id, Subject: "EY", Predicate: "relates_to", Object: "Emre Yılmaz", Evidentiality: factengine.Witnessed},
			},
		}, laneOf(forceSecret), nil
	})
	epID, _ := f.seedEpisode("inbox/a.md", "Emre Acme'de.")
	if _, err := f.runner.ExtractEpisode(context.Background(), "tr-links", epID, factengine.ProvenanceAgentDerived); err != nil {
		t.Fatal(err)
	}
	links := func(pred string) (subject, object sql.NullInt64) {
		if err := f.st.DB().QueryRow(`SELECT subject_entity_id, object_entity_id FROM facts WHERE predicate = ?`, pred).Scan(&subject, &object); err != nil {
			t.Fatal(err)
		}
		return subject, object
	}
	if s, o := links("works_at"); s.Valid || !o.Valid {
		t.Fatalf("works_at links = %v/%v, want subject unlinked (\"Emre\" is not \"Emre Yılmaz\"), object linked", s, o)
	}
	if s, o := links("relates_to"); s.Valid || !o.Valid {
		t.Fatalf("relates_to links = %v/%v, want the alias \"EY\" both entities claim unlinked", s, o)
	}
}

// TestExtractAcrossEpisodesReusesTheSameEntity (MEM-2.a): the same name
// seen in two notes resolves to one non-provisional entity.
func TestExtractAcrossEpisodesReusesTheSameEntity(t *testing.T) {
	f := newFixture(t, emreAcme)
	a, _ := f.seedEpisode("inbox/a.md", "Emre Acme'de.")
	b, _ := f.seedEpisode("inbox/b.md", "Emre yine Acme'de.")
	if st, err := f.runner.RunPending(context.Background(), "tr-two"); err != nil || st.Episodes != 2 {
		t.Fatalf("RunPending = %+v, %v", st, err)
	}
	_, _ = a, b
	if n := f.count(`SELECT count(*) FROM entities`); n != 2 {
		t.Fatalf("entities = %d, want 2 (Emre, Acme) - no namesake explosion", n)
	}
	if n := f.count(`SELECT count(*) FROM entities WHERE provisional = 1`); n != 0 {
		t.Fatalf("provisional entities = %d, want 0", n)
	}
	if rows := f.events(factengine.EventEntityCreated); len(rows) != 2 {
		t.Fatalf("entity_created rows = %v, want 2 (each created once)", rows)
	}
}

func TestExtractRunsAreSerialized(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	f := newFixture(t, func(n int, chunks []reader.ExtractChunk, forceSecret bool) (reader.FactExtractV1, string, error) {
		entered <- struct{}{}
		<-release
		return emreAcme(n, chunks, forceSecret)
	})
	epID, _ := f.seedEpisode("inbox/a.md", "Emre Acme'de.")
	done := make(chan error, 1)
	go func() {
		_, err := f.runner.RunPending(context.Background(), "tr-long")
		done <- err
	}()
	<-entered
	for name, call := range map[string]func() error{
		"RunPending": func() error { _, err := f.runner.RunPending(context.Background(), "tr-x"); return err },
		"ExtractEpisode": func() error {
			_, err := f.runner.ExtractEpisode(context.Background(), "tr-x", epID, factengine.ProvenanceAgentDerived)
			return err
		},
		"ExtractFile": func() error { _, err := f.runner.ExtractFile(context.Background(), "tr-x", "inbox/a.md"); return err },
		"ExtractAll":  func() error { _, err := f.runner.ExtractAll(context.Background(), "tr-x"); return err },
	} {
		if err := call(); !errors.Is(err, ErrInProgress) {
			t.Fatalf("%s during a run = %v, want ErrInProgress", name, err)
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestNightlyDisabledIsANoop(t *testing.T) {
	f := newFixture(t, emreAcme)
	f.runner.Cfg.Enabled = false
	f.seedEpisode("inbox/a.md", "Emre Acme'de.")
	if st, err := f.runner.Nightly(context.Background(), "tr-off"); err != nil || st != (Stats{}) || f.reader.callCount() != 0 {
		t.Fatalf("disabled nightly = %+v, %v, calls %d", st, err, f.reader.callCount())
	}
	f.runner.Cfg.Enabled = true
	if st, err := f.runner.Nightly(context.Background(), "tr-on"); err != nil || st.Episodes != 1 {
		t.Fatalf("enabled nightly = %+v, %v", st, err)
	}
}

func TestRunPendingLedgersRunDoneWithLocalDate(t *testing.T) {
	f := newFixture(t, emreAcme)
	loc := time.FixedZone("UTC+3", 3*3600)
	f.runner.Cfg.Location = loc
	f.runner.Now = func() time.Time { return time.Date(2026, 10, 14, 22, 30, 0, 0, time.UTC) } // 01:30 on the 15th at +03
	epID, _ := f.seedEpisode("inbox/a.md", "Emre Acme'de.")
	if _, err := f.runner.RunPending(context.Background(), "tr-date"); err != nil {
		t.Fatal(err)
	}
	rows := f.events(EventRunDone)
	if len(rows) != 1 || rows[0]["date"] != "2026-10-15" || rows[0]["episodes"] != "1" || rows[0]["last_episode_id"] != fmt.Sprint(epID) {
		t.Fatalf("run_done rows = %v, want the local date 2026-10-15", rows)
	}
}

func TestExtractAllResetsMarkersAndReextracts(t *testing.T) {
	f := newFixture(t, emreAcme)
	epID, _ := f.seedEpisode("inbox/a.md", "Emre Acme'de.")
	ctx := context.Background()
	if _, err := f.runner.RunPending(ctx, "tr-1"); err != nil {
		t.Fatal(err)
	}
	calls := f.reader.callCount()
	if st, err := f.runner.RunPending(ctx, "tr-2"); err != nil || st.Episodes != 0 || f.reader.callCount() != calls {
		t.Fatalf("second run re-extracted: %+v, %v", st, err)
	}
	st, err := f.runner.ExtractAll(ctx, "tr-all")
	if err != nil || st.Episodes != 1 || f.reader.callCount() != calls+1 {
		t.Fatalf("ExtractAll = %+v, %v (calls %d)", st, err, f.reader.callCount())
	}
	if !f.episode(epID).ExtractedAt.Valid {
		t.Fatal("ExtractAll did not stamp the episode again")
	}
}

func TestExtractFileAndEpisodeNotFound(t *testing.T) {
	f := newFixture(t, emreAcme)
	f.seedEpisode("inbox/a.md", "Emre Acme'de.")
	ctx := context.Background()
	if st, err := f.runner.ExtractFile(ctx, "tr", "inbox/a.md"); err != nil || st.Episodes != 1 {
		t.Fatalf("ExtractFile = %+v, %v", st, err)
	}
	if _, err := f.runner.ExtractFile(ctx, "tr", "inbox/none.md"); !errors.Is(err, ErrEpisodeNotFound) {
		t.Fatalf("ExtractFile(unknown) = %v, want ErrEpisodeNotFound", err)
	}
	if _, err := f.runner.ExtractEpisode(ctx, "tr", 9999, factengine.ProvenanceAgentDerived); !errors.Is(err, ErrEpisodeNotFound) {
		t.Fatalf("ExtractEpisode(unknown) = %v, want ErrEpisodeNotFound", err)
	}
	deleted, _ := f.seedEpisode("inbox/gone.md", "x")
	if _, err := f.st.DB().Exec(`UPDATE episodes SET status = 'deleted' WHERE id = ?`, deleted); err != nil {
		t.Fatal(err)
	}
	if _, err := f.runner.ExtractEpisode(ctx, "tr", deleted, factengine.ProvenanceAgentDerived); !errors.Is(err, ErrEpisodeNotFound) {
		t.Fatalf("ExtractEpisode(deleted) = %v, want ErrEpisodeNotFound", err)
	}
}

func TestNormalizeRelPath(t *testing.T) {
	mem := "/Users/u/Kahya/memory"
	for in, want := range map[string]string{
		"inbox/2026-10-08.md":                  "inbox/2026-10-08.md",
		" ./inbox//a.md ":                      "inbox/a.md",
		"/Users/u/Kahya/memory/people/emre.md": "people/emre.md",
		"/Users/u/Kahya/other/x.md":            "",
		"../x.md":                              "",
		"":                                     "",
		".":                                    "",
		"/Users/u/Kahya/memory":                "",
		"inbox/../../x.md":                     "",
		"/Users/u/Kahya/memory/../memory/a.md": "a.md",
	} {
		if got := NormalizeRelPath(mem, in); got != want {
			t.Errorf("NormalizeRelPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestExtractEpisodeLexiconHitForcesLocalWithoutAskingQwen: a deterministic
// secret-lane hit (the sağlık lexicon) in the call's text is final - the
// local model classifier is not even consulted and the call stays local.
func TestExtractEpisodeLexiconHitForcesLocalWithoutAskingQwen(t *testing.T) {
	f := newFixture(t, emreAcme)
	epID, _ := f.seedEpisode("inbox/a.md", "Emre'nin tahlil sonuçları geldi.")
	if _, err := f.runner.ExtractEpisode(context.Background(), "tr-lexicon", epID, factengine.ProvenanceAgentDerived); err != nil {
		t.Fatal(err)
	}
	if f.guardCount() != 0 || !f.reader.call(0).forceSecret {
		t.Fatalf("qwen calls %d, forceSecret %v; want a deterministic local-only decision", f.guardCount(), f.reader.call(0).forceSecret)
	}
	if rows := f.events(EventCloudGuardBlocked); len(rows) != 1 {
		t.Fatalf("cloud_guard_blocked rows = %v", rows)
	}
}
