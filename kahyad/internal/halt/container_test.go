// container_test.go: LIVE Docker daemon test (W6-03 acceptance criterion:
// "docker run -d --label kahya.task_id=<id> alpine sleep 300 registered
// to a fake task -> halt -> docker ps filter empty"). Gated behind
// KAHYA_DOCKER_TESTS=1 - the Makefile's own `test` target exports this
// iff `docker info` succeeds at the time it runs (mcp/shell/
// container_test.go's own doc comment, mirrored here): unset, this test
// is SKIPPED (the suite stays green with no daemon present); set, it runs
// for REAL and FAILS (never skips) on any problem.
package halt

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	mcpshell "kahya/mcp/shell"

	"kahya/kahyad/internal/task"
)

func requireDockerTests(t *testing.T) {
	t.Helper()
	if os.Getenv("KAHYA_DOCKER_TESTS") != "1" {
		t.Skip("KAHYA_DOCKER_TESTS not set — docker daemon not confirmed up; see docker/README.md")
	}
}

func randHexSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return hex.EncodeToString(b)
}

func TestHaltTaskKillsLabeledDockerContainer(t *testing.T) {
	requireDockerTests(t)

	taskID := "halt-container-" + randHexSuffix(t)
	out, err := exec.Command("docker", "run", "-d", "--label", "kahya.task_id="+taskID, "alpine", "sleep", "300").CombinedOutput()
	if err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	containerID := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", containerID).Run() })

	// Sanity: the container is actually running before we ever call halt.
	if psOut, _ := exec.Command("docker", "ps", "-q", "--filter", "label=kahya.task_id="+taskID).CombinedOutput(); strings.TrimSpace(string(psOut)) == "" {
		t.Fatal("container did not start (docker ps filter found nothing before halt)")
	}

	st := testStore(t)
	insertExecutingTask(t, st, taskID)

	machine := task.NewMachine(st.Queries, st)
	live := task.NewLiveRegistry()
	// A bare NewRunner (no fs policy/ledger/log needed - KillLabeled only
	// ever touches Exec/Health, both of which NewRunner always wires to
	// the REAL docker CLI) satisfies halt.ContainerKiller directly.
	runner := mcpshell.NewRunner("", "", "", nil, nil, nil, nil, nil)
	ex := NewExecutor(st.Queries, machine, live, nil, runner, st)

	haltedNow, err := ex.HaltTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("HaltTask() error = %v", err)
	}
	if !haltedNow {
		t.Fatal("HaltTask() haltedNow = false, want true")
	}

	// docker ps -q --filter label=kahya.task_id=<id> is empty - poll
	// briefly (docker kill is near-instant but not provably synchronous
	// with `docker ps` observing it).
	deadline := time.Now().Add(5 * time.Second)
	for {
		psOut, _ := exec.Command("docker", "ps", "-q", "--filter", "label=kahya.task_id="+taskID).CombinedOutput()
		if strings.TrimSpace(string(psOut)) == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("container still running after halt: %s", psOut)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
