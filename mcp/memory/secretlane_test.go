package memory

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// FIX-U-3: memory_write's ingest-time secret-lane seam. The classifier here
// is a fake (the production adapter is kahyad's deterministic
// secretlane.ClassifyDeterministic, covered in kahyad/internal/server); the
// IBAN text only documents what a real hit looks like.
const u3IBAN = "Kira için hesap: TR33 0006 1005 1978 6457 8413 26, aylık 42.000 TL"

type fakeContentClassifier struct {
	secret   bool
	category string
	calls    int
}

func (f *fakeContentClassifier) ClassifySecretLane(string) (bool, string) {
	f.calls++
	return f.secret, f.category
}

type laneCall struct{ taskID, traceID, category string }

type fakeLaneEscalator struct {
	calls []laneCall
	err   error
}

func (f *fakeLaneEscalator) EscalateSecretLane(_ context.Context, taskID, traceID, category string) error {
	f.calls = append(f.calls, laneCall{taskID, traceID, category})
	return f.err
}

type fakeSensitiveMarker struct {
	calls [][2]string
	err   error
}

func (f *fakeSensitiveMarker) MarkSensitiveRead(_ context.Context, sessionKey, traceID string) error {
	f.calls = append(f.calls, [2]string{sessionKey, traceID})
	return f.err
}

// prefixGlobMatcher matches "<prefix>**" globs by prefix - enough for these
// tests (mcp/memory does not import doublestar).
type prefixGlobMatcher struct{}

func (prefixGlobMatcher) Match(pattern, p string) (bool, error) {
	return strings.HasPrefix(p, strings.TrimSuffix(pattern, "**")), nil
}

type erroringGlobMatcher struct{}

func (erroringGlobMatcher) Match(string, string) (bool, error) { return false, errors.New("bad glob") }

func secretSeam(memDir string, c ContentClassifier, m SensitiveMarker, e LaneEscalator) SecretLaneSeam {
	return SecretLaneSeam{
		Classifier: c, Marker: m, Escalator: e, Dir: "gizli",
		Globs: []string{filepath.Join(memDir, "gizli", "**")}, Matcher: prefixGlobMatcher{},
	}
}

// seedCommit gives the fixture repo a first commit, so a test can count
// commits before and after a refused write.
func seedCommit(t *testing.T, repoRoot string) {
	t.Helper()
	cmd := exec.Command("git", "-C", repoRoot, "commit", "-q", "--allow-empty", "-m", "seed")
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=seed", "GIT_AUTHOR_EMAIL=seed@local",
		"GIT_COMMITTER_NAME=seed", "GIT_COMMITTER_EMAIL=seed@local",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seed commit: %v\n%s", err, out)
	}
}

func eventKinds(led *fakeLedger) []string {
	kinds := make([]string, len(led.events))
	for i, e := range led.events {
		kinds[i] = e.kind
	}
	return kinds
}

func assertNotExist(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s exists (stat err %v), want no file", p, err)
		}
	}
}

