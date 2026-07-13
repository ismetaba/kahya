// Package server implements kahyad's control-plane HTTP-over-UDS listener
// (HANDOFF §4 ⚑ IPC contract). This skeleton (W12-01) owns the socket
// lifecycle, /health, and graceful shutdown; later tasks (W12-02..09) mount
// additional routes (e.g. /policy/check) onto the same *http.Server.
package server

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"kahya/kahyad/internal/anthproxy"
	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/egress"
	"kahya/kahyad/internal/factengine"
	"kahya/kahyad/internal/indexer"
	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/notify"
	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/search"
	"kahya/kahyad/internal/secretlane"
	"kahya/kahyad/internal/task"
	"kahya/kahyad/internal/traceid"
	mcpfs "kahya/mcp/fs"
	"kahya/mcp/memory"
	mcposascript "kahya/mcp/osascript"
	mcpshell "kahya/mcp/shell"
)

// ErrAlreadyRunning is returned by Prepare/Run when a live kahyad instance
// already holds the configured socket and answers /health.
var ErrAlreadyRunning = errors.New("server: another kahyad instance is already running")

// DBHealth is the health data source /health reports under "db" and
// "schema_version" (W12-02). It is a narrow interface — rather than the
// concrete *store.Store — so this package does not have to pull in the
// sqlite/cgo dependency just to serve HTTP; *store.Store satisfies it
// without any adapter code.
type DBHealth interface {
	Health(ctx context.Context) (ok bool, schemaVersion int64, err error)
}

// Searcher is the fused BM25 search source /v1/memory/search reports
// (W12-03 step 4). It is a narrow interface - *search.Searcher satisfies
// it without any adapter code - so tests can fake it without a real
// brain.db.
type Searcher interface {
	Search(ctx context.Context, traceID, query string, k int) ([]search.Hit, error)
}

// Reindexer is the corpus-indexing source POST /v1/reindex reports
// (W12-04 step 5; W12-11 step 5 adds reEmbed). It is a narrow interface -
// kahyad/internal/embed.ReindexBackfiller satisfies it without any
// adapter code - so tests can fake it without a real brain.db or
// memory-dir corpus.
type Reindexer interface {
	// Reindex re-walks the corpus (full forces indexer.Indexer's own
	// rechunk-regardless-of-hash behavior) and then backfills chunk
	// vectors (reEmbed forces re-embedding EVERY chunk + purging stale
	// model_ver rows, HANDOFF §4 ⚑ model_ver rule - the W12-11 step 5
	// re_embed trigger).
	Reindex(ctx context.Context, traceID string, full, reEmbed bool) (indexer.Result, error)
}

// EmbedHealth is the local embedding service's supervisor status source
// /health reports under "embed" (W12-11 step 2: "ok|starting|down|
// disabled"). kahyad/internal/mlxsup.Supervisor satisfies it without any
// adapter code.
type EmbedHealth interface {
	State() string
}

const (
	healthCheckDialTimeout = 500 * time.Millisecond
	healthCheckTimeout     = 1 * time.Second
	readHeaderTimeout      = 5 * time.Second
	shutdownTimeout        = 5 * time.Second
)

