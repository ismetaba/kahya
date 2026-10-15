package memlane

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/store/sqlcgen"
)

const testMemoryDir = "/m"

// testGlobs mirror policy.yaml's secret_lane_globs shape: absolute globs over
// the memory dir.
var testGlobs = []string{"/m/finans*.md", "/m/gizli/**"}

type policyMatcher struct{}

func (policyMatcher) Match(pattern, path string) (bool, error) {
	return policy.MatchGlob(pattern, path)
}

type fakeStore struct {
	paths map[int64][]sql.NullString
	err   error
	calls int
}

func (f *fakeStore) ListEpisodePathsForFact(_ context.Context, factID int64) ([]sql.NullString, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.paths[factID], nil
}

func paths(ps ...string) []sql.NullString {
	out := make([]sql.NullString, len(ps))
	for i, p := range ps {
		out[i] = sql.NullString{String: p, Valid: true}
	}
	return out
}

func newClassifier(st Store) Classifier {
	return Classifier{Store: st, MemoryDir: testMemoryDir, SecretGlobs: testGlobs, Matcher: policyMatcher{}}
}

func fact(id int64, subject, predicate, object string) sqlcgen.Fact {
	return sqlcgen.Fact{ID: id, Subject: subject, Predicate: predicate, Object: object}
}

func TestPathIsSecret(t *testing.T) {
	c := newClassifier(nil)
	for rel, want := range map[string]bool{
		"finans-2026.md":         true,
		"finans.md":              true,
		"notes/finans.md":        false,
		"gizli/notes/inbox/x.md": true,
		"gizli/x.md":             true,
		"inbox/x.md":             false,
		"notes/ev.md":            false,
	} {
		if got := c.PathIsSecret(rel); got != want {
			t.Errorf("PathIsSecret(%q) = %v, want %v", rel, got, want)
		}
	}
}

// TestPathIsSecretFoldsCaseAndNormalization (FIX-U-3 review): macOS APFS
// stores "Gizli/" and "gizli/" as ONE directory, so the walkers report
// whichever spelling exists on disk; a case, Turkish-I or Unicode
// normalisation variant of a secret path is secret too (over-matching only
// keeps a path local). Ordinary paths stay normal.
func TestPathIsSecretFoldsCaseAndNormalization(t *testing.T) {
	c := newClassifier(nil)
	for rel, want := range map[string]bool{
		"Gizli/notes/inbox/u.md": true,
		"GIZLI/x.md":             true,
		"GİZLİ/x.md":             true, // dotted capital I
		"Finans-2026.md":         true,
		"FINANS.md":              true,
		"inbox/Gizli.md":         false,
		"notes/Finans.md":        false,
		"Inbox/x.md":             false,
	} {
		if got := c.PathIsSecret(rel); got != want {
			t.Errorf("PathIsSecret(%q) = %v, want %v", rel, got, want)
		}
	}
	// A glob spelled in another case (or a memory dir configured so) still
	// covers the stored path.
	upper := Classifier{MemoryDir: "/M", SecretGlobs: []string{"/m/GIZLI/**"}, Matcher: policyMatcher{}}
	if !upper.PathIsSecret("gizli/x.md") {
		t.Error("PathIsSecret with a differently-cased glob and memory dir = false, want true")
	}
	// Decomposed (NFD) and precomposed (NFC) spellings of a non-ASCII glob.
	nfd := Classifier{MemoryDir: testMemoryDir, SecretGlobs: []string{"/m/sağlık/**"}, Matcher: policyMatcher{}}
	if !nfd.PathIsSecret("sağlık/a.md") {
		t.Error("PathIsSecret(NFD spelling of an NFC glob) = false, want true")
	}
	if got := FoldForSecretGlob("/Users/X/Kahya/memory/Gİzli/**"); got != "/users/x/kahya/memory/gizli/**" {
		t.Errorf("FoldForSecretGlob = %q, want /users/x/kahya/memory/gizli/**", got)
	}
}

