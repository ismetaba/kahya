// Package anthproxy is kahyad's Anthropic forward-proxy + cost governor
// (HANDOFF §4 IPC ⚑, verbatim): "kahyad localhost'ta auth-header ekleyen
// bir forward-proxy dinler ve worker'ı ANTHROPIC_BASE_URL=http://127.0.0.1:
// <port> ile spawn eder. Maliyet valisi, cache-hit metriği ve model-
// çağrısı egress kapısı bu proxy noktasında uygulanır." kahyad opens one
// ephemeral 127.0.0.1 listener per task (Proxy.Start), spawns the worker
// with ANTHROPIC_BASE_URL pointed at it, and closes the listener when the
// task ends (Proxy.Close) — see kahyad/internal/server's wiring into
// kahyad/internal/spawn (W12-07).
//
// # OWNER AUTH DECISION — HANDOFF deviation (documented per this task's
// explicit instructions; also noted in docs/ipc.md)
//
// HANDOFF §4 assumes kahyad reads a real Anthropic API key from the
// macOS Keychain and injects it as every request's credential. The owner
// decided NOT to provision a separate Anthropic API key for this project:
// the worker (claude-agent-sdk) instead authenticates through its own,
// already-logged-in Claude Code SDK session. Consequence: this package
// implements BOTH modes behind one small interface, CredentialSource,
// selected by config.CredentialMode:
//
//   - "keychain" (config.CredentialModeKeychain) — the original HANDOFF
//     design, fully implemented and tested as a valid fallback: strip
//     every inbound auth header, inject the real key read from
//     kahyad/internal/secrets.Keychain (never logged) as "x-api-key".
//   - "passthrough" (config.CredentialModePassthrough) — the OWNER-DECISION
//     DEFAULT: after validating the inbound per-task local token (the
//     ONLY thing that ever proves this request came from this task's own
//     spawned worker — HANDOFF §4 IPC ⚑, "API anahtarı worker'a verilmez"
//     is still upheld: no REAL Anthropic credential ever reaches the
//     worker either way), strip only the header that carried that local
//     token and forward any OTHER auth header the worker's own HTTP
//     client attached completely unchanged — this is the worker's Claude
//     Code SDK session credential, which this proxy never inspects,
//     replaces, or logs.
//
// Every other piece this task specifies — the cost governor, the cache-hit
// metric, the egress-gate hook, the usage/pricing machinery — is built
// exactly per the task file, auth-agnostic: none of it depends on which
// CredentialSource mode is active.
package anthproxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"kahya/kahyad/internal/notify"
)

// CredentialMode values (mirrors kahyad/internal/config's
// CredentialModeKeychain/CredentialModePassthrough literals — this
// package intentionally does not import kahyad/internal/config so it has
// no dependency on config.Config; keep the string values in sync by hand).
const (
	CredentialModeKeychain    = "keychain"
	CredentialModePassthrough = "passthrough"
)

// keyOverrideEnvVar is KAHYA_ANTHROPIC_KEY_OVERRIDE (task spec step 1):
// dev-only substitute for a real Keychain read, required by the hermetic
// test gate (mock upstream, no Keychain on CI). Ignored — loudly — outside
// cfg.Env=="dev".
const keyOverrideEnvVar = "KAHYA_ANTHROPIC_KEY_OVERRIDE"

// devEnv mirrors kahyad/internal/config.EnvDev's literal value ("dev") —
// see the CredentialMode constants' doc comment for why this package
// duplicates the literal instead of importing config.
const devEnv = "dev"

// CredentialSource resolves the upstream credential the proxy injects in
// keychain mode (see this file's package doc for the full OWNER AUTH
// DECISION rationale for why a second, no-op implementation exists).
type CredentialSource interface {
	// UpstreamAuth returns the header name/value pair to inject on the
	// outbound request (e.g. "x-api-key", "sk-ant-..."). Returning
	// ("", "", nil) is the passthroughSource sentinel meaning "inject
	// nothing — forward whatever auth header the worker's own HTTP
	// client already attached, unchanged". The proxy's passthrough
	// Director branch never actually calls this method (it has nothing
	// to inject), but passthroughSource still implements it so
	// credential_mode=passthrough has a concrete CredentialSource value
	// to hold, keeping the wiring shape uniform across both modes.
	UpstreamAuth(ctx context.Context) (headerName string, headerValue string, err error)
}