// Server is kahyad's HTTP-over-UDS control-plane server.
type Server struct {
	cfg         config.Config
	log         *logx.Logger
	version     string
	db          DBHealth
	search      Searcher
	reindex     Reindexer
	embedHealth EmbedHealth

	// eventLogger, mcpMemoryDir, mcpIndexer wire POST /v1/mcp and
	// /v1/memory/search's for_injection ledgering (W12-05) - see
	// mcp.go's SetEventLogger/SetMCPMemory doc comments.
	eventLogger  EventLogger
	mcpMemoryDir string
	mcpIndexer   memory.Indexer

	// taskStore wires POST /v1/task's tasks-table persistence (W12-07 step
	// 4). See SetTaskStore's doc comment.
	taskStore TaskStore

	// policyEngine is the W3-02 autonomy-ladder decision engine consulted
	// by handlePolicyCheck (task.go) and policyGateMiddleware (mcp.go),
	// once s.denyAll itself is not already overriding the decision. See
	// SetPolicyEngine's doc comment (mcp.go).
	policyEngine *policy.Engine

	// fsServer is the W3-03 fs MCP tool set (fs_read/fs_write/fs_delete),
	// registered onto the SAME shared /v1/mcp server as mcp/memory's tools
	// (buildMCPHandler) and consulted by handlePolicyUndo/
	// handlePolicyFeedback (policy.go) to execute the owning recipe once
	// kahyad/internal/policy.Engine.TriggerUndo has flipped the window and
	// demoted the ladder state. nil until SetFSTool is called - see fs.go.
	fsServer *mcpfs.Server

	// shellServer is the W3-04 shell MCP tool set (shell_docker/
	// shell_host), registered onto the SAME shared /v1/mcp server as
	// mcp/memory's/mcp/fs's own tools (buildMCPHandler). nil until
	// SetShellTool is called - see shell.go. Server.Shutdown calls
	// shellServer.Shutdown to kill every kahya.task_id-labeled container
	// this daemon may have left running (this task's spec step 7).
	shellServer *mcpshell.Server

	// osascriptServer is the W3-09 osascript MCP tool set (applescript_run/
	// jxa_run/shortcuts_run), registered onto the SAME shared /v1/mcp
	// server as mcp/memory's/mcp/fs's/mcp/shell's own tools
	// (buildMCPHandler). nil until SetOsascriptTool is called - see
	// osascript.go.
	osascriptServer *mcposascript.Server

	// anthGovernor/anthNotifier/anthCredential/anthEgressGateFactory wire
	// POST /v1/task's per-task Anthropic forward-proxy + cost governor
	// (W12-08). See SetAnthproxy's doc comment (task.go).
	anthGovernor          *anthproxy.Governor
	anthNotifier          notify.Notifier
	anthCredential        anthproxy.CredentialSource
	anthEgressGateFactory func(taskID, traceID string) func(*http.Request) error

	// egressGate is the W3-05 decision engine (kahya/internal/egress.Gate)
	// POST /session/sensitive-read consults (egress.go), and the SAME Gate
	// task.go's per-task anthproxy egress-gate factory and main.go's
	// egress.Proxy both call — one gate, every in-process caller. See
	// SetEgressGate's doc comment (egress.go).
	egressGate *egress.Gate

	// secretLaneClassifier/secretLaneAnswerer/markSensitiveRead wire
	// W3-08's ingest-time classifier + local-only answer path. nil (the
	// default) means every task is classified lane="normal"
	// unconditionally — see SetSecretLane's doc comment (task.go) — so
	// every pre-W3-08 test/caller that never wires this in keeps its exact
	// original behavior.
	secretLaneClassifier *secretlane.Classifier
	secretLaneAnswerer   secretlane.Answerer
	markSensitiveRead    func(ctx context.Context, sessionKey, traceID string) error

	// taskResolver/taskDurabilityStore/taskLive wire GET /v1/task/status
	// and POST /v1/task/resolve (W4-02) - see task_durability.go's
	// SetTaskDurability doc comment. nil until that setter is called - both
	// routes answer 503 the same "unwired dependency" way SetSearcher/
	// SetReindexer do.
	taskResolver        *task.Resolver
	taskDurabilityStore TaskDurabilityStore
	taskLive            LivePIDLookup
	// taskLiveRegistry additionally receives Register/Unregister calls
	// around every worker handleTask spawns (task.go) - see
	// SetTaskLiveRegistry's doc comment. May be the exact same value as
	// taskLive (both interfaces a *task.LiveRegistry satisfies), kept as a
	// separate field only because the two are conceptually different
	// capabilities (read-only lookup vs. register/unregister).
	taskLiveRegistry TaskLiveRegistry

	// taskMachine/taskCloudRetry wire the W4-04 cloud-call error taxonomy:
	// taskMachine drives handleTask's own intent->executing transition
	// (task.go); taskCloudRetry is NewTaskProxy's OnCloudUnreachable/
	// OnNonRetryableFailure callback target (task.go's own doc comment).
	// See SetTaskCloudRetry's doc comment (task_durability.go). Both nil
	// until that setter is called - matching this package's usual
	// unwired-dependency posture (a nil taskMachine simply skips the
	// transition; NewTaskProxy's callbacks no-op when taskCloudRetry is
	// nil).
	taskMachine    *task.Machine
	taskCloudRetry *task.CloudRetry

	// devStubReceipts wires the W4-07 acceptance-gate-only w2_slow_stub MCP
	// tool (devstub.go) - see SetDevStub's own doc comment. nil (the
	// default, and ALWAYS nil in production - main.go only calls SetDevStub
	// under KAHYA_ENV=dev) means buildMCPHandler (mcp.go) never registers
	// the tool onto the shared MCP server at all.
	devStubReceipts *task.Receipts

	// sessionTaintDB is the raw *sql.DB handle handleTask's OnSession
	// callback opens its own transaction against (W4-03 task spec step 1a:
	// insert this task's session_taint(tier=clean) row in the SAME
	// transaction as UpdateTaskSession's own session_started persistence).
	// See SetSessionTaintDB's doc comment. nil (the default) means that
	// callback falls back to the plain, non-transactional UpdateTaskSession
	// call every pre-W4-03 caller/test already made - no session_taint row
	// is inserted in that configuration (this package's usual "unwired
	// dependency" posture).
	sessionTaintDB *sql.DB

	// scheduler wires POST /jobs/trigger/{name} (W4-01): job registry,
	// resolution, and async dispatch+ledgering all live in
	// kahyad/internal/scheduler.Scheduler (jobs.go's SetScheduler doc
	// comment); nil until SetScheduler is called - the route answers 503
	// the same "unwired dependency" way SetSearcher/SetReindexer do.
	scheduler JobScheduler

	// ledgerVerifier wires POST /v1/ledger/verify (W4-05): the
	// recompute-from-event-1 tamper check lives in
	// kahyad/internal/anchor.Verifier (ledger.go's SetLedgerVerifier doc
	// comment); nil until that setter is called - the route answers 503
	// the same "unwired dependency" way SetSearcher/SetReindexer do.
	ledgerVerifier LedgerVerifier

	// consolidation wires GET/POST /v1/consolidation* (W5-02): the nightly
	// consolidation show/approve/reject control routes live in
	// kahyad/internal/consolidation.Consolidator (consolidation.go's
	// SetConsolidation doc comment); nil until that setter is called - the
	// routes answer 503 the same "unwired dependency" way SetSearcher/
	// SetReindexer/SetLedgerVerifier do.
	consolidation ConsolidationRunner

	// factEngine wires /v1/fact/confirm, /v1/fact/retract, /v1/entity/
	// merge, /v1/entity/split (W5-04, factengine.go's own doc comment);
	// nil until SetFactEngine is called - same "unwired dependency"
	// convention as consolidation above.
	factEngine FactEngineRunner

	// remembered wires POST /v1/remembered (W5-03, remembered.go's own doc
	// comment); nil until SetRememberedMarker is called - same "unwired
	// dependency" convention as factEngine above.
	remembered RememberedMarker

	// denyAll is W3-01's deny-all-mode flag: set (via SetDenyAll, before
	// Prepare) when policy.yaml failed to load/validate at boot. Both
	// /policy/check (task.go's handlePolicyCheck) and /v1/mcp's
	// policyGateMiddleware (mcp.go) consult it BEFORE consulting
	// kahyad/internal/policy's interim static table (or, once W3-02 lands,
	// the real ladder engine) - deny-all overrides even memory_search, the
	// one tool the interim table itself allows. There is deliberately no
	// path back out of deny-all short of restarting kahyad with a fixed
	// policy.yaml (HANDOFF §4 fail-closed posture: a policy load failure is
	// a startup-time, not a runtime-recoverable, condition).
	denyAll bool

	ln   net.Listener
	http *http.Server
	// lock is the exclusive startup flock on <socket>.lock, held for the
	// whole daemon lifetime. It serializes socket takeover across processes
	// and proves the socket at cfg.Socket is ours to unlink on Shutdown.
	// The kernel releases it on any process death, including SIGKILL.
	lock *os.File

	started time.Time
}