func TestHandleWriteSecretContentLandsUnderGizli(t *testing.T) {
	repoRoot, memDir := newFixtureRepo(t)
	idx := &fakeIndexer{episodeID: 7}
	led := &fakeLedger{}
	esc := &fakeLaneEscalator{}
	mk := &fakeSensitiveMarker{}
	s := newTestServer(memDir, &fakeSearcher{}, idx, led)
	s.SetSecretLaneSeam(secretSeam(memDir, &fakeContentClassifier{secret: true, category: "finans"}, mk, esc))

	out, err := s.HandleWriteForTask(context.Background(), "trace-u3", "task-u3", MemoryWriteArgs{Content: u3IBAN, File: "inbox/2026-10-13.md"})
	if err != nil {
		t.Fatalf("HandleWriteForTask: %v", err)
	}
	const want = "gizli/notes/inbox/2026-10-13.md"
	if out.File != want || !out.SecretLane || out.EpisodeID != 7 || out.CommitSHA == "" {
		t.Fatalf("out = %+v, want File %s, SecretLane true, EpisodeID 7, a commit", out, want)
	}
	raw, err := os.ReadFile(filepath.Join(memDir, "gizli", "notes", "inbox", "2026-10-13.md"))
	if err != nil {
		t.Fatalf("read redirected note: %v", err)
	}
	if !strings.HasPrefix(string(raw), "---\nkahya_source_tier: agent_derived\n---\n") || !strings.Contains(string(raw), u3IBAN) {
		t.Fatalf("redirected note = %q, want agent_derived front matter + the content", raw)
	}
	assertNotExist(t, filepath.Join(memDir, "inbox", "2026-10-13.md"))
	if !reflect.DeepEqual(mk.calls, [][2]string{{"trace-u3", "trace-u3"}}) {
		t.Errorf("sensitive marks = %v, want [[trace-u3 trace-u3]]", mk.calls)
	}
	if !reflect.DeepEqual(esc.calls, []laneCall{{"task-u3", "trace-u3", "finans"}}) {
		t.Errorf("escalations = %v, want [{task-u3 trace-u3 finans}]", esc.calls)
	}
	if !reflect.DeepEqual(idx.calls, []string{want}) {
		t.Errorf("reindex calls = %v, want [%s]", idx.calls, want)
	}
	if msg := runGitT(t, repoRoot, "log", "-1", "--format=%s"); msg != "memory_write: "+want {
		t.Errorf("commit subject = %q, want memory_write: %s", msg, want)
	}
	if author := runGitT(t, repoRoot, "log", "-1", "--format=%an"); author != "kahyad" {
		t.Errorf("commit author = %q, want kahyad", author)
	}
	if got := eventKinds(led); !reflect.DeepEqual(got, []string{"memory_write", EventMemoryWriteSecretLaneRedirected}) {
		t.Fatalf("ledger kinds = %v, want [memory_write %s]", got, EventMemoryWriteSecretLaneRedirected)
	}
	w := led.events[0].payload
	if w["file"] != want || w["secret_lane"] != true || w["task_id"] != "task-u3" {
		t.Errorf("memory_write payload = %v, want file %s, secret_lane true, task_id task-u3", w, want)
	}
	r := led.events[1].payload
	if r["requested_file"] != "inbox/2026-10-13.md" || r["file"] != want || r["category"] != "finans" || r["commit_sha"] != out.CommitSHA || r["task_id"] != "task-u3" {
		t.Errorf("redirect payload = %v, want requested inbox/2026-10-13.md -> %s, finans, the commit", r, want)
	}
	for _, e := range led.events {
		if e.traceID != "trace-u3" {
			t.Errorf("%s ledgered under %q, want trace-u3", e.kind, e.traceID)
		}
	}
}

func TestHandleWriteSecretDefaultFileRedirectsUnderGizliNotesInbox(t *testing.T) {
	_, memDir := newFixtureRepo(t)
	s := newTestServer(memDir, &fakeSearcher{}, &fakeIndexer{}, &fakeLedger{})
	s.Location = time.UTC
	s.SetSecretLaneSeam(secretSeam(memDir, &fakeContentClassifier{secret: true, category: "kimlik"}, nil, &fakeLaneEscalator{}))

	before := "gizli/notes/" + defaultInboxFile(time.Now(), time.UTC)
	out, err := s.HandleWriteForTask(context.Background(), "trace-u3", "task-u3", MemoryWriteArgs{Content: "TC kimlik numaram 10000000146"})
	after := "gizli/notes/" + defaultInboxFile(time.Now(), time.UTC)
	if err != nil {
		t.Fatalf("HandleWriteForTask: %v", err)
	}
	if out.File != before && out.File != after {
		t.Fatalf("out.File = %q, want %q (the default inbox day file, redirected)", out.File, before)
	}
}

func TestHandleWriteSecretPathAlreadyUnderGizliNotDoublePrefixed(t *testing.T) {
	_, memDir := newFixtureRepo(t)
	s := newTestServer(memDir, &fakeSearcher{}, &fakeIndexer{}, &fakeLedger{})
	s.SetSecretLaneSeam(secretSeam(memDir, &fakeContentClassifier{secret: true}, nil, &fakeLaneEscalator{}))

	for _, file := range []string{"gizli/x.md", "gizli/notes/inbox/y.md"} {
		out, err := s.HandleWriteForTask(context.Background(), "trace-u3", "task-u3", MemoryWriteArgs{Content: u3IBAN, File: file})
		if err != nil {
			t.Fatalf("HandleWriteForTask(%s): %v", file, err)
		}
		if out.File != file || !out.SecretLane {
			t.Errorf("out = %+v, want File %s kept as is, SecretLane true", out, file)
		}
	}
}

