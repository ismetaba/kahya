package anthproxy

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// FIX-I-11: httputil.ReverseProxy closes the upstream body (it does not
// Read it to an error) when the worker's HTTP client goes away mid-stream.
// usageCapturingBody.Close must therefore record the usage observed so far,
// exactly once, with errClientDisconnected - and a later upstream EOF must
// never produce a second record.

// usageRecorder captures every onDone call a usageCapturingBody makes.
type usageRecorder struct {
	mu   sync.Mutex
	us   []Usage
	errs []error
}

func (r *usageRecorder) onDone(u Usage, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.us = append(r.us, u)
	r.errs = append(r.errs, err)
}

func (r *usageRecorder) snapshot() ([]Usage, []error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Usage(nil), r.us...), append([]error(nil), r.errs...)
}

const sseMessageStart1000 = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1000,\"cache_creation_input_tokens\":0,\"cache_read_input_tokens\":0}}}\n\n"

func TestUsageCapturingBodyCloseBeforeEOFRecordsOnce(t *testing.T) {
	pr, pw := io.Pipe()
	rec := &usageRecorder{}
	b := &usageCapturingBody{ReadCloser: pr, isSSE: true, onDone: rec.onDone}
	go func() {
		_, _ = pw.Write([]byte(sseMessageStart1000))
	}()
	buf := make([]byte, 4096)
	if _, err := b.Read(buf); err != nil {
		t.Fatalf("Read: %v", err)
	}
	_ = b.Close()                 // the client went away; ReverseProxy closes the body
	_ = pw.CloseWithError(io.EOF) // upstream finishes later - must NOT record a second time
	// A Read after Close (never done by ReverseProxy, but harmless) must not
	// record again either.
	_, _ = b.Read(buf)

	got, gotErr := rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("onDone calls = %d, want exactly 1 (Close before EOF must record the observed usage once)", len(got))
	}
	if got[0].InputTokens != 1000 {
		t.Errorf("InputTokens = %d, want 1000", got[0].InputTokens)
	}
	if !errors.Is(gotErr[0], errClientDisconnected) {
		t.Errorf("err = %v, want errClientDisconnected", gotErr[0])
	}
}

func TestUsageCapturingBodyCloseFlushesTrailingPartialSSELine(t *testing.T) {
	// The final usage-carrying "data:" line arrived WITHOUT its trailing
	// newline before the client went away: Close must still parse it.
	partial := "data: {\"type\":\"message_delta\",\"delta\":{},\"usage\":{\"output_tokens\":17}}"
	rec := &usageRecorder{}
	b := &usageCapturingBody{ReadCloser: io.NopCloser(strings.NewReader(sseMessageStart1000 + partial)), isSSE: true, onDone: rec.onDone}
	buf := make([]byte, len(sseMessageStart1000)+len(partial))
	if _, err := io.ReadFull(b, buf); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if got, _ := rec.snapshot(); len(got) != 0 {
		t.Fatalf("onDone fired before EOF/Close (%d calls)", len(got))
	}
	_ = b.Close()
	got, gotErr := rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("onDone calls = %d, want 1", len(got))
	}
	if got[0].InputTokens != 1000 || got[0].OutputTokens != 17 {
		t.Errorf("usage = %+v, want input 1000 / output 17 (the trailing partial line must be fed on Close)", got[0])
	}
	if !errors.Is(gotErr[0], errClientDisconnected) {
		t.Errorf("err = %v, want errClientDisconnected", gotErr[0])
	}
}

func TestUsageCapturingBodyCloseAfterEOFDoesNotDoubleRecord(t *testing.T) {
	events := sseMessageStart1000 + "data: {\"type\":\"message_delta\",\"delta\":{},\"usage\":{\"output_tokens\":5}}\n\n"
	rec := &usageRecorder{}
	b := &usageCapturingBody{ReadCloser: io.NopCloser(strings.NewReader(events)), isSSE: true, onDone: rec.onDone}
	if _, err := io.Copy(io.Discard, b); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if got, _ := rec.snapshot(); len(got) != 1 {
		t.Fatalf("onDone calls after EOF = %d, want 1", len(got))
	}
	_ = b.Close() // ReverseProxy always closes after a clean copy too
	got, gotErr := rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("onDone calls after EOF+Close = %d, want still 1 (Close after EOF must not record again)", len(got))
	}
	if !errors.Is(gotErr[0], io.EOF) {
		t.Errorf("err = %v, want io.EOF (the clean-EOF record, not a disconnect)", gotErr[0])
	}
	if got[0].InputTokens != 1000 || got[0].OutputTokens != 5 {
		t.Errorf("usage = %+v, want input 1000 / output 5", got[0])
	}
}

func TestUsageCapturingBodyNonStreamCloseRecordsParsedUsage(t *testing.T) {
	body := `{"usage":{"input_tokens":7,"output_tokens":3,"cache_creation_input_tokens":2,"cache_read_input_tokens":1}}`
	rec := &usageRecorder{}
	b := &usageCapturingBody{ReadCloser: io.NopCloser(strings.NewReader(body)), isSSE: false, onDone: rec.onDone}
	buf := make([]byte, len(body))
	// Read exactly the body's bytes: EOF is never observed.
	if _, err := io.ReadFull(b, buf); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if got, _ := rec.snapshot(); len(got) != 0 {
		t.Fatalf("onDone fired before EOF/Close (%d calls)", len(got))
	}
	_ = b.Close()
	got, gotErr := rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("onDone calls = %d, want 1", len(got))
	}
	want := Usage{InputTokens: 7, OutputTokens: 3, CacheCreationInputTokens: 2, CacheReadInputTokens: 1}
	if got[0] != want {
		t.Errorf("usage = %+v, want %+v", got[0], want)
	}
	if !errors.Is(gotErr[0], errClientDisconnected) {
		t.Errorf("err = %v, want errClientDisconnected", gotErr[0])
	}
}

