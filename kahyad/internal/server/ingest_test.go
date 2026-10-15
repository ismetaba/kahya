package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kahya/kahyad/internal/extract"
	"kahya/kahyad/internal/factengine"
	"kahya/kahyad/internal/ingest"
	"kahya/kahyad/internal/memlane"
	"kahya/kahyad/internal/reader"
	"kahya/kahyad/internal/ritual"
	"kahya/kahyad/internal/secretlane"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/taint"
	"kahya/kahyad/internal/task"
	"kahya/mcp/memory"
)

const ingestRouteDoc = "Zeynep Ankara ofisinde proje yöneticisi olarak başladı.\n"

const ingestRouteAnswer = `{"title":"Zeynep ofis notu","summary":"Zeynep Ankara ofisinde proje yöneticisi oldu.","key_points":["Zeynep Ankara ofisinde."],"dates":[],"amounts":[],` +
	`"entities":[{"name":"Zeynep","kind":"person","aliases":[]}],` +
	`"facts":[{"chunk_id":0,"subject":"Zeynep","predicate":"role_at","object":"Ankara ofisi","evidentiality":"witnessed","quote":"Zeynep Ankara ofisinde proje yöneticisi olarak başladı.","valid_from":"","valid_to":"","importance":0.7,"retraction":false}]}`

// newIngestFixture is the MCP fixture (real store, indexer, searcher and
// process-wide memory server) with a real ingest runner whose Reader models
// are fakes: an ordinary classifier verdict and a cloud model that answers
// ingestRouteAnswer.
func newIngestFixture(t *testing.T, maxBytes int64) (mcpTestFixture, *int) {
	t.Helper()
	cloudCalls := new(int)
	f := newMCPTestFixtureWith(t, testPolicyDoc(), func(srv *Server, st *store.Store) {
		classifier := secretlane.NewClassifier(secretlane.QwenClassifierFunc(func(ctx context.Context, text string) (secretlane.Verdict, error) {
			return secretlane.Verdict{SecretLane: false, Category: secretlane.CategoryNone, Reason: "qwen"}, nil
		}))
		cloud := reader.CloudModelFunc(func(ctx context.Context, jobType string, raw []byte, traceID string) (string, error) {
			*cloudCalls++
			return ingestRouteAnswer, nil
		})
		lane := memlane.Classifier{Store: st.Queries, MemoryDir: srv.mcpMemoryDir, Matcher: ritual.PolicyGlobMatcher{}}
		srv.SetIngester(&ingest.Runner{
			Reader:  reader.NewRunner(classifier, nil, cloud, nil, st),
			Memory:  srv.MemoryNoteWriter(),
			Extract: &extract.Runner{Store: st.Queries, Engine: factengine.New(st.Queries, taint.New(st.Queries, st), st), Lane: lane, Ledger: st},
			Lane:    lane, Store: st.Queries, Ledger: st,
			Cfg: ingest.Config{MaxBytes: maxBytes, Location: time.UTC},
		})
	})
	return f, cloudCalls
}