func TestHandleWriteSecretRedirectCleansInDirDotDot(t *testing.T) {
	_, memDir := newFixtureRepo(t)
	esc := &fakeLaneEscalator{}
	s := newTestServer(memDir, &fakeSearcher{}, &fakeIndexer{}, &fakeLedger{})
	s.SetSecretLaneSeam(secretSeam(memDir, &fakeContentClassifier{secret: true}, nil, esc))

	out, err := s.HandleWriteForTask(context.Background(), "trace-u3", "task-u3", MemoryWriteArgs{Content: u3IBAN, File: "a/../b.md"})
	if err != nil {
		t.Fatalf("HandleWriteForTask(a/../b.md): %v", err)
	}
	if out.File != "gizli/notes/b.md" {
		t.Fatalf("out.File = %q, want gizli/notes/b.md", out.File)
	}

	esc.calls = nil
	if _, err := s.HandleWriteForTask(context.Background(), "trace-u3", "task-u3", MemoryWriteArgs{Content: u3IBAN, File: "../x.md"}); err == nil {
		t.Fatal("HandleWriteForTask(../x.md) = nil error, want the path refused first")
	}
	if len(esc.calls) != 0 {
		t.Errorf("escalations after a refused path = %v, want none (the path is validated first)", esc.calls)
	}
	assertNotExist(t, filepath.Join(filepath.Dir(memDir), "x.md"), filepath.Join(memDir, "gizli", "notes", "x.md"))
}

func TestHandleWriteNormalContentUnchangedWithSeamWired(t *testing.T) {
	_, memDir := newFixtureRepo(t)
	led := &fakeLedger{}
	esc := &fakeLaneEscalator{}
	mk := &fakeSensitiveMarker{}
	cls := &fakeContentClassifier{}
	s := newTestServer(memDir, &fakeSearcher{}, &fakeIndexer{}, led)
	s.SetSecretLaneSeam(secretSeam(memDir, cls, mk, esc))

	out, err := s.HandleWriteForTask(context.Background(), "trace-n", "task-n", MemoryWriteArgs{Content: "Dünkü toplantı notları", File: "inbox/n.md"})
	if err != nil {
		t.Fatalf("HandleWriteForTask: %v", err)
	}
	if out.File != "inbox/n.md" || out.SecretLane {
		t.Fatalf("out = %+v, want inbox/n.md, SecretLane false", out)
	}
	if cls.calls != 1 || len(esc.calls) != 0 || len(mk.calls) != 0 {
		t.Errorf("classify/escalate/mark calls = %d/%d/%d, want 1/0/0", cls.calls, len(esc.calls), len(mk.calls))
	}
	if got := eventKinds(led); !reflect.DeepEqual(got, []string{"memory_write"}) || led.events[0].payload["secret_lane"] != false {
		t.Errorf("ledger = %+v, want one memory_write with secret_lane false", led.events)
	}
}

func TestHandleWriteSecretSensitiveMarkFailureRefusesWrite(t *testing.T) {
	repoRoot, memDir := newFixtureRepo(t)
	seedCommit(t, repoRoot)
	commitsBefore := runGitT(t, repoRoot, "rev-list", "--count", "HEAD")
	led := &fakeLedger{}
	esc := &fakeLaneEscalator{}
	idx := &fakeIndexer{}
	s := newTestServer(memDir, &fakeSearcher{}, idx, led)
	s.SetSecretLaneSeam(secretSeam(memDir, &fakeContentClassifier{secret: true, category: "finans"}, &fakeSensitiveMarker{err: errors.New("disk")}, esc))

	_, err := s.HandleWriteForTask(context.Background(), "trace-u3", "task-u3", MemoryWriteArgs{Content: u3IBAN, File: "inbox/m.md"})
	if err == nil || err.Error() != MsgMemoryWriteSecretLaneRefused {
		t.Fatalf("err = %v, want %q", err, MsgMemoryWriteSecretLaneRefused)
	}
	if len(esc.calls) != 0 {
		t.Errorf("escalations = %v, want none (the mark comes first)", esc.calls)
	}
	assertNotExist(t, filepath.Join(memDir, "inbox", "m.md"), filepath.Join(memDir, "gizli", "notes", "inbox", "m.md"))
	if got := runGitT(t, repoRoot, "rev-list", "--count", "HEAD"); got != commitsBefore {
		t.Errorf("commit count = %s, want %s (nothing committed)", got, commitsBefore)
	}
	if len(idx.calls) != 0 {
		t.Errorf("reindex calls = %v, want none", idx.calls)
	}
	if got := eventKinds(led); !reflect.DeepEqual(got, []string{EventMemoryWriteSecretLaneRefused}) || led.events[0].payload["reason"] != "sensitive_mark_failed" {
		t.Fatalf("ledger = %+v, want one %s (sensitive_mark_failed)", led.events, EventMemoryWriteSecretLaneRefused)
	}
}