// TestProxyRecordsUsageWhenClientDisconnectsMidStream is the end-to-end
// shape: the upstream streams message_start, then holds the stream open;
// the worker's client reads that first event and hangs up. The call must
// still be ledgered once, with the observed input tokens and
// status "aborted".
func TestProxyRecordsUsageWhenClientDisconnectsMidStream(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseAll)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		_, _ = fmt.Fprint(w, sseMessageStart1000)
		flusher.Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = fmt.Fprint(w, "data: {\"type\":\"message_delta\",\"delta\":{},\"usage\":{\"output_tokens\":9}}\n\n")
		flusher.Flush()
	}))
	t.Cleanup(upstream.Close)

	ledger := &fakeLedger{}
	cfg := testProxyConfig(t, upstream.URL, generousGovernor(), ledger, nil)
	baseURL, _ := startTestProxy(t, cfg)

	// A private transport so closing the body really drops the connection
	// (no keep-alive pool shared with other tests).
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Do(newAuthedRequest(t, "POST", baseURL+"/v1/messages", `{"model":"claude-sonnet-5-5","stream":true}`))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	rd := bufio.NewReader(resp.Body)
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			resp.Body.Close()
			t.Fatalf("reading message_start: %v", err)
		}
		if strings.HasPrefix(line, "data: ") {
			break
		}
	}
	// The worker goes away mid-stream.
	_ = resp.Body.Close()
	client.CloseIdleConnections()
	time.Sleep(50 * time.Millisecond)
	releaseAll()

	if !waitForCondition(t, 3*time.Second, func() bool { return ledger.countKind(EventModelCall) == 1 }) {
		t.Fatalf("model_call rows = %d, want 1 (a client that disconnects mid-stream must still be billed)", ledger.countKind(EventModelCall))
	}
	// Give a (buggy) second record a chance to appear.
	time.Sleep(100 * time.Millisecond)
	if n := ledger.countKind(EventModelCall); n != 1 {
		t.Fatalf("model_call rows = %d, want exactly 1", n)
	}
	ledger.mu.Lock()
	var payload map[string]any
	for _, c := range ledger.calls {
		if c.kind == EventModelCall {
			payload = c.payload
		}
	}
	ledger.mu.Unlock()
	if payload["input_tokens"] != int64(1000) {
		t.Errorf("model_call input_tokens = %v, want 1000", payload["input_tokens"])
	}
	if payload["status"] != "aborted" {
		t.Errorf("model_call status = %v, want aborted", payload["status"])
	}
}

// failAfterWriter is an http.ResponseWriter whose Write fails once `limit`
// bytes have been accepted - a worker whose connection broke mid-stream,
// observed exactly where httputil.ReverseProxy observes it (a failed client
// write), with no inbound-context cancellation involved.
type failAfterWriter struct {
	mu      sync.Mutex
	header  http.Header
	status  int
	written int
	limit   int
}

func (w *failAfterWriter) Header() http.Header { return w.header }
func (w *failAfterWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.status = code
}
func (w *failAfterWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.written >= w.limit {
		return 0, errors.New("write: broken pipe (test)")
	}
	w.written += len(p)
	return len(p), nil
}

// TestProxyRecordsUsageWhenClientWriteFailsMidStream drives the real
// httputil.ReverseProxy copy loop into its "client write failed" exit: it
// closes the upstream body without ever Reading it to an error, so only
// usageCapturingBody.Close can record the call (red before FIX-I-11: zero
// model_call rows).
func TestProxyRecordsUsageWhenClientWriteFailsMidStream(t *testing.T) {
	upstream := newSSEUpstream(t, []string{
		strings.TrimSuffix(sseMessageStart1000, "\n"),
		"data: {\"type\":\"message_delta\",\"delta\":{},\"usage\":{\"output_tokens\":9}}\n",
		"data: {\"type\":\"message_stop\"}\n",
	}, 40*time.Millisecond)
	ledger := &fakeLedger{}
	p, err := New(testProxyConfig(t, upstream.srv.URL, generousGovernor(), ledger, nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://proxy.invalid/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5-5","stream":true}`))
	req.Header.Set(TaskTokenHeader, testToken)
	req.Header.Set("Authorization", "Bearer test-upstream-credential")
	w := &failAfterWriter{header: http.Header{}, limit: len(sseMessageStart1000)}

	p.ServeHTTP(w, req) // returns once ReverseProxy gives up on the client

	if n := ledger.countKind(EventModelCall); n != 1 {
		t.Fatalf("model_call rows = %d, want exactly 1 (ReverseProxy closed the body after a failed client write)", n)
	}
	ledger.mu.Lock()
	var payload map[string]any
	for _, c := range ledger.calls {
		if c.kind == EventModelCall {
			payload = c.payload
		}
	}
	ledger.mu.Unlock()
	if payload["input_tokens"] != int64(1000) {
		t.Errorf("model_call input_tokens = %v, want 1000", payload["input_tokens"])
	}
	if payload["status"] != "aborted" {
		t.Errorf("model_call status = %v, want aborted", payload["status"])
	}
}