// New constructs a Server bound to cfg.Socket, reporting db's health at
// /health. Call Prepare (or Run, which calls Prepare for you) to actually
// bind the listener.
func New(cfg config.Config, log *logx.Logger, version string, db DBHealth) *Server {
	return &Server{cfg: cfg, log: log, version: version, db: db}
}

// AdoptStartupLock hands the Server a startup flock already acquired via
// AcquireStartupLock (the production path acquires it before the DB opens).
// The Server owns it from here: Shutdown releases it.
func (s *Server) AdoptStartupLock(f *os.File) {
	s.lock = f
}

// SetSearcher wires the /v1/memory/search route to searcher (W12-03 step
// 4). Call this before Prepare/Run. Kept as a setter (rather than a New
// parameter) so every existing New(...) call site - including every
// current server_test.go test - keeps working unchanged; /v1/memory/search
// answers 503 until a searcher is set.
func (s *Server) SetSearcher(searcher Searcher) {
	s.search = searcher
}

// SetReindexer wires the /v1/reindex route to r (W12-04 step 5). Call this
// before Prepare/Run. Kept as a setter, matching SetSearcher's rationale:
// every existing New(...) call site keeps working unchanged; /v1/reindex
// answers 503 until a Reindexer is set.
func (s *Server) SetReindexer(r Reindexer) {
	s.reindex = r
}

// SetEmbedHealth wires /health's "embed" field to sup (W12-11 step 2).
// Call this before Prepare/Run. Kept as a setter, matching SetSearcher's
// rationale: every existing New(...) call site keeps working unchanged;
// /health reports "embed":"disabled" until an EmbedHealth is set.
func (s *Server) SetEmbedHealth(sup EmbedHealth) {
	s.embedHealth = sup
}

// SetDenyAll puts the server into permanent deny-all mode (W3-01): every
// subsequent /policy/check response and /v1/mcp tools/call is denied,
// regardless of tool name - including memory_search, the one tool the
// interim static table itself allows. main.go calls this BEFORE Prepare
// (so no /policy/check can ever be answered any other way) whenever
// policy.Load(cfg.PolicyPath) fails; it also logs event=policy_load_failed
// itself, in the same boot sequence, before the socket is even bound.
func (s *Server) SetDenyAll() {
	s.denyAll = true
}

// DenyAll reports whether the server is in W3-01 deny-all mode.
func (s *Server) DenyAll() bool {
	return s.denyAll
}

