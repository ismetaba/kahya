//go:build mlx

// Package mlxe2e is the mlx-tagged cross-lingual verification (W12-11
// step 7): requires the REAL Qwen3-Embedding-0.6B model (already
// downloaded by W0-03) and the REAL mlx/embed/.venv (see mlx/embed/
// README.md's setup section) - this package is gated behind the "mlx"
// build tag and run only by `make test-mlx`, never by plain `make test`.
// It lives under kahyad/internal/ (rather than alongside tests/e2e's
// other gates) specifically so it can import kahyad/internal/search
// directly for the FTS-only comparison below - Go's internal-package
// visibility rule only allows that from somewhere rooted under kahyad/.
//
// It spins up a REAL kahyad (built binary), lets it LAZILY spawn the REAL
// mlx/embed/server.py on the first search that needs the vector leg (no
// stub/mock anywhere in this file), indexes an English fixture note plus
// several Turkish decoys (see mlxFixtures), and proves the vector leg is
// what makes a Turkish query about the ENGLISH note's topic actually rank
// it first: the SAME corpus + query against an FTS-only fusion config
// does NOT rank it first (asserted directly against the same on-disk
// brain.db) - only the vec leg's semantic understanding does.
package mlxe2e

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/search"
)

// mlxCrossLingualQuery is the task spec step 7's exact query. Its literal
// token "saga" DOES coincidentally overlap with the EN note's own text
// (both use the Latin loanword) - which, in a toy corpus of only the EN
// note plus one obviously-unrelated decoy, would let the trigram leg find
// the EN note trivially via that shared substring alone, without the vec
// leg contributing anything (confirmed empirically while building this
// test). mlxFixtures below adds several MORE Turkish notes that also
// happen to mention "saga" (a family saga, a TV saga, a game's "saga
// mode", ...) - realistic background clutter, not a rigged corpus - so
// the trigram leg's bm25 has real competition for that token and no
// longer trivially singles out the EN note; only the vec leg's actual
// semantic understanding still does (verified below via both fusion
// configs against the exact same corpus).
const mlxCrossLingualQuery = "altın projesinde saga nasıl kurulmuştu?"

// mlxEnNoteRelPath is asserted as the top hit below; it must match
// mlxFixtures[0].path.
const mlxEnNoteRelPath = "gold-token-en.md"

var mlxFixtures = []struct{ path, text string }{
	{mlxEnNoteRelPath, "The gold-token backend uses NATS JetStream for saga orchestration."},
	// Topically unrelated, no lexical overlap with the query at all - the
	// baseline "obviously irrelevant" distractor.
	{"ev-bakma-tr.md", "Kadıköy'de yeni bir ev bakıyoruz; salonu genişçe, mutfağı ayrı bir daire istiyoruz."},
	// The remaining four all mention "saga" in an unrelated context -
	// realistic competition for the trigram/unicode61 legs' bm25 on that
	// shared token, none of them anywhere near "gold-token backend" /
	// "NATS JetStream" / orchestration semantically.
	{"star-wars-saga-tr.md", "Star Wars saga'sını yeniden izlemeye başladık, çok eğlenceli geçiyor."},
	{"forsyte-saga-tr.md", "Forsyte Saga dizisini bitirdik, aile draması çok başarılıydı."},
	{"aile-saga-tr.md", "Büyükannemin anlattığı aile sagası nesiller boyu sürdü, herkes çok etkilendi."},
	{"oyun-saga-tr.md", "Yeni bir strateji oyunu aldık, saga modunda ilerliyoruz, çok keyifli."},
}