func TestHandleWriteSecretEscalateFailureRefusesWrite(t *testing.T) {
	repoRoot, memDir := newFixtureRepo(t)
	seedCommit(t, repoRoot)
	commitsBefore := runGitT(t, repoRoot, "rev-list", "--count", "HEAD")
	led := &fakeLedger{}
	mk := &fakeSensitiveMarker{}
	s := newTestServer(memDir, &fakeSearcher{}, &fakeIndexer{}, led)
	s.SetSecretLaneSeam(secretSeam(memDir, &fakeContentClassifier{secret: true, category: "finans"}, mk, &fakeLaneEscalator{err: errors.New("db down")}))

	_, err := s.HandleWriteForTask(context.Background(), "trace-u3", "task-u3", MemoryWriteArgs{Content: u3IBAN, File: "inbox/e.md"})
	if err == nil || err.Error() != MsgMemoryWriteSecretLaneRefused {
		t.Fatalf("err = %v, want %q", err, MsgMemoryWriteSecretLaneRefused)
	}
	if len(mk.calls) != 1 {
		t.Errorf("sensitive marks = %v, want 1 (the mark landed before the escalation failed)", mk.calls)
	}
	assertNotExist(t, filepath.Join(memDir, "inbox", "e.md"), filepath.Join(memDir, "gizli", "notes", "inbox", "e.md"))
	if got := runGitT(t, repoRoot, "rev-list", "--count", "HEAD"); got != commitsBefore {
		t.Errorf("commit count = %s, want %s (nothing committed)", got, commitsBefore)
	}
	if got := eventKinds(led); !reflect.DeepEqual(got, []string{EventMemoryWriteSecretLaneRefused}) || led.events[0].payload["reason"] != "lane_escalate_failed" {
		t.Fatalf("ledger = %+v, want one %s (lane_escalate_failed), no memory_write", led.events, EventMemoryWriteSecretLaneRefused)
	}
	if led.events[0].payload["err"] != "db down" {
		t.Errorf("refusal err = %v, want db down", led.events[0].payload["err"])
	}
}

func TestHandleWriteSecretWithoutGlobFailsClosed(t *testing.T) {
	_, memDir := newFixtureRepo(t)
	led := &fakeLedger{}
	esc := &fakeLaneEscalator{}
	s := newTestServer(memDir, &fakeSearcher{}, &fakeIndexer{}, led)
	seam := secretSeam(memDir, &fakeContentClassifier{secret: true, category: "finans"}, nil, esc)
	seam.Globs = nil
	s.SetSecretLaneSeam(seam)

	_, err := s.HandleWriteForTask(context.Background(), "trace-u3", "task-u3", MemoryWriteArgs{Content: u3IBAN, File: "inbox/g.md"})
	if err == nil || err.Error() != MsgMemoryWriteSecretGlobMissing {
		t.Fatalf("err = %v, want %q", err, MsgMemoryWriteSecretGlobMissing)
	}
	if len(esc.calls) != 1 {
		t.Errorf("escalations = %v, want 1 (the task is escalated anyway - the safe direction)", esc.calls)
	}
	assertNotExist(t, filepath.Join(memDir, "inbox", "g.md"), filepath.Join(memDir, "gizli", "notes", "inbox", "g.md"))
	if len(led.events) != 1 || led.events[0].kind != EventMemoryWriteSecretLaneRefused || led.events[0].payload["reason"] != "secret_glob_missing" {
		t.Fatalf("ledger = %+v, want one %s (secret_glob_missing)", led.events, EventMemoryWriteSecretLaneRefused)
	}

	// A glob that cannot be evaluated is no coverage either.
	seam = secretSeam(memDir, &fakeContentClassifier{secret: true}, nil, &fakeLaneEscalator{})
	seam.Matcher = erroringGlobMatcher{}
	s.SetSecretLaneSeam(seam)
	if _, err := s.HandleWriteForTask(context.Background(), "trace-u3", "task-u3", MemoryWriteArgs{Content: u3IBAN, File: "inbox/g.md"}); err == nil || err.Error() != MsgMemoryWriteSecretGlobMissing {
		t.Fatalf("err with an erroring matcher = %v, want %q", err, MsgMemoryWriteSecretGlobMissing)
	}
	assertNotExist(t, filepath.Join(memDir, "gizli", "notes", "inbox", "g.md"))
}