// KeychainReader is the narrow read dependency keychainSource needs
// (kahyad/internal/secrets.Keychain.Read already has exactly this shape).
type KeychainReader interface {
	Read() (string, error)
}

// keychainSource is the HANDOFF-original credential source.
type keychainSource struct {
	kc  KeychainReader
	env string
	// warnOverrideIgnored is called (best-effort) whenever
	// KAHYA_ANTHROPIC_KEY_OVERRIDE is set but env != "dev" — wired to a
	// JSONL event=key_override_ignored warn line by the caller.
	warnOverrideIgnored func()
}

// NewKeychainCredentialSource constructs the production keychain-mode
// CredentialSource. env is cfg.Env ("dev"|"prod") — the override env var
// only ever substitutes when env=="dev" (task spec step 1); any other
// value ignores it with warnOverrideIgnored (may be nil).
func NewKeychainCredentialSource(kc KeychainReader, env string, warnOverrideIgnored func()) CredentialSource {
	return &keychainSource{kc: kc, env: env, warnOverrideIgnored: warnOverrideIgnored}
}

func (s *keychainSource) UpstreamAuth(context.Context) (string, string, error) {
	if override := os.Getenv(keyOverrideEnvVar); override != "" {
		if s.env == devEnv {
			return "x-api-key", override, nil
		}
		if s.warnOverrideIgnored != nil {
			s.warnOverrideIgnored()
		}
		// Fall through to the real Keychain read in prod — the override
		// is IGNORED, never silently trusted, outside cfg.Env=="dev".
	}
	key, err := s.kc.Read()
	if err != nil {
		return "", "", fmt.Errorf("anthproxy: keychain unavailable: %w", err)
	}
	return "x-api-key", key, nil
}

// passthroughSource is the OWNER-DECISION DEFAULT (see package doc).
type passthroughSource struct{}

// NewPassthroughCredentialSource constructs the passthrough-mode
// CredentialSource.
func NewPassthroughCredentialSource() CredentialSource { return &passthroughSource{} }

func (passthroughSource) UpstreamAuth(context.Context) (string, string, error) {
	return "", "", nil
}

// ProxyConfig bundles everything one per-task Proxy needs.
type ProxyConfig struct {
	TaskID      string
	TraceID     string
	Token       string // this task's kahya-task-<hex32> local auth token
	UpstreamURL string // cfg.anthropic_upstream_url

	CredentialMode string // CredentialModeKeychain | CredentialModePassthrough; defaults to passthrough
	Credential     CredentialSource

	Governor    *Governor
	Notifier    notify.Notifier
	EventLedger EventLedger

	// PauseBudget is the task-row callback (task spec step 3: "task state
	// update via a callback into spawn/store, keeping anthproxy
	// store-agnostic"). May be nil (best-effort).
	PauseBudget func(ctx context.Context, taskID string) error

	// EgressGate is the named hook step "egressGate(req) error returns
	// nil for now (W3-05 fills it)". May be nil, treated as always-allow.
	EgressGate func(req *http.Request) error

	// Now defaults to time.Now; tests inject a fixed/controllable clock.
	Now func() time.Time
}

// Proxy is kahyad's per-task forward-proxy listener.
type Proxy struct {
	cfg ProxyConfig
	ln  net.Listener
	srv *http.Server
	rp  *httputil.ReverseProxy

	keychainWarnedOnce sync.Once

	// requestCount is a test-only observability hook (W4-03's no-cloud-
	// fallback regression test - kahyad/internal/reader's own tests spin up
	// a REAL Proxy and assert this stays 0 for a secret-lane Reader job, or
	// a secret-lane job with the local model unavailable, proving
	// structurally that no byte from that job ever reached this - or any -
	// forward-proxy instance, not merely that a fake CloudModel double was
	// never called). Incremented at the very top of ServeHTTP, before
	// auth/budget/anything else runs, so it counts every inbound request
	// this Proxy instance EVER received, regardless of outcome.
	requestCount int64
}