// TestCrossLingualVecLegFindsEnglishNote is the W12-11 step 7 live
// verification. It skips (rather than fails) when the prerequisite build
// artifacts are missing, mirroring TestW12Acceptance's own posture.
func TestCrossLingualVecLegFindsEnglishNote(t *testing.T) {
	root := mlxRepoRoot(t)

	kahyadBin := filepath.Join(root, "bin", "kahyad")
	embedPython := filepath.Join(root, "mlx", "embed", ".venv", "bin", "python")
	for _, p := range []string{kahyadBin, embedPython} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("W12-11 cross-lingual gate requires a built kahyad + mlx/embed venv "+
				"(run `make build` and set up mlx/embed/.venv per its README first): missing %s: %v", p, err)
		}
	}

	// A short, directly-under-os-tempdir path (NOT t.TempDir(), which
	// nests under this test's own long name) - macOS's AF_UNIX sun_path
	// has a ~104-byte limit, and homeDir/data/kahyad.sock under a
	// t.TempDir() here would blow well past it (confirmed empirically:
	// "bind: invalid argument").
	homeDir := mlxShortTempDir(t)
	dataDir := filepath.Join(homeDir, "data")
	memDir := filepath.Join(homeDir, "memory-fixture")
	sockPath := filepath.Join(dataDir, "kahyad.sock")
	logDir := filepath.Join(dataDir, "logs")

	if err := os.MkdirAll(memDir, 0o700); err != nil {
		t.Fatalf("mkdir memory fixture dir: %v", err)
	}
	for _, f := range mlxFixtures {
		if err := os.WriteFile(filepath.Join(memDir, f.path), []byte(f.text+"\n"), 0o600); err != nil {
			t.Fatalf("write fixture %s: %v", f.path, err)
		}
	}

	embedPort := mlxFreePort(t)
	mlxWriteConfigYAML(t, homeDir, embedPort)

	env := mlxBuildKahyadEnv(homeDir, dataDir, memDir, sockPath)

	cmd := mlxStartKahyad(t, kahyadBin, env, homeDir)
	t.Cleanup(func() { mlxStopProcess(cmd) })

	mlxWaitForHealth(t, sockPath, homeDir)

	client := mlxNewUDSClient(sockPath)
	if rr := mlxDoReindex(t, client); rr.FilesIndexed < len(mlxFixtures) {
		t.Fatalf("reindex: files_indexed = %d, want >= %d", rr.FilesIndexed, len(mlxFixtures))
	}

	// The FIRST search that needs the vector leg lazily spawns the real
	// embed service (kahyad/internal/mlxsup, event=mlx_spawn) and blocks
	// on it becoming healthy - this dev machine's own sandboxed tool
	// environment has been observed to add a large, highly variable
	// delay (anywhere from near-instant to ~35s) to a FRESH cross-process
	// 127.0.0.1 connection specifically (reproduced independently of this
	// codebase with plain curl/nc/python-socket against a bare
	// http.server - see kahyad/internal/mlxsup/supervisor_test.go's own
	// realConnectTimeout comment); a generous client timeout absorbs
	// that plus genuine first-model-load time.
	resp := mlxDoMemorySearch(t, client, mlxCrossLingualQuery, 3)
	if len(resp.Results) == 0 {
		t.Fatalf("memory_search(%q) returned no results\n%s", mlxCrossLingualQuery, mlxDumpKahyadLogs(homeDir))
	}
	if resp.Results[0].Path != mlxEnNoteRelPath {
		t.Fatalf("top hit = %+v, want %s ranked first via the vec leg\nfull results: %+v\n%s",
			resp.Results[0], mlxEnNoteRelPath, resp.Results, mlxDumpKahyadLogs(homeDir))
	}

	// Confirm the health check + lazy-spawn contract empirically (also
	// double as this task's own required "capture curl .../health output"
	// evidence, from inside the same process that just proved retrieval
	// works, rather than a separate manual step).
	healthResp, err := client.Get("http://kahyad/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer healthResp.Body.Close()
	healthBody, _ := io.ReadAll(healthResp.Body)
	t.Logf("kahyad /health after cross-lingual search: %s", healthBody)
	var healthDecoded struct {
		Embed string `json:"embed"`
	}
	if err := json.Unmarshal(healthBody, &healthDecoded); err != nil {
		t.Fatalf("decode /health: %v: %s", err, healthBody)
	}
	if healthDecoded.Embed != "ok" {
		t.Errorf(`/health "embed" = %q, want "ok" after a successful vec-leg search`, healthDecoded.Embed)
	}

	directEmbedHealth := mlxCurlEmbedHealth(t, embedPort)
	t.Logf("embed service GET /health directly: %s", directEmbedHealth)

	// event=search_degraded_no_vec must never have fired, and vec_hits > 0
	// must appear at least once - both corroborate the health-endpoint
	// check above from the daemon's own JSONL logs.
	kahyadLog := filepath.Join(logDir, "kahyad.jsonl")
	logData, err := os.ReadFile(kahyadLog)
	if err != nil {
		t.Fatalf("read %s: %v", kahyadLog, err)
	}
	sawVecHit := false
	for _, line := range strings.Split(string(logData), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		if m["event"] == "search_degraded_no_vec" {
			t.Errorf("saw event=search_degraded_no_vec - the vec leg was expected to be healthy for this run: %v", m)
		}
		if m["event"] == "memory_search" {
			if vh, ok := m["vec_hits"].(float64); ok && vh > 0 {
				sawVecHit = true
			}
		}
	}
	if !sawVecHit {
		t.Errorf("no memory_search JSONL line reports vec_hits > 0 - cannot confirm the vec leg actually contributed\n%s", mlxDumpKahyadLogs(homeDir))
	}

	// FTS-only proof (task spec step 7: "FTS legs alone fail this; assert
	// the vec leg contributed by also asserting FTS-only search misses
	// it"): open the SAME brain.db kahyad just populated and search it
	// directly with search.FTSOnlyConfig() - the exact weights Search
	// itself automatically falls back to whenever the vec leg is
	// unavailable (kahyad/internal/search.Searcher.Search's own degraded
	// path) - with no embedder wired at all, so this leg is guaranteed
	// pure-FTS. The EN note must NOT be the top hit here, in contrast to
	// the vec-leg-assisted result asserted above via the live server.
	ftsDB, err := sql.Open("sqlite3", filepath.Join(dataDir, "brain.db")+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open brain.db read-only: %v", err)
	}
	defer ftsDB.Close()
	ftsLog, err := logx.New(t.TempDir(), "test-mlx-fts-only-boot-000000")
	if err != nil {
		t.Fatalf("logx.New: %v", err)
	}
	defer ftsLog.Close()
	ftsOnlySearcher := search.New(ftsDB, ftsLog, search.FTSOnlyConfig())
	ftsHits, err := ftsOnlySearcher.Search(context.Background(), "", mlxCrossLingualQuery, 3)
	if err != nil {
		t.Fatalf("FTS-only Search: %v", err)
	}
	if len(ftsHits) > 0 && ftsHits[0].Path == mlxEnNoteRelPath {
		t.Errorf("FTS-only fusion ALSO ranked %s first for %q (hits=%+v) - this query does not actually "+
			"distinguish the vec leg's contribution; pick a query/fixture pair with less lexical overlap",
			mlxEnNoteRelPath, mlxCrossLingualQuery, ftsHits)
	}
}

func mlxRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed - cannot locate repo root")
	}
	// file = <root>/kahyad/internal/mlxe2e/cross_lingual_test.go
	return filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(file))))
}

// mlxShortTempDir returns a fresh temp dir created directly under the OS
// temp root (os.MkdirTemp("", ...)) rather than t.TempDir() (which nests
// under a path including this test's own, fairly long, name) - matching
// kahyad/internal/server/server_test.go's shortSocketDir helper and for
// the exact same reason: macOS's AF_UNIX sun_path has a ~104-byte limit,
// easily blown past once kahyad.sock's full path includes the test name.
func mlxShortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "kmlx")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func mlxFreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func mlxWriteConfigYAML(t *testing.T, homeDir string, embedPort int) {
	t.Helper()
	dir := filepath.Join(homeDir, "Library", "Application Support", "Kahya")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	content := fmt.Sprintf("embed_port: %d\ntask_timeout_min: 2\n", embedPort)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(content), 0o600); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}
}

func mlxBuildKahyadEnv(homeDir, dataDir, memDir, sockPath string) []string {
	strip := map[string]bool{
		"HOME": true, "KAHYA_DATA_DIR": true, "KAHYA_MEMORY_DIR": true,
		"KAHYA_SOCKET": true, "KAHYA_DB_PATH": true, "KAHYA_ENV": true,
		"KAHYA_LOG_LEVEL": true, "HF_HOME": true,
	}
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strip[k] {
			continue
		}
		out = append(out, kv)
	}
	// HF_HOME pins the REAL Hugging Face cache (W0-03's download target),
	// independent of the isolated/fake HOME below: mlx/embed/server.py
	// (spawned by kahyad as a CHILD, inheriting kahyad's own environment
	// verbatim - kahyad/internal/mlxsup.spawnLocked) resolves the pinned
	// Qwen3-Embedding-0.6B snapshot via huggingface_hub's normal HOME-
	// derived cache path; without this override it would look under the
	// FAKE homeDir's (empty) cache instead and fail closed with
	// local_files_only=True - a test-harness-only concern, since in real
	// production kahyad and its embed child both run under the actual
	// user's real HOME already.
	realHome, err := os.UserHomeDir()
	if err != nil {
		panic(fmt.Sprintf("mlxBuildKahyadEnv: os.UserHomeDir: %v", err))
	}
	return append(out,
		"HOME="+homeDir,
		"HF_HOME="+filepath.Join(realHome, ".cache", "huggingface"),
		"KAHYA_DATA_DIR="+dataDir,
		"KAHYA_MEMORY_DIR="+memDir,
		"KAHYA_SOCKET="+sockPath,
		"KAHYA_ENV=dev",
		"KAHYA_LOG_LEVEL=info",
		"PYTHONUTF8=1",
	)
}

