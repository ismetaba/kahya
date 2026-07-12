//go:build acceptance

package w4gate

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

// msgCloudParked must stay byte-exact with kahyad/internal/task.MsgCloudParked
// (cloudretry.go) - this package cannot import kahyad/internal/task (Go's
// internal-package import boundary), so the literal is duplicated here, the
// same way every other tests/w3-/tests/e2e-style gate test in this codebase
// duplicates a Turkish string it needs to assert against byte-exactly.
const msgCloudParked = "Bulut servisine ulaşılamıyor; görev bekliyor-yeniden-deneme durumunda. Ağ dönünce otomatik devam edecek."

// msgCloudGiveUpFmt mirrors kahyad/internal/task.MsgCloudGiveUpFmt exactly
// (same byte-exact-duplication rationale as msgCloudParked above). Its one
// substitution is the task_id (cloudretry.go's own giveUp doc comment: "no
// prompt excerpt is available at this layer").
const msgCloudGiveUpFmt = "Yeniden deneme süresi doldu (24 sa). Görev kapatıldı: %s."

const statusRetryWait = "bekliyor-yeniden-deneme"

// TestScenarioB_OfflineThenReconnectCompletes is HANDOFF §6 W4's second
// gate clause's happy leg: a command issued while the cloud is unreachable
// parks in bekliyor-yeniden-deneme with the exact parked notification, then
// completes once the network is back (task spec step 4).
func TestScenarioB_OfflineThenReconnectCompletes(t *testing.T) {
	workerScript := filepath.Join(fixturesDir(t), "cloud_worker.py")
	pythonBin := findPython3(t)

	// port is reserved (freed immediately) but nothing listens on it yet -
	// the SAME failure shape as a real blackhole (connection refused). Only
	// later, once the task has genuinely parked, does this test bind a real
	// "network is back" HTTP server to this EXACT port - avoiding a kahyad
	// restart entirely (config.AnthropicUpstreamURL is resolved once at
	// boot, so a literal upstream-address swap is not an option here).
	port := reservePort(t)
	upstreamURL := fmt.Sprintf("http://127.0.0.1:%d", port)

	d := bootKahyad(t, daemonOpts{
		workerCmd:              []string{pythonBin, workerScript},
		anthropicUpstreamURL:   upstreamURL,
		cloudRetryMaxInline:    1,
		cloudRetryTaskSchedule: []string{"1s"},
		cloudRetryGiveUpAfter:  "60s", // never fires during this happy-path test
	})

	traceID := newTraceID()
	resp := d.postTask(t, traceID, "w4-07 scenario B probe (offline)")
	drainSSEAsync(resp)

	db := d.openDB(t)
	taskID := waitForTaskID(t, db, traceID, 10*time.Second)

	// Parked: task reaches bekliyor-yeniden-deneme...
	waitForTaskStatus(t, d, taskID, 15*time.Second, statusRetryWait)
	// ...and the exact parked Turkish notification event exists.
	if !waitForEvent(t, db, traceID, "task.waiting_retry", 2*time.Second) {
		t.Fatalf("no task.waiting_retry event for trace_id=%s", traceID)
	}
	requireEventMessage(t, db, traceID, "task.waiting_retry", msgCloudParked)

	// Network back: bind a real "healthy upstream" HTTP server to the exact
	// SAME port the blackhole phase left unbound.
	healthy := startFakeHealthyUpstream(t, port)
	defer healthy.Close()

	final := waitForTaskStatus(t, d, taskID, 15*time.Second, "done", "failed")
	if final.Status != "done" {
		t.Fatalf("task %s ended in status=%q after reconnect, want done\n%s", taskID, final.Status, dumpLogs(d.dirs.homeDir))
	}

	// events WHERE trace_id=? ORDER BY id - the task spec's own evidence
	// query (acceptance criteria: "task.waiting_retry with the exact parked
	// string, then task.done after upstream restore").
	kinds := eventKindsForTrace(t, db, traceID)
	mustContain(t, kinds, "task.waiting_retry")
}