// RequestCount returns the number of inbound HTTP requests this Proxy
// instance has received so far. See the Proxy.requestCount field's own
// doc comment.
func (p *Proxy) RequestCount() int64 {
	return atomic.LoadInt64(&p.requestCount)
}

// New constructs a Proxy for one task. Call Start to bind the ephemeral
// listener and begin serving.
func New(cfg ProxyConfig) (*Proxy, error) {
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, fmt.Errorf("anthproxy: Token must not be empty")
	}
	if strings.TrimSpace(cfg.UpstreamURL) == "" {
		return nil, fmt.Errorf("anthproxy: UpstreamURL must not be empty")
	}
	if cfg.Governor == nil {
		return nil, fmt.Errorf("anthproxy: Governor must not be nil")
	}
	if cfg.CredentialMode == "" {
		cfg.CredentialMode = CredentialModePassthrough
	}
	if cfg.CredentialMode == CredentialModeKeychain && cfg.Credential == nil {
		return nil, fmt.Errorf("anthproxy: Credential must not be nil in keychain mode")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}

	upstream, err := url.Parse(cfg.UpstreamURL)
	if err != nil {
		return nil, fmt.Errorf("anthproxy: parse upstream_url %q: %w", cfg.UpstreamURL, err)
	}

	p := &Proxy{cfg: cfg}

	p.rp = &httputil.ReverseProxy{
		// SSE must flow token-by-token, never buffered until the response
		// ends (task spec step 1) — a negative FlushInterval flushes the
		// client connection after every single Read from the upstream
		// body.
		FlushInterval: -1,
		Director: func(req *http.Request) {
			req.URL.Scheme = upstream.Scheme
			req.URL.Host = upstream.Host
			req.Host = upstream.Host
			switch p.cfg.CredentialMode {
			case CredentialModeKeychain:
				req.Header.Del("x-api-key")
				req.Header.Del("Authorization")
				if cred, ok := req.Context().Value(credentialCtxKey{}).(injectedCredential); ok && cred.name != "" {
					req.Header.Set(cred.name, cred.value)
				}
			default: // CredentialModePassthrough
				stripTokenHeader(req.Header, p.cfg.Token)
			}
		},
		ModifyResponse: p.wrapResponseBody,
		ErrorHandler:   handleUpstreamError,
	}
	return p, nil
}

// Start binds an ephemeral 127.0.0.1 listener and begins serving in the
// background; it returns the base URL to set as ANTHROPIC_BASE_URL (task
// spec: "kahyad opens a per-task ephemeral listener (127.0.0.1:0)").
func (p *Proxy) Start() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("anthproxy: listen: %w", err)
	}
	p.ln = ln
	p.srv = &http.Server{Handler: p}
	go func() {
		_ = p.srv.Serve(ln)
	}()
	return "http://" + ln.Addr().String(), nil
}

// Close stops the listener (task spec: "close it after worker exit" /
// step 6: "per-task listener closes on task end (connection refused
// after)").
func (p *Proxy) Close() error {
	if p.srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return p.srv.Shutdown(ctx)
}

// reqDataCtxKey/credentialCtxKey stash per-request data ServeHTTP computes
// before delegating to p.rp, retrieved later by Director/ModifyResponse -
// httputil.ReverseProxy clones the incoming request's context, so a value
// set here is visible in both.
type reqDataCtxKey struct{}
type credentialCtxKey struct{}

type injectedCredential struct{ name, value string }

type reqData struct {
	Model       string
	SystemHash  string
	Start       time.Time
	Reservation ReservationID
}

