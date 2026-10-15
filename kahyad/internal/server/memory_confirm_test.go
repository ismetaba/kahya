package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kahya/kahyad/internal/task"
	"kahya/mcp/memory"
)

// writeAgentNote writes an agent_derived note through the process-wide
// memory server (the same HandleWrite the memory_write tool runs below the
// policy gate).
func writeAgentNote(t *testing.T, f mcpTestFixture, rel, content string) {
	t.Helper()
	if _, err := f.srv.memoryServer().HandleWrite(context.Background(), "tr-write", memory.MemoryWriteArgs{File: rel, Content: content}); err != nil {
		t.Fatalf("HandleWrite: %v", err)
	}
}

func postMemoryConfirm(t *testing.T, client *http.Client, body string) (int, map[string]any) {
	t.Helper()
	resp, err := client.Post("http://kahyad/v1/memory/confirm", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/memory/confirm: %v", err)
	}
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

// injectedBlock runs a for_injection:true search and returns the block.
func injectedBlock(t *testing.T, client *http.Client, query string) string {
	t.Helper()
	resp := postMemorySearch(t, client, `{"query":"`+query+`","for_injection":true,"task_id":"t1"}`)
	defer resp.Body.Close()
	var body struct {
		HafizaBlock string `json:"hafiza_block"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body.HafizaBlock
}

// TestMemorySearchForInjectionIncludesConfirmedNote: end to end over a real
// store, indexer and searcher - a memory_write note (agent_derived) stays
// out of the reflex-injection block; after `kahya memory confirm` (POST
// /v1/memory/confirm) the same note's chunk is in it.
func TestMemorySearchForInjectionIncludesConfirmedNote(t *testing.T) {
	f := newMCPTestFixture(t)
	writeAgentNote(t, f, "inbox/2026-10-08.md", "Zeynep pazartesi Ankara ofisine taşınıyor.")
	if block := injectedBlock(t, f.client, "Ankara ofisine"); strings.Contains(block, "Ankara ofisine") {
		t.Fatalf("agent_derived note injected before confirmation: %q", block)
	}

	status, res := postMemoryConfirm(t, f.client, `{"file":"inbox/2026-10-08.md"}`)
	if status != http.StatusOK || res["ok"] != true || res["previous_tier"] != "agent_derived" || res["commit_sha"] == "" {
		t.Fatalf("confirm = %d %v", status, res)
	}
	if block := injectedBlock(t, f.client, "Ankara ofisine"); !strings.Contains(block, "Zeynep pazartesi Ankara ofisine") {
		t.Fatalf("confirmed note not injected: %q", block)
	}
	var tier string
	if err := f.store.DB().QueryRow(`SELECT source_tier FROM episodes WHERE source_path = 'inbox/2026-10-08.md'`).Scan(&tier); err != nil || tier != "user_asserted" {
		t.Fatalf("episode tier = %q (%v), want user_asserted", tier, err)
	}
	if n := countEventsOfKind(t, f.store, memory.EventMemoryConfirm); n != 1 {
		t.Fatalf("memory_confirm rows = %d, want 1", n)
	}
	// An absolute path inside the memory dir is accepted (the CLI user may
	// paste one); a second confirm is a 409, outside paths a 400, a
	// missing note a 404.
	abs := filepath.Join(f.memoryDir, "inbox", "2026-10-08.md")
	if status, res := postMemoryConfirm(t, f.client, `{"file":"`+abs+`"}`); status != http.StatusConflict || !strings.Contains(res["error"].(string), "memory_confirm: not zaten onaylı (user_asserted)") {
		t.Fatalf("second confirm = %d %v, want 409", status, res)
	}
	for body, want := range map[string]int{
		`{"file":"../x.md"}`:    http.StatusBadRequest,
		`{"file":"/etc/hosts"}`: http.StatusBadRequest,
		`{"file":""}`:           http.StatusBadRequest,
		`{"file":"yok.md"}`:     http.StatusNotFound,
		`{`:                     http.StatusBadRequest,
	} {
		if status, res := postMemoryConfirm(t, f.client, body); status != want {
			t.Errorf("%s = %d %v, want %d", body, status, res, want)
		}
	}
}

// TestMCPToolsListDoesNotExposeMemoryConfirm: the human gate is never a
// model tool - tools/list stays the five memory tools, none of them a
// confirm, and calling "memory_confirm" over /v1/mcp is an unknown tool.
func TestMCPToolsListDoesNotExposeMemoryConfirm(t *testing.T) {
	f := newMCPTestFixture(t)
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
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Result.Tools) != 5 {
		t.Fatalf("tools = %+v, want 5", parsed.Result.Tools)
	}
	for _, tool := range parsed.Result.Tools {
		if strings.Contains(tool.Name, "confirm") {
			t.Fatalf("tools/list exposes %q", tool.Name)
		}
	}
	writeAgentNote(t, f, "inbox/x.md", "ajan notu")
	resp = postMCP(t, f.client, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"memory_confirm","arguments":{"file":"inbox/x.md"}}}`)
	resp.Body.Close()
	raw, _ := os.ReadFile(filepath.Join(f.memoryDir, "inbox", "x.md"))
	if !strings.Contains(string(raw), "kahya_source_tier: agent_derived") {
		t.Fatalf("a tools/call memory_confirm changed the note: %q", raw)
	}
}

// TestMemoryConfirmRouteRefusesLiveWorker (G00 C-16): a caller inside a
// live worker's session is refused on /v1/memory/confirm even with the
// control secret, and the note keeps its agent_derived tier.
func TestMemoryConfirmRouteRefusesLiveWorker(t *testing.T) {
	f := newMCPTestFixture(t)
	writeAgentNote(t, f, "inbox/x.md", "ajan notu")
	reg := task.NewLiveRegistry()
	f.srv.SetTaskLiveRegistry(reg)
	reg.RegisterWorker("t-live", "trace-live", os.Getpid(), testProcessSID(t))
	worker := unixHTTPClientNoSecret(f.sock)
	status, body := doRaw(t, worker, http.MethodPost, "/v1/memory/confirm", "trace-mc-worker", readSecretFile(t, f.sock), `{"file":"inbox/x.md"}`)
	if status != http.StatusForbidden || body != MsgControlSecretRejected {
		t.Fatalf("live worker confirm = %d %q, want 403", status, body)
	}
	raw, _ := os.ReadFile(filepath.Join(f.memoryDir, "inbox", "x.md"))
	if !strings.Contains(string(raw), "kahya_source_tier: agent_derived") {
		t.Fatalf("note promoted by a worker: %q", raw)
	}
}

// TestMemoryConfirmRouteUnwiredIs503: no memory dir/indexer wired = 503.
func TestMemoryConfirmRouteUnwiredIs503(t *testing.T) {
	client, _, _ := newEntityRouteFixture(t, false)
	if status, _ := postMemoryConfirm(t, client, `{"file":"inbox/x.md"}`); status != http.StatusServiceUnavailable {
		t.Fatalf("unwired confirm = %d, want 503", status)
	}
}