func TestHandleWriteSecretMatcherUnwiredFailsClosed(t *testing.T) {
	_, memDir := newFixtureRepo(t)
	led := &fakeLedger{}
	s := newTestServer(memDir, &fakeSearcher{}, &fakeIndexer{}, led)
	seam := secretSeam(memDir, &fakeContentClassifier{secret: true}, nil, &fakeLaneEscalator{})
	seam.Matcher = nil
	s.SetSecretLaneSeam(seam)

	_, err := s.HandleWriteForTask(context.Background(), "trace-u3", "task-u3", MemoryWriteArgs{Content: u3IBAN, File: "inbox/u.md"})
	if err == nil || err.Error() != MsgMemoryWriteSecretLaneRefused {
		t.Fatalf("err = %v, want %q", err, MsgMemoryWriteSecretLaneRefused)
	}
	assertNotExist(t, filepath.Join(memDir, "inbox", "u.md"), filepath.Join(memDir, "gizli", "notes", "inbox", "u.md"))
	if len(led.events) != 1 || led.events[0].payload["reason"] != "matcher_unwired" {
		t.Fatalf("ledger = %+v, want one refusal (matcher_unwired)", led.events)
	}
}

func TestHandleWriteNilSeamIsHEADBehaviour(t *testing.T) {
	_, memDir := newFixtureRepo(t)
	led := &fakeLedger{}
	s := newTestServer(memDir, &fakeSearcher{}, &fakeIndexer{}, led)

	out, err := s.HandleWrite(context.Background(), "trace-x", MemoryWriteArgs{Content: u3IBAN, File: "inbox/x.md"})
	if err != nil {
		t.Fatalf("HandleWrite: %v", err)
	}
	if out.File != "inbox/x.md" || out.SecretLane {
		t.Fatalf("out = %+v, want inbox/x.md, SecretLane false (no seam wired)", out)
	}
	if len(led.events) != 1 || led.events[0].kind != "memory_write" ||
		led.events[0].payload["secret_lane"] != false || led.events[0].payload["task_id"] != "" {
		t.Fatalf("ledger = %+v, want one memory_write (secret_lane false, task_id \"\")", led.events)
	}
}

// TestMCPMemoryWriteToolPassesTaskHeader: the registered memory_write tool
// hands the X-Kahya-Task-Id header to the escalator (kahyad pins it to the
// attributed live worker before it reaches this package).
func TestMCPMemoryWriteToolPassesTaskHeader(t *testing.T) {
	if got := taskIDFromRequest(nil); got != "" {
		t.Fatalf("taskIDFromRequest(nil) = %q, want empty", got)
	}
	_, memDir := newFixtureRepo(t)
	esc := &fakeLaneEscalator{}
	s := newTestServer(memDir, &fakeSearcher{}, &fakeIndexer{}, &fakeLedger{})
	s.SetSecretLaneSeam(secretSeam(memDir, &fakeContentClassifier{secret: true, category: "saglik"}, nil, esc))

	callMemoryWriteTool(t, s, map[string]string{traceHeader: "trace-h", taskHeader: "task-h"}, `{"content":"x","file":"inbox/h.md"}`)
	if !reflect.DeepEqual(esc.calls, []laneCall{{"task-h", "trace-h", "saglik"}}) {
		t.Fatalf("escalations = %v, want [{task-h trace-h saglik}]", esc.calls)
	}
	if _, err := os.Stat(filepath.Join(memDir, "gizli", "notes", "inbox", "h.md")); err != nil {
		t.Fatalf("redirected note: %v", err)
	}
}

// callMemoryWriteTool drives the REGISTERED memory_write tool (MCPServer)
// through the SDK's stateless streamable-HTTP handler in-process (no
// listener), with headers, exactly as kahyad mounts it at /v1/mcp.
func callMemoryWriteTool(t *testing.T, s *Server, headers map[string]string, args string) string {
	t.Helper()
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.MCPServer(nil) },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"memory_write","arguments":` + args + `}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"isError":true`) {
		t.Fatalf("memory_write tool call = %d %s, want a successful result", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}