const proxyRequestMaxBytes = 16 << 20 // 16 MiB: generous for a /v1/messages body

// ServeHTTP implements the full per-request pipeline (task spec steps
// 1/3/4, reordered by BLOCKER 2): local-auth check -> egress gate -> read
// body -> governor check-and-RESERVE (needs the body/model to size its
// conservative estimate - see Governor.CheckBeforeForward) -> (in keychain
// mode) credential resolution -> delegate to the reverse proxy, which
// streams the response back through wrapResponseBody's usage capture.
//
// BLOCKER 2: once CheckBeforeForward grants a reservation, this handler
// unconditionally defers releasing it - every return path below the check
// (keychain failure, or the reverse-proxy's own ErrorHandler firing before
// ModifyResponse ever runs on a failed upstream RoundTrip) would otherwise
// leak that reservation forever, permanently over-counting against every
// later request's ceiling/budget check. The success path's own
// RecordUsage call (wrapResponseBody's onDone, invoked synchronously
// during p.rp.ServeHTTP below - ReverseProxy blocks until the response
// body is fully copied) already releases the SAME reservation before this
// defer ever runs; ReleaseReservation is idempotent, so this is always
// safe, never a double-subtraction.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt64(&p.requestCount, 1)
	ctx := r.Context()

	if !p.checkLocalAuth(r) {
		p.ledgerAuthReject(ctx, r)
		writeAnthropicError(w, http.StatusUnauthorized, "authentication_error",
			"Yerel yetkilendirme jetonu eksik veya gecersiz.")
		return
	}

	if p.cfg.EgressGate != nil {
		if err := p.cfg.EgressGate(r); err != nil {
			writeAnthropicError(w, http.StatusForbidden, "permission_error", err.Error())
			return
		}
	}

	bodyBytes, err := readAndRestoreBody(r)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "Istek govdesi okunamadi.")
		return
	}
	model, systemHash := probeRequest(bodyBytes)

	check := p.cfg.Governor.CheckBeforeForward(p.cfg.TaskID, model, bodyBytes)
	if !check.Allowed {
		p.onBudgetBlocked(ctx, check.Message)
		writeAnthropicError(w, http.StatusServiceUnavailable, "api_error", check.Message)
		return
	}
	defer p.cfg.Governor.ReleaseReservation(check.Reservation)

	if p.cfg.CredentialMode == CredentialModeKeychain {
		headerName, headerValue, credErr := p.cfg.Credential.UpstreamAuth(ctx)
		if credErr != nil || headerName == "" {
			p.onKeychainUnavailable(ctx)
			writeAnthropicError(w, http.StatusServiceUnavailable, "api_error", MsgKeychainUnavailable)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), credentialCtxKey{}, injectedCredential{headerName, headerValue}))
	}

	r = r.WithContext(context.WithValue(r.Context(), reqDataCtxKey{}, &reqData{
		Model: model, SystemHash: systemHash, Start: p.cfg.Now(), Reservation: check.Reservation,
	}))

	p.rp.ServeHTTP(w, r)
}

// checkLocalAuth implements the task spec's "Reject inbound whose
// x-api-key/authorization != this task's kahya-task-<hex32> token" — the
// local token proves this request came from this task's own spawned
// worker (whichever header the worker's HTTP client happened to carry it
// in); it does not, on its own, decide what (if anything) is forwarded
// upstream — applyCredential/Director does that per credential_mode.
func (p *Proxy) checkLocalAuth(r *http.Request) bool {
	token := p.cfg.Token
	if v := r.Header.Get("x-api-key"); v != "" {
		return v == token
	}
	if v := r.Header.Get("Authorization"); v != "" {
		return v == token || v == "Bearer "+token
	}
	return false
}

// stripTokenHeader removes exactly the header that carried the local
// per-task token (passthrough mode, task spec: "forward the worker's own
// upstream auth header unchanged" — the local token is NOT a real
// Anthropic credential and must never reach the real upstream; any OTHER
// auth header is left completely untouched).
func stripTokenHeader(h http.Header, token string) {
	if h.Get("x-api-key") == token {
		h.Del("x-api-key")
	}
	if auth := h.Get("Authorization"); auth == token || auth == "Bearer "+token {
		h.Del("Authorization")
	}
}

