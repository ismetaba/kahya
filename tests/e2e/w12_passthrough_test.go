//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"kahya/tests/e2e/mockanthropic"
)

// TestW12PassthroughCloudAuth is FIX-I-1's hermetic end-to-end proof of the
// OWNER-DECISION default credential mode (credential_mode: passthrough):
// the REAL worker -> claude-agent-sdk -> bundled claude CLI chain, spawned by
// a real kahyad whose per-task proxy forwards to the mock upstream.
//
// The worker CLI's "own login" is supplied through CLAUDE_CODE_OAUTH_TOKEN=
// hermetic-dummy (the CLI's documented OAuth-token env source; deliberately
// NOT in spawn's secretEnvDenylist, owner decision G11-1/G11-2) so the test
// never depends on - or touches - the developer's real Claude Code login.
// Before FIX-I-1 BuildEnv also set ANTHROPIC_API_KEY=<token>, the CLI chose
// that as its single credential source, and the proxy stripped it: every
// passthrough call reached the upstream with no credential at all.
//
// The CLI's other egress is pinned to a dead local proxy (HTTPS_PROXY/
// HTTP_PROXY=127.0.0.1:9, NO_PROXY=loopback) so a dummy-token OAuth side
// call could never leave the machine; the model call itself goes to the
// loopback per-task proxy and is unaffected.
func TestW12PassthroughCloudAuth(t *testing.T) {
	root := repoRoot(t)
	kahyadBin := filepath.Join(root, "bin", "kahyad")
	kahyaBin := filepath.Join(root, "bin", "kahya")
	kahyaMCPBin := filepath.Join(root, "bin", "kahya-mcp")
	venvPython := filepath.Join(root, "worker", ".venv", "bin", "python")
	for _, p := range []string{kahyadBin, kahyaBin, kahyaMCPBin, venvPython} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("passthrough gate requires built binaries + worker venv "+
				"(run `make build && make venv` first): missing %s: %v", p, err)
		}
	}

	mock := mockanthropic.New()
	t.Cleanup(mock.Close)

	homeDir := t.TempDir()
	dataDir := filepath.Join(homeDir, "data")
	memDir := filepath.Join(homeDir, "memory-fixture")
	logDir := filepath.Join(dataDir, "logs")
	dbPath := filepath.Join(dataDir, "brain.db")
	// macOS caps unix socket paths at ~104 bytes: a short dedicated dir.
	sockDir, err := os.MkdirTemp("", "k")
	if err != nil {
		t.Fatalf("mkdir socket dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sockPath := filepath.Join(sockDir, "k.sock")

	seedMemoryFixture(t, root, memDir)
	writeConfigYAML(t, homeDir, mock.URL(), "passthrough")
	policyPath := writeHermeticPolicyYAML(t, root, homeDir, mock.URL())

	kahyadEnv := buildKahyadEnv(homeDir, dataDir, memDir, sockPath, policyPath,
		"CLAUDE_CODE_OAUTH_TOKEN=hermetic-dummy",
		"HTTPS_PROXY=http://127.0.0.1:9",
		"HTTP_PROXY=http://127.0.0.1:9",
		"NO_PROXY=127.0.0.1,localhost",
	)
	cliEnv := buildCLIEnv(homeDir, sockPath)

	cmd := startKahyad(t, kahyadBin, kahyadEnv, homeDir)
	t.Cleanup(func() { stopProcess(cmd) })
	waitForHealth(t, sockPath, homeDir)

	stdout, stderr, exitCode := runCLI(t, kahyaBin, cliEnv, cliPrompt)
	if exitCode != 0 {
		t.Fatalf("kahya %q exited %d\n--- stdout ---\n%s\n--- stderr ---\n%s\n%s", cliPrompt, exitCode, stdout, stderr, dumpKahyadLogs(homeDir))
	}
	traceID := extractTraceID(t, stderr)
	if stdout != mockanthropic.AnswerText {
		t.Errorf("cli stdout = %q, want %q", stdout, mockanthropic.AnswerText)
	}

	t.Run("worker_own_credential_forwarded", func(t *testing.T) {
		var messages int
		for i, req := range mock.Requests() {
			if !strings.HasPrefix(req.Path, "/v1/messages") {
				continue
			}
			messages++
			if got := req.Header.Get("Authorization"); got != "Bearer hermetic-dummy" {
				t.Errorf("request %d (%s): upstream Authorization = %q, want the worker CLI's own credential forwarded unchanged", i, req.Path, redactCredential(got))
			}
			if got := req.Header.Get("x-api-key"); got != "" {
				t.Errorf("request %d (%s): upstream x-api-key = %q, want empty in passthrough", i, req.Path, redactCredential(got))
			}
		}
		if messages == 0 {
			t.Fatal("no /v1/messages request reached the mock upstream")
		}
	})

	t.Run("local_token_never_upstream", func(t *testing.T) {
		assertNoLocalTokenUpstream(t, mock.Requests())
	})

	t.Run("ledger", func(t *testing.T) {
		kinds := eventKindsForTrace(t, dbPath, traceID)
		for _, want := range []string{"model_call", "task_done"} {
			if !kinds[want] {
				t.Errorf("events has no kind=%s row for trace %s (kinds: %v)", want, traceID, kinds)
			}
		}
		for _, bad := range []string{"proxy_auth_reject", "proxy.no_upstream_credential", "task.failed"} {
			if kinds[bad] {
				t.Errorf("events has a kind=%s row for trace %s; passthrough must authenticate cleanly", bad, traceID)
			}
		}
	})

	t.Run("single_trace_id", func(t *testing.T) {
		if n := countTraceMatches(t, filepath.Join(logDir, "worker.jsonl"), traceID); n == 0 {
			t.Errorf("worker.jsonl has no line carrying trace_id=%s", traceID)
		}
	})
}

// redactCredential keeps assertion messages from echoing a credential value
// (only the hermetic dummy is ever expected here, but a regression could
// surface something else).
func redactCredential(v string) string {
	if v == "" || v == "Bearer hermetic-dummy" || v == "hermetic-dummy" {
		return v
	}
	return "<redacted len=" + strconv.Itoa(len(v)) + ">"
}