func postIngest(t *testing.T, client *http.Client, body string) (int, map[string]any) {
	t.Helper()
	resp, err := client.Post("http://kahyad/v1/ingest", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/ingest: %v", err)
	}
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

func ingestBody(doc, source string) string {
	b, _ := json.Marshal(map[string]any{"content_b64": base64.StdEncoding.EncodeToString([]byte(doc)), "source_ref": source})
	return string(b)
}

// TestIngestRouteHappyPath: POST /v1/ingest writes the external_doc note
// through the process-wide memory server and its facts through the
// extractor; the note's chunk stays out of the reflex <hafiza> block until
// `kahya memory confirm` (decision MEM-6.a + MEM-5), then it is in it.
func TestIngestRouteHappyPath(t *testing.T) {
	f, cloudCalls := newIngestFixture(t, 1<<20)
	status, res := postIngest(t, f.client, ingestBody(ingestRouteDoc, "not"))
	if status != http.StatusOK || res["ok"] != true || res["lane"] != "normal" || res["reader_lane"] != "normal" || res["source_tier"] != "external_doc" || res["facts"] != float64(1) || *cloudCalls != 1 {
		t.Fatalf("ingest = %d %v (cloud calls %d)", status, res, *cloudCalls)
	}
	file, _ := res["file"].(string)
	if !strings.HasPrefix(file, "ingest/") || !strings.HasSuffix(file, "-not.md") {
		t.Fatalf("file = %q", file)
	}
	note, err := os.ReadFile(filepath.Join(f.memoryDir, file))
	if err != nil || !strings.HasPrefix(string(note), "---\nkahya_source_tier: external_doc\n") || strings.Contains(string(note), "olarak başladı") {
		t.Fatalf("note = %q (%v) - want the external_doc tier and no raw sentence", note, err)
	}
	if n := countEventsOfKind(t, f.store, ingest.EventDone); n != 1 {
		t.Fatalf("ingest.done rows = %d", n)
	}
	if block := injectedBlock(t, f.client, "Ankara ofisinde"); strings.Contains(block, "Ankara ofisinde") {
		t.Fatalf("an unconfirmed external_doc note was injected: %q", block)
	}
	if status, res := postMemoryConfirm(t, f.client, `{"file":"`+file+`"}`); status != http.StatusOK || res["previous_tier"] != "external_doc" {
		t.Fatalf("confirm = %d %v", status, res)
	}
	if block := injectedBlock(t, f.client, "Ankara ofisinde"); !strings.Contains(block, "Zeynep Ankara ofisinde proje yöneticisi oldu.") {
		t.Fatalf("confirmed ingest note not injected: %q", block)
	}
	// The same document again is a 409 naming the first note.
	if status, res := postIngest(t, f.client, ingestBody(ingestRouteDoc, "not")); status != http.StatusConflict || res["reason"] != ingest.ReasonDuplicate || !strings.Contains(res["error"].(string), file) {
		t.Fatalf("duplicate = %d %v", status, res)
	}
}

// TestIngestRouteErrors: a document over the cap is a 413 carrying the
// Turkish text and the cap; a binary one a 400; bad base64 a 400; unwired a
// 503.
func TestIngestRouteErrors(t *testing.T) {
	f, cloudCalls := newIngestFixture(t, 32)
	status, res := postIngest(t, f.client, ingestBody(strings.Repeat("a", 33), "x"))
	if status != http.StatusRequestEntityTooLarge || res["max_bytes"] != float64(32) || res["error"] != "Dosya çok büyük (en fazla 32 bayt)." {
		t.Fatalf("too large = %d %v", status, res)
	}
	big, _ := json.Marshal(map[string]string{"content_b64": strings.Repeat("QUFB", 20000)})
	if status, res := postIngest(t, f.client, string(big)); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body = %d %v", status, res)
	}
	if status, res := postIngest(t, f.client, ingestBody("ikili\x00", "x")); status != http.StatusBadRequest || res["error"] != ingest.MsgIngestBinary {
		t.Fatalf("binary = %d %v", status, res)
	}
	if status, _ := postIngest(t, f.client, `{"content_b64":"%%%"}`); status != http.StatusBadRequest {
		t.Fatalf("bad base64 = %d", status)
	}
	if *cloudCalls != 0 {
		t.Fatal("a refused document reached the cloud")
	}
	client, _, _ := newEntityRouteFixture(t, false)
	if status, _ := postIngest(t, client, ingestBody("x", "x")); status != http.StatusServiceUnavailable {
		t.Fatalf("unwired = %d, want 503", status)
	}
}