func (p *Proxy) ledgerAuthReject(ctx context.Context, r *http.Request) {
	if p.cfg.EventLedger == nil {
		return
	}
	_ = p.cfg.EventLedger.LogEvent(ctx, p.cfg.TraceID, EventProxyAuthReject, map[string]any{
		"task_id":     p.cfg.TaskID,
		"remote_addr": r.RemoteAddr,
	})
}

func (p *Proxy) onBudgetBlocked(ctx context.Context, message string) {
	if p.cfg.PauseBudget != nil {
		_ = p.cfg.PauseBudget(ctx, p.cfg.TaskID)
	}
	if p.cfg.EventLedger != nil {
		_ = p.cfg.EventLedger.LogEvent(ctx, p.cfg.TraceID, EventTaskPausedBudget, map[string]any{
			"task_id": p.cfg.TaskID, "message": message,
		})
	}
	if p.cfg.Notifier != nil {
		_ = p.cfg.Notifier.Alarm(ctx, p.cfg.TraceID, EventTaskPausedBudget, message, map[string]any{"task_id": p.cfg.TaskID})
	}
}

// onKeychainUnavailable notifies exactly once per Proxy (task spec step 1:
// "notify once (event=keychain_unavailable), keep serving locals" — the
// "keep serving locals" half is inherent: this failure only ever affects
// THIS task's cloud-lane requests, never anything else running in kahyad).
func (p *Proxy) onKeychainUnavailable(ctx context.Context) {
	p.keychainWarnedOnce.Do(func() {
		if p.cfg.Notifier != nil {
			_ = p.cfg.Notifier.Notify(ctx, p.cfg.TraceID, EventKeychainUnavailable, MsgKeychainUnavailable, map[string]any{"task_id": p.cfg.TaskID})
			return
		}
		if p.cfg.EventLedger != nil {
			_ = p.cfg.EventLedger.LogEvent(ctx, p.cfg.TraceID, EventKeychainUnavailable, map[string]any{
				"task_id": p.cfg.TaskID, "message": MsgKeychainUnavailable,
			})
		}
	})
}

// readAndRestoreBody reads r.Body fully (capped, so an oversized body
// can't tie up the proxy) and resets r.Body to a fresh reader over the
// same bytes so the ORIGINAL, byte-exact body still reaches the upstream
// via the reverse proxy - this function only peeks at the body, it never
// changes what gets forwarded.
func readAndRestoreBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, proxyRequestMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > proxyRequestMaxBytes {
		return nil, fmt.Errorf("anthproxy: request body exceeds %d bytes", proxyRequestMaxBytes)
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	r.ContentLength = int64(len(b))
	return b, nil
}