// Prepare resolves the socket takeover logic (HANDOFF §4 IPC step 3) and
// binds the listener, but does not yet start serving:
//   - socket file missing → bind fresh.
//   - socket file present and a live daemon answers /health → return
//     ErrAlreadyRunning (already logged as event=already_running).
//   - socket file present but dead (dial fails) → unlink and bind fresh.
//
// The bound socket is chmod'd 0600.
func (s *Server) Prepare() error {
	ln, lock, err := prepareListener(s.cfg.Socket, s.lock)
	if err != nil {
		if errors.Is(err, ErrAlreadyRunning) {
			s.log.Error("already_running", "socket", s.cfg.Socket)
		}
		return err
	}
	s.ln = ln
	s.lock = lock
	s.started = time.Now()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/v1/memory/search", s.handleMemorySearch)
	mux.HandleFunc("/v1/reindex", s.handleReindex)
	mux.HandleFunc("/v1/log", s.handleLog)
	mux.Handle("/v1/mcp", s.buildMCPHandler())
	mux.HandleFunc("/v1/task", s.handleTask)
	mux.HandleFunc("/policy/check", s.handlePolicyCheck)
	mux.HandleFunc("/policy/consume-token", s.handlePolicyConsumeToken)
	mux.HandleFunc("/policy/feedback", s.handlePolicyFeedback)
	mux.HandleFunc("/policy/approvals", s.handlePolicyApprovals)
	mux.HandleFunc("/policy/state", s.handlePolicyState)
	mux.HandleFunc("/policy/promote", s.handlePolicyPromote)
	mux.HandleFunc("/policy/undo", s.handlePolicyUndo)
	mux.HandleFunc("/session/sensitive-read", s.handleSensitiveRead)
	mux.HandleFunc(jobTriggerPrefix, s.handleJobTrigger)
	mux.HandleFunc("/v1/task/status", s.handleTaskStatus)
	mux.HandleFunc("/v1/task/resolve", s.handleTaskResolve)
	mux.HandleFunc("/v1/ledger/verify", s.handleLedgerVerify)
	mux.HandleFunc("/v1/consolidation", s.handleConsolidationShow)
	mux.HandleFunc("/v1/consolidation/approve", s.handleConsolidationApprove)
	mux.HandleFunc("/v1/consolidation/reject", s.handleConsolidationReject)
	mux.HandleFunc("/v1/fact/confirm", s.handleFactConfirm)
	mux.HandleFunc("/v1/fact/retract", s.handleFactRetract)
	mux.HandleFunc("/v1/entity/merge", s.handleEntityMerge)
	mux.HandleFunc("/v1/entity/split", s.handleEntitySplit)
	mux.HandleFunc("/v1/remembered", s.handleRemembered)

	s.http = &http.Server{
		Handler:           s.withTraceLogging(mux),
		ReadHeaderTimeout: readHeaderTimeout,
	}
	return nil
}

// Serve blocks accepting connections until Shutdown is called or a fatal
// listener error occurs. It returns nil on a clean shutdown.
func (s *Server) Serve() error {
	err := s.http.Serve(s.ln)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown gracefully stops the HTTP server (5s budget) and unlinks the
// socket file. It does not log shutdown_complete — the caller (main.go)
// logs that on its boot-scoped logger after Shutdown returns, since it is
// not tied to any single request.
func (s *Server) Shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	err := s.http.Shutdown(ctx)

	// W3-04 spec step 7: "on kahyad shutdown, kill all containers labeled
	// kahya.task_id" - best-effort, never fails shutdown itself over a
	// docker-daemon hiccup.
	if s.shellServer != nil {
		if killErr := s.shellServer.Shutdown(ctx); killErr != nil {
			s.log.Warn("shell_shutdown_kill_failed", "err", killErr.Error())
		}
	}

	// Safe: we have held the startup flock since Prepare, so no other
	// daemon can have bound this path in the meantime.
	_ = os.Remove(s.cfg.Socket)
	if s.lock != nil {
		_ = s.lock.Close() // releases the flock; the .lock file stays (never unlink a lock file)
		s.lock = nil
	}
	return err
}

// Run prepares, serves, and blocks until ctx is cancelled, at which point
// it performs a graceful Shutdown. It returns ErrAlreadyRunning immediately
// if another instance already holds the socket.
func (s *Server) Run(ctx context.Context) error {
	if err := s.Prepare(); err != nil {
		return err
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- s.Serve() }()

	select {
	case <-ctx.Done():
		return s.Shutdown()
	case err := <-serveErr:
		return err
	}
}