// TestIngestRouteNotAnMCPTool: ingest is the user's decision - tools/list
// stays the five memory tools, and a live worker is refused on the route
// even with the control secret (G00 C-16); nothing is ingested.
func TestIngestRouteNotAnMCPTool(t *testing.T) {
	f, cloudCalls := newIngestFixture(t, 1<<20)
	resp := postMCP(t, f.client, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var parsed struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || len(parsed.Result.Tools) != 5 {
		t.Fatalf("tools/list = %s (%v), want the five memory tools", body, err)
	}
	for _, tool := range parsed.Result.Tools {
		if strings.Contains(tool.Name, "ingest") {
			t.Fatalf("tools/list exposes %q", tool.Name)
		}
	}
	reg := task.NewLiveRegistry()
	f.srv.SetTaskLiveRegistry(reg)
	reg.RegisterWorker("t-live", "trace-live", os.Getpid(), testProcessSID(t))
	worker := unixHTTPClientNoSecret(f.sock)
	status, msg := doRaw(t, worker, http.MethodPost, "/v1/ingest", "trace-ing-worker", readSecretFile(t, f.sock), ingestBody(ingestRouteDoc, "not"))
	if status != http.StatusForbidden || msg != MsgControlSecretRejected {
		t.Fatalf("live worker ingest = %d %q, want 403", status, msg)
	}
	if *cloudCalls != 0 || countEventsOfKind(t, f.store, ingest.EventDone) != 0 {
		t.Fatal("a worker's ingest ran")
	}
}

// TestIngestRouteSourcePathSecretGlobStaysLocal (FIX-MEM-6 review; HANDOFF
// §4 ordering, G00 H-7): POST /v1/ingest's source_paths reach the runner -
// a document read from a path secret_lane_globs covers is read by the local
// model only and lands under gizli/ingest/, though the classifier calls it
// normal and the label is a free one. A relative source path is a 400.
func TestIngestRouteSourcePathSecretGlobStaysLocal(t *testing.T) {
	cloudCalls, localCalls := new(int), new(int)
	f := newMCPTestFixtureWith(t, testPolicyDoc(), func(srv *Server, st *store.Store) {
		globs := []string{filepath.Join(srv.mcpMemoryDir, "gizli", "**"), "/Users/x/Documents/kimlik/**"}
		srv.SetMCPMemorySecretLane(memory.SecretLaneSeam{Dir: "gizli", Globs: globs, Matcher: ritual.PolicyGlobMatcher{}})
		classifier := secretlane.NewClassifier(secretlane.QwenClassifierFunc(func(ctx context.Context, text string) (secretlane.Verdict, error) {
			return secretlane.Verdict{SecretLane: false, Category: secretlane.CategoryNone, Reason: "qwen"}, nil
		}))
		local := ingestLocalModel(func(ctx context.Context, systemPrompt, raw string) (string, error) {
			*localCalls++
			return ingestRouteAnswer, nil
		})
		cloud := reader.CloudModelFunc(func(ctx context.Context, jobType string, raw []byte, traceID string) (string, error) {
			*cloudCalls++
			return ingestRouteAnswer, nil
		})
		lane := memlane.Classifier{Store: st.Queries, MemoryDir: srv.mcpMemoryDir, SecretGlobs: globs, Matcher: ritual.PolicyGlobMatcher{}}
		srv.SetIngester(&ingest.Runner{
			Reader:  reader.NewRunner(classifier, local, cloud, nil, st),
			Memory:  srv.MemoryNoteWriter(),
			Extract: &extract.Runner{Store: st.Queries, Engine: factengine.New(st.Queries, taint.New(st.Queries, st), st), Lane: lane, Ledger: st},
			Lane:    lane, Store: st.Queries, Ledger: st,
			Cfg: ingest.Config{MaxBytes: 1 << 20, Location: time.UTC},
		})
	})
	body := func(paths ...string) string {
		b, _ := json.Marshal(map[string]any{"content_b64": base64.StdEncoding.EncodeToString([]byte(ingestRouteDoc)), "source_ref": "not", "source_paths": paths})
		return string(b)
	}
	status, res := postIngest(t, f.client, body("/Users/x/Documents/kimlik/zeynep.txt"))
	if status != http.StatusOK || res["lane"] != "secret" || res["reader_lane"] != "secret" || !strings.HasPrefix(res["file"].(string), "gizli/ingest/") || *cloudCalls != 0 || *localCalls != 1 {
		t.Fatalf("ingest = %d %v (cloud %d, local %d)", status, res, *cloudCalls, *localCalls)
	}
	if status, res := postIngest(t, f.client, body("Documents/kimlik/zeynep.txt")); status != http.StatusBadRequest || res["reason"] != ingest.ReasonBadRequest {
		t.Fatalf("relative source path = %d %v", status, res)
	}
	if *cloudCalls != 0 {
		t.Fatal("a secret-path document reached the cloud")
	}
}

// ingestLocalModel adapts a func to reader.LocalModel.
type ingestLocalModel func(ctx context.Context, systemPrompt, raw string) (string, error)

func (l ingestLocalModel) Read(ctx context.Context, systemPrompt, raw string) (string, error) {
	return l(ctx, systemPrompt, raw)
}