type erroringMatcher struct{}

func (erroringMatcher) Match(string, string) (bool, error) { return false, errors.New("bad glob") }

// TestPathIsSecretMatcherErrorIsSecret: a glob that cannot be evaluated
// classifies the path as secret (fail-closed). The ritual sampler this
// classification moved from treated a match error as "no match".
func TestPathIsSecretMatcherErrorIsSecret(t *testing.T) {
	c := Classifier{MemoryDir: testMemoryDir, SecretGlobs: testGlobs, Matcher: erroringMatcher{}}
	if !c.PathIsSecret("notes/ev.md") {
		t.Fatal("a glob match error must classify the path as secret")
	}
	if c.Matcher = nil; !c.PathIsSecret("notes/ev.md") {
		t.Fatal("no matcher wired must classify the path as secret")
	}
	empty := Classifier{MemoryDir: testMemoryDir, Matcher: erroringMatcher{}}
	if empty.PathIsSecret("notes/ev.md") {
		t.Fatal("with no secret globs nothing is secret (no glob was evaluated)")
	}
}

func TestCloudEligibleSecretGlobExcludes(t *testing.T) {
	st := &fakeStore{paths: map[int64][]sql.NullString{
		1: paths("notes/ev.md", "finans-2026.md"),
		2: paths("notes/ev.md"),
	}}
	c := newClassifier(st)
	v, err := c.Classify(context.Background(), fact(1, "Emre", "sever", "kahve"))
	if err != nil || v.Eligible || v.Reason != ReasonPathGlob {
		t.Fatalf("fact citing a secret path = %+v, %v; want excluded (path_glob)", v, err)
	}
	ok, err := c.CloudEligible(context.Background(), fact(2, "Emre", "sever", "kahve"))
	if err != nil || !ok {
		t.Fatalf("fact citing only normal paths = %v, %v; want eligible", ok, err)
	}
}

// TestCloudEligibleNoCitationIsFailClosed: no evidence row citing a
// resolvable episode path means no classification record - treated as
// secret, excluded.
func TestCloudEligibleNoCitationIsFailClosed(t *testing.T) {
	st := &fakeStore{paths: map[int64][]sql.NullString{
		1: nil,
		2: {{String: "", Valid: true}, {}},
	}}
	c := newClassifier(st)
	for _, id := range []int64{1, 2, 3} {
		v, err := c.Classify(context.Background(), fact(id, "Emre", "sever", "kahve"))
		if err != nil || v.Eligible || v.Reason != ReasonNoClassification {
			t.Fatalf("fact %d = %+v, %v; want excluded (no_classification_record)", id, v, err)
		}
	}
}

type fakeContent struct {
	secret bool
	err    error
	texts  []string
}

func (f *fakeContent) ClassifySecretLane(_ context.Context, text string) (bool, error) {
	f.texts = append(f.texts, text)
	return f.secret, f.err
}

// TestClassifyContentHalf (FIX-P-17, moved from ritual): the fact's own text
// is classified locally after the path check - the deterministic pre-pass
// always, an optional content classifier on top, fail-closed on its error.
func TestClassifyContentHalf(t *testing.T) {
	st := &fakeStore{paths: map[int64][]sql.NullString{1: paths("notes/ev.md")}}
	c := newClassifier(st)
	iban := fact(1, "Emre", "IBAN", "TR33 0006 1005 1978 6457 8413 26")
	if v, err := c.Classify(context.Background(), iban); err != nil || v.Eligible || v.Reason != ReasonContentDeterministic {
		t.Fatalf("IBAN fact = %+v, %v; want content_deterministic", v, err)
	}
	plain := fact(1, "Emre", "sever", "kahve")
	for _, tc := range []struct {
		content *fakeContent
		want    Verdict
	}{
		{&fakeContent{secret: true}, Verdict{Reason: ReasonContentClassifier}},
		{&fakeContent{err: errors.New("model down")}, Verdict{Reason: ReasonClassifyFailed}},
		{&fakeContent{}, Verdict{Eligible: true}},
	} {
		c.Content = tc.content
		v, err := c.Classify(context.Background(), plain)
		if err != nil || v != tc.want {
			t.Fatalf("content %+v: verdict %+v, %v; want %+v", tc.content, v, err, tc.want)
		}
		if len(tc.content.texts) != 1 || tc.content.texts[0] != "Emre sever kahve" {
			t.Fatalf("classified texts = %q, want the fact text", tc.content.texts)
		}
	}
}