func mlxStartKahyad(t *testing.T, bin string, env []string, homeDir string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(bin)
	cmd.Env = env
	outPath := filepath.Join(homeDir, fmt.Sprintf("kahyad.stdout.%d.log", time.Now().UnixNano()))
	errPath := filepath.Join(homeDir, fmt.Sprintf("kahyad.stderr.%d.log", time.Now().UnixNano()))
	outFile, err := os.Create(outPath)
	if err != nil {
		t.Fatalf("create %s: %v", outPath, err)
	}
	errFile, err := os.Create(errPath)
	if err != nil {
		t.Fatalf("create %s: %v", errPath, err)
	}
	cmd.Stdout = outFile
	cmd.Stderr = errFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("start kahyad (%s): %v", bin, err)
	}
	t.Logf("kahyad started pid=%d stdout=%s stderr=%s", cmd.Process.Pid, outPath, errPath)
	return cmd
}

func mlxStopProcess(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		_, _ = cmd.Process.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
}

func mlxWaitForHealth(t *testing.T, sockPath, homeDir string) {
	t.Helper()
	client := mlxNewUDSClient(sockPath)
	deadline := time.Now().Add(15 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, "http://kahyad/health", nil)
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("kahyad never became healthy at %s: %v\n%s", sockPath, lastErr, mlxDumpKahyadLogs(homeDir))
}

func mlxDumpKahyadLogs(homeDir string) string {
	entries, err := os.ReadDir(homeDir)
	if err != nil {
		return ""
	}
	var sb strings.Builder
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "kahyad.") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(homeDir, name))
		if err != nil {
			continue
		}
		fmt.Fprintf(&sb, "--- %s ---\n%s\n", name, b)
	}
	logDir := filepath.Join(homeDir, "data", "logs", "kahyad.jsonl")
	if b, err := os.ReadFile(logDir); err == nil {
		fmt.Fprintf(&sb, "--- kahyad.jsonl ---\n%s\n", b)
	}
	return sb.String()
}

// mlxNewUDSClient uses a generous timeout - see this file's own doc
// comment on the cross-process 127.0.0.1 connection delay observed in
// this dev environment specifically (kahyad's HTTP-over-UDS control
// socket itself is unaffected - only the embed service's separate TCP
// loopback listener is - but the SAME client is reused for both the UDS
// control-plane calls and reading kahyad's own /health, so its timeout is
// sized for the slower of the two paths).
func mlxNewUDSClient(sock string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				d := net.Dialer{Timeout: 2 * time.Second}
				return d.DialContext(ctx, "unix", sock)
			},
		},
		Timeout: 90 * time.Second,
	}
}

type mlxMemSearchResult struct {
	ChunkID    int64   `json:"chunk_id"`
	EpisodeID  int64   `json:"episode_id"`
	Path       string  `json:"path"`
	Text       string  `json:"text"`
	Score      float64 `json:"score"`
	SourceTier string  `json:"source_tier"`
}

type mlxMemSearchResponse struct {
	Results []mlxMemSearchResult `json:"results"`
}

func mlxDoMemorySearch(t *testing.T, client *http.Client, query string, k int) mlxMemSearchResponse {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"query": query, "k": k})
	req, _ := http.NewRequest(http.MethodPost, "http://kahyad/v1/memory/search", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/memory/search: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/memory/search: status %d: %s", resp.StatusCode, b)
	}
	var out mlxMemSearchResponse
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode /v1/memory/search response: %v: %s", err, b)
	}
	return out
}

type mlxReindexResp struct {
	FilesIndexed int `json:"files_indexed"`
	Chunks       int `json:"chunks"`
}

func mlxDoReindex(t *testing.T, client *http.Client) mlxReindexResp {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		body, _ := json.Marshal(map[string]any{"full": true})
		req, _ := http.NewRequest(http.MethodPost, "http://kahyad/v1/reindex", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST /v1/reindex: %v", err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusConflict && time.Now().Before(deadline) {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST /v1/reindex: status %d: %s", resp.StatusCode, b)
		}
		var out mlxReindexResp
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("decode /v1/reindex response: %v: %s", err, b)
		}
		return out
	}
}

// mlxCurlEmbedHealth GETs the embed service's OWN /health directly
// (127.0.0.1:embedPort, bypassing kahyad entirely) with a generous,
// retrying timeout - purely to capture the literal
// `curl 127.0.0.1:$KAHYA_EMBED_PORT/health` evidence this task asks for;
// mlxDoMemorySearch above already proves the same endpoint answers
// correctly via kahyad's own lazy-start + poll path.
func mlxCurlEmbedHealth(t *testing.T, port int) string {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	deadline := time.Now().Add(60 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return string(b)
		}
		lastErr = err
		time.Sleep(1 * time.Second)
	}
	t.Logf("direct embed /health check never succeeded (informational only): %v", lastErr)
	return ""
}