// TestScenarioB_GiveUpAfterExceeded is the task spec's own "(CI test only)"
// failure leg: blackhole + a tiny give_up_after => 'failed' + the exact
// give-up Turkish string, once retrying has cumulatively exceeded it.
func TestScenarioB_GiveUpAfterExceeded(t *testing.T) {
	workerScript := filepath.Join(fixturesDir(t), "cloud_worker.py")
	pythonBin := findPython3(t)

	port := reservePort(t) // stays unbound for this test's whole lifetime - a permanent blackhole
	upstreamURL := fmt.Sprintf("http://127.0.0.1:%d", port)

	d := bootKahyad(t, daemonOpts{
		workerCmd:              []string{pythonBin, workerScript},
		anthropicUpstreamURL:   upstreamURL,
		cloudRetryMaxInline:    1,
		cloudRetryTaskSchedule: []string{"1s"},
		// give_up_after is deliberately several retry cycles out (not "1s",
		// right at the very first exhaustion's own edge) - a give_up_after
		// so tight that ordinary scheduling jitter (this whole acceptance
		// package's own goroutines/system load) could make the FIRST
		// exhaustion already exceed it was observed to flake exactly this
		// way once while hardening this test: the task then skips
		// bekliyor-yeniden-deneme entirely and goes straight to failed,
		// failing the first waitForTaskStatus below. A few seconds of
		// margin (4s, ~4 retry cycles at the 1s schedule) reliably
		// exercises the intended "parks at least once, THEN gives up"
		// sequence instead.
		cloudRetryGiveUpAfter: "4s",
	})

	traceID := newTraceID()
	resp := d.postTask(t, traceID, "w4-07 scenario B probe (give up)")
	drainSSEAsync(resp)

	db := d.openDB(t)
	taskID := waitForTaskID(t, db, traceID, 10*time.Second)

	// First exhaustion parks (created_at is still fresh); a later
	// redispatch cycle eventually exceeds give_up_after (measured
	// cumulatively from created_at) and gives up instead.
	waitForTaskStatus(t, d, taskID, 15*time.Second, statusRetryWait)

	final := waitForTaskStatus(t, d, taskID, 20*time.Second, "failed", "done")
	if final.Status != "failed" {
		t.Fatalf("task %s ended in status=%q, want failed (give-up)\n%s", taskID, final.Status, dumpLogs(d.dirs.homeDir))
	}

	wantMsg := fmt.Sprintf(msgCloudGiveUpFmt, taskID)
	requireEventMessage(t, db, traceID, "task.failed", wantMsg)
}

// requireEventMessage fails the test unless at least one events row
// (trace_id, kind) carries payload.message == want, byte-exact - waits up
// to 2s for it to appear (the ledger write and this poll are not
// synchronized with each other).
func requireEventMessage(t *testing.T, db *sql.DB, traceID, kind, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var lastSeen []string
	for time.Now().Before(deadline) {
		lastSeen = nil
		for _, raw := range eventPayloads(t, db, traceID, kind) {
			var m map[string]any
			if err := json.Unmarshal([]byte(raw), &m); err != nil {
				continue
			}
			msg, _ := m["message"].(string)
			lastSeen = append(lastSeen, msg)
			if msg == want {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("no events(trace_id=%s, kind=%s) row with message=%q byte-exact; saw messages=%v", traceID, kind, want, lastSeen)
}

// startFakeHealthyUpstream binds a plain net/http server to 127.0.0.1:port
// (a port the caller already knows is currently free) that answers every
// POST /v1/messages with a minimal 200 OK Anthropic-shaped body - the
// task spec's own "flip upstream to the fake healthy responder" step,
// standing in for W4-04's own Go test fixtures (this package cannot import
// kahyad/internal/anthproxy's test-only fixtures - they are _test.go files
// in a different, internal-boundary-protected package - so this is an
// independent, SAME-SHAPE stand-in, exactly like mcp/shell/
// egress_integration_test.go's own documented precedent for an identical
// cross-package constraint).
func startFakeHealthyUpstream(t *testing.T, port int) *http.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_fake","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-sonnet-5","usage":{"input_tokens":1,"output_tokens":1}}`))
	})
	srv := &http.Server{Handler: mux}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("bind fake healthy upstream to port %d: %v", port, err)
	}
	go func() { _ = srv.Serve(ln) }()
	return srv
}