// probeRequest extracts the /v1/messages request's "model" field and the
// sha256 hex of its "system[0]" block (task spec step 4) without altering
// what gets forwarded. A malformed body yields ("", "") - metering must
// never be the reason a request fails.
func probeRequest(body []byte) (model string, systemHashHex string) {
	var probe struct {
		Model  string          `json:"model"`
		System json.RawMessage `json:"system"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return "", ""
	}
	model = probe.Model
	if len(probe.System) == 0 {
		return model, ""
	}

	var blocks []json.RawMessage
	zero := probe.System
	if err := json.Unmarshal(probe.System, &blocks); err == nil && len(blocks) > 0 {
		zero = blocks[0]
	}
	sum := sha256.Sum256(zero)
	return model, hex.EncodeToString(sum[:])
}

// usageCapturingBody wraps the upstream response body so ServeHTTP's
// caller can extract Usage (step 2) as the bytes are relayed to the
// client, without buffering the whole streamed response before forwarding
// it (SSE must flow unbuffered - step 1). It never alters what Read
// returns to its caller; it only mirrors those same bytes into a small
// local parser as they pass through.
type usageCapturingBody struct {
	io.ReadCloser
	isSSE bool

	sse     SSEUsageAccumulator
	partial []byte // trailing partial SSE line, carried across Read calls
	jsonBuf bytes.Buffer

	onDone func(u Usage, bodyErr error) // called exactly once, on the first error/EOF Read observes
	done   bool
}

func (b *usageCapturingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		chunk := p[:n]
		if b.isSSE {
			b.feedSSE(chunk)
		} else {
			b.jsonBuf.Write(chunk)
		}
	}
	if err != nil && !b.done {
		b.done = true
		if b.isSSE {
			b.onDone(b.sse.Usage(), err)
		} else {
			// A non-stream response is already complete before any byte
			// reaches the client, so a JSON parse failure here means the
			// upstream sent something unexpected - not a proxy fault. u
			// is simply the zero value in that case; onDone's caller
			// (wrapResponseBody) still records the call (usd=0) so it is
			// never silently dropped from the ledger.
			u, _ := ParseNonStreamUsage(b.jsonBuf.Bytes())
			b.onDone(u, err)
		}
	}
	return n, err
}

func (b *usageCapturingBody) feedSSE(chunk []byte) {
	b.partial = append(b.partial, chunk...)
	for {
		i := bytes.IndexByte(b.partial, '\n')
		if i < 0 {
			break
		}
		line := b.partial[:i]
		b.partial = b.partial[i+1:]
		b.sse.Feed(string(line))
	}
}

// wrapResponseBody is httputil.ReverseProxy's ModifyResponse hook: it
// installs usageCapturingBody around resp.Body and, once the body is
// fully drained, prices the observed Usage and hands it to the Governor.
func (p *Proxy) wrapResponseBody(resp *http.Response) error {
	data, _ := resp.Request.Context().Value(reqDataCtxKey{}).(*reqData)
	if data == nil {
		data = &reqData{Start: p.cfg.Now()}
	}
	isSSE := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
	status := resp.StatusCode

	wrapped := &usageCapturingBody{ReadCloser: resp.Body, isSSE: isSSE}
	wrapped.onDone = func(u Usage, bodyErr error) {
		durationMs := time.Since(data.Start).Milliseconds()
		statusStr := "ok"
		if status >= 400 {
			statusStr = "error"
		} else if bodyErr != nil && !errors.Is(bodyErr, io.EOF) {
			statusStr = "error"
		}

		var usd float64
		if row, priceErr := PriceFor(data.Model, p.cfg.Now()); priceErr == nil {
			usd = u.CostUSD(row)
		}

		// context.Background(): the inbound request's own context may
		// already be cancelled by the time the body finishes draining
		// (client disconnect, worker moved on) - a disconnected worker
		// must never prevent kahyad from recording that the call
		// happened, mirroring kahyad/internal/server/task.go's
		// persistCtx convention.
		p.cfg.Governor.RecordUsage(context.Background(), data.Reservation, p.cfg.EventLedger, p.cfg.TraceID, p.cfg.TaskID,
			data.Model, u, usd, statusStr, durationMs, data.SystemHash)
	}
	resp.Body = wrapped
	return nil
}

// handleUpstreamError passes upstream/connection failures through as a
// generic Anthropic-shaped 502 - retry/backoff/error taxonomy is W4-04
// (out of scope here); this proxy never invents its own retry behavior.
func handleUpstreamError(w http.ResponseWriter, _ *http.Request, _ error) {
	writeAnthropicError(w, http.StatusBadGateway, "api_error", "Yukari akis baglantisi basarisiz.")
}

// --- Anthropic-shaped error bodies ---

type anthropicErrorResponse struct {
	Type  string             `json:"type"`
	Error anthropicErrorBody `json:"error"`
}

type anthropicErrorBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func writeAnthropicError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(anthropicErrorResponse{
		Type:  "error",
		Error: anthropicErrorBody{Type: errType, Message: message},
	})
}
