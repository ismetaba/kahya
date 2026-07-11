package secretlane

import (
	"context"
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