// TestClassifyStoreErrorIsAnError: a failed evidence read is returned (the
// caller then refuses the fact), and a nil store refuses too.
func TestClassifyStoreErrorIsAnError(t *testing.T) {
	c := newClassifier(&fakeStore{err: errors.New("db locked")})
	if _, err := c.Classify(context.Background(), fact(1, "a", "b", "c")); err == nil {
		t.Fatal("a store error must be returned")
	}
	if ok, err := c.CloudEligible(context.Background(), fact(1, "a", "b", "c")); ok || err == nil {
		t.Fatalf("CloudEligible on a store error = %v, %v; want false + error", ok, err)
	}
	c.Store = nil
	if _, err := c.Classify(context.Background(), fact(1, "a", "b", "c")); err == nil {
		t.Fatal("a nil store must be an error (fail-closed)")
	}
}

// TestClassifyOverRealStore: the real ListEpisodePathsForFact query - an
// evidence row with no episode, an episode with no source_path and a
// duplicate citation are skipped exactly like ritual's per-row loop did.
func TestClassifyOverRealStore(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(config.Config{DBPath: filepath.Join(t.TempDir(), "brain.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	const now = "2026-10-15T00:00:00Z"
	ep := func(path string) int64 {
		p := sql.NullString{String: path, Valid: path != ""}
		e, err := st.Queries.InsertEpisode(ctx, sqlcgen.InsertEpisodeParams{Source: "memory_file", SourcePath: p, SourceTier: "user_asserted", Status: "active", CreatedAt: now})
		if err != nil {
			t.Fatal(err)
		}
		return e.ID
	}
	newFact := func(object string, episodes ...int64) sqlcgen.Fact {
		f, err := st.Queries.InsertFact(ctx, sqlcgen.InsertFactParams{
			Subject: "Emre", Predicate: "sever", Object: object, SourceTier: "user_asserted", Evidentiality: "witnessed",
			Status: "active", UpdatedAt: now, CreatedAt: now,
		})
		if err != nil {
			t.Fatal(err)
		}
		for i, e := range episodes {
			var epID any
			if e != 0 {
				epID = e
			}
			if _, err := st.DB().Exec(`INSERT INTO evidence (fact_id, episode_id, session_id, polarity, weight, created_at) VALUES (?, ?, ?, 1, 0.5, ?)`,
				f.ID, epID, "s"+object+string(rune('a'+i)), now); err != nil {
				t.Fatal(err)
			}
		}
		return f
	}
	normal, secret, noPath := ep("notes/ev.md"), ep("finans-2026.md"), ep("")
	c := newClassifier(st.Queries)
	for _, tc := range []struct {
		f    sqlcgen.Fact
		want Verdict
	}{
		{newFact("kahve", normal, normal), Verdict{Eligible: true}},
		{newFact("çay", normal, secret), Verdict{Reason: ReasonPathGlob}},
		{newFact("su", noPath, 0), Verdict{Reason: ReasonNoClassification}},
		{newFact("ayran"), Verdict{Reason: ReasonNoClassification}},
	} {
		v, err := c.Classify(ctx, tc.f)
		if err != nil || v != tc.want {
			t.Fatalf("fact %q = %+v, %v; want %+v", tc.f.Object, v, err, tc.want)
		}
	}
}

func TestFactText(t *testing.T) {
	if got := FactText(fact(1, " Emre", "sever", "kahve ")); got != "Emre sever kahve" {
		t.Fatalf("FactText = %q", got)
	}
}
