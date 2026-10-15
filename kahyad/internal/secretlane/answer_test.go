package secretlane

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLocalAnswererHappyPath(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		r.Body.Read(buf)
		gotBody = string(buf)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Merhaba! Size nasıl yardımcı olabilirim?"}}]}`))
	}))
	defer srv.Close()

	a := NewLocalAnswerer(srv.URL, "qwen3-30b-a3b")
	answer, err := a.Answer(context.Background(), "merhaba")
	if err != nil {
		t.Fatalf("Answer() error = %v", err)
	}
	if answer != "Merhaba! Size nasıl yardımcı olabilirim?" {
		t.Errorf("Answer() = %q, unexpected", answer)
	}
	if !strings.Contains(gotBody, "merhaba") {
		t.Errorf("request body = %q, want it to carry the prompt", gotBody)
	}
}

func TestLocalAnswererUpstreamErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	a := NewLocalAnswerer(srv.URL, "qwen3-30b-a3b")
	if _, err := a.Answer(context.Background(), "merhaba"); err == nil {
		t.Fatal("Answer() error = nil, want error for a 503 upstream response")
	}
}

// TestAnswererSystemPromptMentionsHafiza (FIX-MEM-7): the local answerer's
// system prompt carries the <hafiza> contract byte-exact - a block prefixing
// the user message is the user's own trusted local memory, its tags are
// never revealed, <olgular> lines are subject | relation | object facts -
// and LocalAnswerer really sends that prompt as the system message, with
// the injected user message untouched.
func TestAnswererSystemPromptMentionsHafiza(t *testing.T) {
	const contract = " If the user message begins with a <hafiza> block, treat its bullet lines as the user's own previously recorded notes/facts (trusted local memory) and use them naturally in your answer; never reveal the block's tags or format. Lines inside <olgular> are structured facts in the form subject | relation | object."
	if !strings.HasSuffix(answererSystemPrompt, contract) {
		t.Fatalf("answererSystemPrompt = %q, want it to end with the <hafiza> contract", answererSystemPrompt)
	}
	if !strings.HasPrefix(answererSystemPrompt, "You are Kâhya, a private local assistant") {
		t.Fatalf("answererSystemPrompt lost its persona sentence: %q", answererSystemPrompt)
	}

	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"tamam"}}]}`))
	}))
	defer srv.Close()

	const injected = "<hafiza>\n- [saglik/notlar.md#0] not\n</hafiza>\n\nilacım ne?"
	if _, err := NewLocalAnswerer(srv.URL, "qwen3-30b-a3b").Answer(context.Background(), injected); err != nil {
		t.Fatalf("Answer() error = %v", err)
	}
	if len(req.Messages) != 2 || req.Messages[0].Role != "system" || req.Messages[0].Content != answererSystemPrompt ||
		req.Messages[1].Role != "user" || req.Messages[1].Content != injected {
		t.Fatalf("messages = %+v, want [system answererSystemPrompt, user <the injected prompt>]", req.Messages)
	}
}