// AcquireStartupLock takes the exclusive cross-process flock that serializes
// the entire daemon startup — socket takeover AND everything before it.
// main.go MUST call this before store.Open: goose migrations against a
// fresh brain.db are not safe to race, and kahyad being brain.db's only
// writer implies at most one kahyad exists from the first DB byte on.
// Pass the returned lock to the Server via AdoptStartupLock. It also
// creates the socket directory and tightens it to 0700 even when it
// pre-existed with looser permissions (MkdirAll alone is a no-op on an
// existing directory's mode).
func AcquireStartupLock(socketPath string) (*os.File, error) {
	dir := filepath.Dir(socketPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("server: create socket dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("server: chmod socket dir %s: %w", dir, err)
	}

	f, err := os.OpenFile(socketPath+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("server: open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			// Another instance holds the lock: it is either serving already
			// or mid-startup and about to. Either way, we must not start.
			return nil, ErrAlreadyRunning
		}
		return nil, fmt.Errorf("server: flock %s.lock: %w", socketPath, err)
	}
	return f, nil
}

// prepareListener implements the socket takeover decision described on
// Prepare, returning a bound, chmod 0600 unix listener plus the held
// startup lock. The flock makes the stat→probe→remove→listen sequence
// atomic across processes — without it, two racing startups can both
// conclude the socket is dead, both bind, and later unlink each other's
// live socket. If lock is nil it is acquired here (test convenience);
// the production path acquires it earlier, before the DB opens.
func prepareListener(socketPath string, lock *os.File) (net.Listener, *os.File, error) {
	if lock == nil {
		var err error
		lock, err = AcquireStartupLock(socketPath)
		if err != nil {
			return nil, nil, err
		}
	}

	if _, err := os.Stat(socketPath); err == nil {
		alive := probeHealth(socketPath)
		if alive {
			lock.Close()
			return nil, nil, ErrAlreadyRunning
		}
		// Dead socket file: unlink before binding a fresh one.
		if err := os.Remove(socketPath); err != nil {
			lock.Close()
			return nil, nil, fmt.Errorf("server: remove stale socket %s: %w", socketPath, err)
		}
	} else if !os.IsNotExist(err) {
		lock.Close()
		return nil, nil, fmt.Errorf("server: stat socket %s: %w", socketPath, err)
	}

	// Tighten the umask so the socket is never observable with wider
	// permissions than 0600, even before the explicit chmod below. The
	// enclosing directory is already 0700, so this is defense in depth.
	oldMask := syscall.Umask(0o177)
	ln, err := net.Listen("unix", socketPath)
	syscall.Umask(oldMask)
	if err != nil {
		lock.Close()
		return nil, nil, fmt.Errorf("server: listen on %s: %w", socketPath, err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		ln.Close()
		lock.Close()
		return nil, nil, fmt.Errorf("server: chmod socket %s: %w", socketPath, err)
	}
	return ln, lock, nil
}

// probeHealth dials socketPath and asks /health; it returns true only if a
// live daemon answers 200. Any dial/request error is treated as "not
// alive" — the caller then unlinks and rebinds. This mirrors the fail-safe
// posture elsewhere in the system: ambiguity resolves toward "take over a
// dead socket" rather than "refuse to start forever".
func probeHealth(socketPath string) bool {
	client := &http.Client{
		Timeout: healthCheckTimeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				d := net.Dialer{Timeout: healthCheckDialTimeout}
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
	}
	resp, err := client.Get("http://kahyad/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

type healthResponse struct {
	Status        string `json:"status"`
	PID           int    `json:"pid"`
	UptimeS       int64  `json:"uptime_s"`
	Version       string `json:"version"`
	DB            string `json:"db"`
	SchemaVersion int64  `json:"schema_version"`
	// Embed is the local MLX embedding service's supervisor status
	// (W12-11 step 2): "ok"|"starting"|"down"|"disabled". "disabled" is
	// also what a kahyad build with no EmbedHealth wired at all reports -
	// never claiming readiness for a capability that was never
	// configured (same fail-safe posture as DB's "error" default below).
	Embed string `json:"embed"`
}

// handleHealth reports process liveness plus brain.db reachability and
// schema version (W12-02 step "extend /health"), plus the local embedding
// service's supervisor state (W12-11 step 2). db is "ok" only when a live
// ping against brain.db succeeds; any ping failure or a nil db (should
// never happen outside of misconfigured tests) reports "error" — this
// endpoint never claims the database is fine when it hasn't verified that.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	dbStatus := "error"
	var schemaVersion int64
	if s.db != nil {
		ok, version, err := s.db.Health(r.Context())
		schemaVersion = version
		if err == nil && ok {
			dbStatus = "ok"
		}
	}

	embedStatus := "disabled"
	if s.embedHealth != nil {
		embedStatus = s.embedHealth.State()
	}

	resp := healthResponse{
		Status:        "ok",
		PID:           os.Getpid(),
		UptimeS:       int64(time.Since(s.started).Seconds()),
		Version:       s.version,
		DB:            dbStatus,
		SchemaVersion: schemaVersion,
		Embed:         embedStatus,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// memorySearchRequest is POST /v1/memory/search's request body (W12-03
// step 4). trace_id is optional: a caller resuming a multi-step trace
// supplies its own trace_id here on purpose, independent of the
// X-Kahya-Trace-Id header withTraceLogging uses for the outer
// event=http_request line. When the body omits it, handleMemorySearch
// falls back to withTraceLogging's own resolved trace id (via
// traceIDFromContext) rather than minting a fresh one, so the
// event=memory_search JSONL line correlates with the event=http_request
// line for the SAME request (MINOR 6).
type memorySearchRequest struct {
	Query   string `json:"query"`
	K       int    `json:"k"`
	TraceID string `json:"trace_id"`
	// ForInjection and TaskID are W12-05 step 1's extension: when
	// ForInjection is true, the response additionally renders a <hafiza>
	// injection block (excluding 'agent_derived' quarantined episodes,
	// HANDOFF §5 memory #1) and ledgers it (kind='hafiza_injected',
	// HANDOFF §5 safety #4). TaskID is carried into that ledger payload
	// only - it does not affect ranking/search itself.
	ForInjection bool   `json:"for_injection"`
	TaskID       string `json:"task_id"`
}

type memorySearchResultItem struct {
	ChunkID    int64   `json:"chunk_id"`
	EpisodeID  int64   `json:"episode_id"`
	Path       string  `json:"path"`
	Text       string  `json:"text"`
	Score      float64 `json:"score"`
	SourceTier string  `json:"source_tier"`
}

type memorySearchResponse struct {
	Results []memorySearchResultItem `json:"results"`
	// HafizaBlock is only populated when the request set for_injection:true
	// (W12-05 step 1d). Its bytes are exactly what was ledgered as the
	// hafiza_injected event's payload.block (safety #4 forensic
	// traceability: the injected block and the ledgered block must be
	// byte-identical, sha256-verifiable).
	HafizaBlock string `json:"hafiza_block,omitempty"`
}

type errorResponse struct {
	Error string `json:"error"`
}

// handleMemorySearch is the raw internal ranking API (W12-03 step 4); the
// <hafiza> injection-eligibility layer on top is W12-05. An empty query
// (or one that is all whitespace) is a 400, never a panic; k<=0 defaults to
// 8 inside search.Searcher.Search.
func (s *Server) handleMemorySearch(w http.ResponseWriter, r *http.Request) {
	// Deny-all fail-closed (W3-01): policy.yaml failed to load, so no policy
	// decision can be trusted. /v1/memory/search is the endpoint the worker's
	// UserPromptSubmit hook calls to build the <hafiza> block that reaches the
	// cloud model, so it must NOT serve memory content while policy is
	// untrusted (deny-all must override even memory_search here, not only at
	// /policy/check and /v1/mcp). The worker hook treats a non-200 as
	// "continue without injection" — a safe degrade.
	if s.DenyAll() {
		writeJSONError(w, http.StatusForbidden, policy.ReasonDenyAll)
		return
	}
	if s.search == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "memory search not available")
		return
	}

	var req memorySearchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		writeJSONError(w, http.StatusBadRequest, "query must not be empty")
		return
	}

	traceID := req.TraceID
	if traceID == "" {
		// MINOR 6: fall back to withTraceLogging's resolved trace id (the
		// inbound X-Kahya-Trace-Id header, or one it freshly minted) rather
		// than letting search.Searcher.Search mint its own - otherwise the
		// event=memory_search line can never be correlated with this
		// request's event=http_request line.
		traceID = traceIDFromContext(r)
	}

	hits, err := s.search.Search(r.Context(), traceID, req.Query, req.K)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	// W12-05 step 1a / W5-04: for_injection=true excludes 'agent_derived'
	// episodes via kahyad/internal/factengine.TierInjectionEligible - the
	// SAME tier-quarantine rule facts.Fact rows are held to
	// (factengine.InjectionEligible), applied here to CHUNK/episode hits
	// (which carry only source_tier, never a per-fact confirmed_at) with
	// confirmed always false - a chunk has no per-item human confirmation
	// mechanism of its own, only facts do (kahya fact confirm). This
	// filters BOTH the returned results AND what the renderer/ledger below
	// ever sees - "absent with for_injection:true" means absent from the
	// response entirely, not merely excluded from hafiza_block.
	filtered := hits
	if req.ForInjection {
		filtered = make([]search.Hit, 0, len(hits))
		for _, h := range hits {
			if !factengine.TierInjectionEligible(h.SourceTier, false) {
				continue
			}
			filtered = append(filtered, h)
		}
	}

	resp := memorySearchResponse{Results: make([]memorySearchResultItem, 0, len(filtered))}
	for _, h := range filtered {
		resp.Results = append(resp.Results, memorySearchResultItem{
			ChunkID:    h.ChunkID,
			EpisodeID:  h.EpisodeID,
			Path:       h.Path,
			Text:       h.Text,
			Score:      h.Score,
			SourceTier: h.SourceTier,
		})
	}

	if req.ForInjection {
		memHits := make([]memory.Hit, len(filtered))
		for i, h := range filtered {
			memHits[i] = memory.Hit{ChunkID: h.ChunkID, Path: h.Path, Seq: h.Seq, Text: h.Text, Score: h.Score, SourceTier: h.SourceTier}
		}
		block, kept := memory.RenderKept(memHits, memory.DefaultTopK)
		resp.HafizaBlock = block

		// W12-05 step 1c/HANDOFF §5 safety #4: ledger the EXACT injected
		// bytes (forensic poisoning traceability) - chunk_ids is only the
		// chunks that actually survived RenderKept's top-k/budget trim,
		// never the broader filtered candidate pool.
		if s.eventLogger != nil {
			chunkIDs := make([]int64, len(kept))
			for i, h := range kept {
				chunkIDs[i] = h.ChunkID
			}
			sum := sha256.Sum256([]byte(block))
			payload := map[string]any{
				"task_id":      req.TaskID,
				"chunk_ids":    chunkIDs,
				"block_sha256": hex.EncodeToString(sum[:]),
				"block":        block,
			}
			if err := s.eventLogger.LogEvent(r.Context(), traceID, "hafiza_injected", payload); err != nil {
				s.log.With(traceID).Warn("hafiza_injected_ledger_error", "err", err.Error())
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// reindexRequest is POST /v1/reindex's request body (W12-04 step 5).
// {"full": false} is the default (an empty/omitted body also defaults
// full to false, see handleReindex). trace_id follows the same optional-
// override pattern as memorySearchRequest.TraceID (MINOR 6): a caller
// resuming a multi-step trace supplies its own trace_id here on purpose;
// when the body omits it, handleReindex falls back to withTraceLogging's
// own resolved trace id rather than letting Reindex mint an unrelated one.
type reindexRequest struct {
	Full    bool   `json:"full"`
	TraceID string `json:"trace_id"`
	// ReEmbed is the W12-11 step 5 trigger: re-embeds EVERY chunk under
	// cfg.ActiveEmbedModelVer and purges chunk_vec rows left under any
	// other (stale) model_ver. {"full":false,"re_embed":true} is the
	// documented version-switch procedure's shape (mlx/embed/README.md).
	ReEmbed bool `json:"re_embed"`
}

// reindexResponse is exactly the five-key schema the task spec fixes:
// {"files_indexed","files_unchanged","files_removed","chunks",
// "duration_ms"}. indexer.Result also tracks files_errored (logged and
// carried in the ledger event payload), but that key is deliberately left
// out of the HTTP response to match the spec's schema exactly.
type reindexResponse struct {
	FilesIndexed   int   `json:"files_indexed"`
	FilesUnchanged int   `json:"files_unchanged"`
	FilesRemoved   int   `json:"files_removed"`
	Chunks         int   `json:"chunks"`
	DurationMs     int64 `json:"duration_ms"`
}

// handleReindex triggers a corpus reindex (W12-04 step 5). A second,
// concurrent call while one is already running answers 409 with the exact
// Turkish body the task spec fixes (this is the one user-facing string on
// this route - kahya reindex, W12-06, surfaces it verbatim to the CLI).
func (s *Server) handleReindex(w http.ResponseWriter, r *http.Request) {
	if s.reindex == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "reindex not available")
		return
	}

	var req reindexRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	traceID := req.TraceID
	if traceID == "" {
		traceID = traceIDFromContext(r)
	}

	res, err := s.reindex.Reindex(r.Context(), traceID, req.Full, req.ReEmbed)
	if err != nil {
		if errors.Is(err, indexer.ErrReindexInProgress) {
			writeJSONError(w, http.StatusConflict, "reindex zaten çalışıyor")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	resp := reindexResponse{
		FilesIndexed:   res.FilesIndexed,
		FilesUnchanged: res.FilesUnchanged,
		FilesRemoved:   res.FilesRemoved,
		Chunks:         res.Chunks,
		DurationMs:     res.DurationMs,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// logLineResponse is GET /v1/log's response body (W12-06 deliverable: this
// endpoint is read-only log plumbing owned by kahyad; the kahya CLI, W12-06's
// client, is the only consumer of it so far). Lines is every matching JSONL
// line, each decoded to a generic object and re-encoded, ordered by ts
// ascending across every source file.
type logLineResponse struct {
	Lines []map[string]any `json:"lines"`
}

// handleLog answers GET /v1/log?trace_id=<id>: it scans every
// <log_dir>/*.jsonl file for lines whose trace_id matches the query
// parameter, ordered by ts ascending. This is read-only log plumbing, not
// task/policy logic, so it never fails closed on a security decision - only
// on malformed input (empty trace_id) or I/O errors reading the log
// directory itself.
func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	traceID := strings.TrimSpace(r.URL.Query().Get("trace_id"))
	if traceID == "" {
		writeJSONError(w, http.StatusBadRequest, "trace_id must not be empty")
		return
	}

	lines, err := readLogLines(s.cfg.LogDir, traceID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(logLineResponse{Lines: lines})
}

// timedLine pairs a decoded JSONL line with its parsed ts (and whether that
// parse succeeded), purely so readLogLines can sort by time without
// re-parsing ts as a string compare (RFC3339Nano strips trailing zero
// fractional digits, so it is not always lexicographically sortable).
type timedLine struct {
	ts   time.Time
	tsOK bool
	line map[string]any
}

// readLogLines implements handleLog's scan: every *.jsonl file directly
// under logDir is opened, read line by line, and each line whose
// "trace_id" field equals traceID is decoded, tagged with "proc" (the
// file's basename minus ".jsonl" - e.g. "kahyad.jsonl" -> "kahyad",
// "worker.jsonl" -> "worker", per W12-06 step 4: "proc derived from source
// file name"), and collected. A missing log dir is not an error (a fresh
// install may not have logged anything yet under it); a malformed line or an
// unreadable file is skipped rather than failing the whole request, since
// one bad line/rotated-away file must not hide every other matching line.
//
// Lines are read with a bufio.Reader + ReadString('\n') loop rather than
// bufio.Scanner (BLOCKER 3): Scanner's default token buffer caps out at 1MB
// and would otherwise treat one oversized JSONL line as EOF, silently
// dropping every subsequent line in that file - including later real
// matches. ReadString has no such cap.
func readLogLines(logDir, traceID string) ([]map[string]any, error) {
	paths, err := filepath.Glob(filepath.Join(logDir, "*.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("server: glob log dir %s: %w", logDir, err)
	}

	var timed []timedLine
	for _, path := range paths {
		proc := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		f, err := os.Open(path)
		if err != nil {
			continue // rotated/removed mid-scan: skip, don't fail the request
		}
		r := bufio.NewReader(f)
		for {
			text, readErr := r.ReadString('\n')
			raw := strings.TrimSpace(text)
			if raw == "" {
				if readErr != nil {
					break
				}
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(raw), &m); err != nil {
				// Malformed (or, per BLOCKER 3, merely oversized-and-thus-
				// truncated-looking) line: skip just this line and keep
				// reading the rest of the file - don't fail the whole scan.
				if readErr != nil {
					break
				}
				continue
			}
			if tid, _ := m["trace_id"].(string); tid == traceID {
				m["proc"] = proc
				var ts time.Time
				var tsOK bool
				if s, ok := m["ts"].(string); ok {
					if parsed, err := time.Parse(time.RFC3339Nano, s); err == nil {
						ts, tsOK = parsed, true
					}
				}
				timed = append(timed, timedLine{ts: ts, tsOK: tsOK, line: m})
			}
			if readErr != nil {
				break
			}
		}
		f.Close()
	}

	// MINOR 7: a line with a missing/unparseable ts must sort AFTER every
	// valid-ts line, tie-broken by original read order - not before all of
	// them, which is what comparing raw zero-value time.Time would do.
	// SliceStable preserves relative order whenever less() is false both
	// ways, which is exactly the "both invalid: keep read order" case below.
	sort.SliceStable(timed, func(i, j int) bool {
		if timed[i].tsOK && timed[j].tsOK {
			return timed[i].ts.Before(timed[j].ts)
		}
		if timed[i].tsOK != timed[j].tsOK {
			return timed[i].tsOK // valid ts sorts before any invalid/missing ts
		}
		return false // both invalid: stable sort keeps original read order
	})

	out := make([]map[string]any, 0, len(timed))
	for _, t := range timed {
		out = append(out, t.line)
	}
	return out, nil
}

// writeJSONError writes a {"error": msg} body with the given status code -
// every error path in this package answers JSON, never a bare text/plain
// http.Error body, so CLI/worker callers can always json.Decode the
// response regardless of status.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorResponse{Error: msg})
}

// statusRecorder captures the status code written by a downstream handler
// so middleware can log it after the fact.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush forwards to the wrapped ResponseWriter's http.Flusher when it has
// one, so a handler behind this middleware (POST /v1/task's SSE stream,
// W12-07) can still flush each event to the client as it is written -
// without this, wrapping the ResponseWriter here would silently hide
// Flusher from every handler's own `w.(http.Flusher)` type assertion, even
// though the real underlying ResponseWriter supports it.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// traceIDContextKey is the unexported context key withTraceLogging stashes
// its resolved trace id under (MINOR 6), so a downstream handler can
// correlate its own JSONL lines with the outer event=http_request line
// instead of minting an unrelated trace id when a request has none of its
// own. An unexported type, rather than a bare string/int, follows
// context.WithValue's documented collision-avoidance idiom.
type traceIDContextKey struct{}

// traceIDFromContext returns the trace_id withTraceLogging resolved for r
// (the inbound X-Kahya-Trace-Id header, or a freshly minted one), or "" if
// r never passed through withTraceLogging - should not happen in
// production, since every route is mounted through it in Prepare.
func traceIDFromContext(r *http.Request) string {
	id, _ := r.Context().Value(traceIDContextKey{}).(string)
	return id
}

// withTraceLogging assigns/propagates a trace_id and logs event=http_request
// for every handled request (HANDOFF §4 IPC step 3). The resolved trace id
// is also stashed on the request context (traceIDFromContext) so handlers
// can correlate their own logging with this request without minting a
// second, uncorrelated trace id of their own (MINOR 6).
func (s *Server) withTraceLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := r.Header.Get("X-Kahya-Trace-Id")
		if id == "" {
			id = traceid.New()
		}
		r = r.WithContext(context.WithValue(r.Context(), traceIDContextKey{}, id))
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.With(id).Info("http_request",
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}
