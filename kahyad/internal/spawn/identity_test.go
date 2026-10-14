package spawn

import (
	"context"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// FIX-I-13: NewWorkerIdentity is what stands between a persisted
// tasks.worker_pgid and a SIGKILL of whatever process group now owns that
// number.

func TestWorkerIdentityMatchesOwnSpawnedScript(t *testing.T) {
	cfg := testConfig("testdata/hang_worker.py")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pidCh := make(chan int, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = Run(ctx, cfg, testEnvelope(t), Callbacks{OnStart: func(pid int) { pidCh <- pid }})
	}()
	pid := <-pidCh
	t.Cleanup(func() { cancel(); <-done })

	// The worker's own ps line can take a moment to show the exec'd argv.
	ok := false
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if NewWorkerIdentity(cfg.Cmd)(pid) {
			ok = true
			break
		}
	}
	if !ok {
		out, _ := psCommandLine(pid)
		t.Fatalf("NewWorkerIdentity(%q)(%d) = false for our own live worker (ps: %q)", cfg.Cmd, pid, out)
	}
	if NewWorkerIdentity([]string{"python3", "testdata/other_worker.py"})(pid) {
		t.Error("a different configured worker script must not match")
	}
	if NewWorkerIdentity([]string{"/usr/bin/env", "testdata/hang_worker.py"})(pid) {
		t.Error("a different configured executable must not match")
	}
}

func TestWorkerIdentityRejectsForeignProcess(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _ = cmd.Wait() })
	for _, workerCmd := range [][]string{
		{"python3", "testdata/hang_worker.py"},
		{"/repo/worker/.venv/bin/python", "-m", "kahya_worker"},
	} {
		if NewWorkerIdentity(workerCmd)(cmd.Process.Pid) {
			t.Errorf("NewWorkerIdentity(%q) matched an unrelated `sleep 30`", workerCmd)
		}
	}
}

func TestWorkerIdentityDeadPIDIsFalse(t *testing.T) {
	cmd := exec.Command("python3", "-c", "pass")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if NewWorkerIdentity([]string{"python3", "-c", "pass"})(cmd.Process.Pid) {
		t.Error("an exited (reaped) pid must never look like a live worker")
	}
	for _, pgid := range []int{0, 1, -5} {
		if NewWorkerIdentity([]string{"python3", "x.py"})(pgid) {
			t.Errorf("pgid %d must never match", pgid)
		}
	}
	if NewWorkerIdentity(nil)(1234) {
		t.Error("an empty worker command must never match")
	}
}

// TestCommandLooksLikeWorkerTable pins the ps-line matcher on the shapes
// seen in practice, including macOS's framework-Python argv[0] rewrite.
func TestCommandLooksLikeWorkerTable(t *testing.T) {
	const framework = "/opt/homebrew/Cellar/python@3.14/3.14.6/Frameworks/Python.framework/Versions/3.14/Resources/Python.app/Contents/MacOS/Python"
	realCmd := []string{"/Users/u/code/kahya/worker/.venv/bin/python", "-m", "kahya_worker"}
	cases := []struct {
		name      string
		workerCmd []string
		line      string
		want      bool
	}{
		{"venv python as is", realCmd, "/Users/u/code/kahya/worker/.venv/bin/python -m kahya_worker\n", true},
		{"macOS framework re-exec", realCmd, framework + " -m kahya_worker", true},
		{"repo path with spaces", []string{"/Users/u/My Code/kahya/worker/.venv/bin/python3.14", "-m", "kahya_worker"}, "/Users/u/My Code/kahya/worker/.venv/bin/python3.14 -m kahya_worker", true},
		{"test fixture script", []string{"python3", "testdata/hang_worker.py"}, framework + " testdata/hang_worker.py", true},
		{"different module", realCmd, framework + " -m kahya_worker_x", false},
		{"module only as an argument", realCmd, "/usr/bin/vim -m kahya_worker", false},
		{"marker glued to a longer arg", []string{"python3", "w.py"}, framework + " /tmp/evil-w.py", false},
		{"extra trailing args", realCmd, framework + " -m kahya_worker --other", false},
		{"other python program", realCmd, framework + " -c import time", false},
		{"unrelated binary", realCmd, "sleep 300", false},
		{"single-element workerCmd", []string{"/opt/kahya/bin/worker"}, "/opt/kahya/bin/worker", true},
		{"single-element mismatch", []string{"/opt/kahya/bin/worker"}, "/opt/kahya/bin/worker2 x", false},
		{"empty line", realCmd, "  \n", false},
	}
	for _, c := range cases {
		exe, marker := workerIdentityMarkers(c.workerCmd)
		if got := commandLooksLikeWorker(c.line, exe, marker); got != c.want {
			t.Errorf("%s: commandLooksLikeWorker(%q, %q, %q) = %v, want %v", c.name, c.line, exe, marker, got, c.want)
		}
	}
}
