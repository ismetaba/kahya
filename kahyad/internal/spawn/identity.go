package spawn

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// psTimeout bounds one `ps` identity probe; on expiry the probe reports
// "not our worker" (fail-closed: never kill on doubt).
const psTimeout = 2 * time.Second

// NewWorkerIdentity returns a predicate reporting whether process group
// pgid's LEADER still looks like a worker this daemon spawned from
// workerCmd (FIX-I-13). spawn.Run starts every worker as its own session and
// group leader (Setsid), so pid == pgid and the leader IS the worker
// process. The halt executor consults it before SIGKILLing a pgid it read
// back from tasks.worker_pgid (no in-memory LiveRegistry entry - a daemon
// restart), and the boot orphan sweep does the same: a recorded pgid is just
// a number once its worker exited, and the kernel hands pid numbers out
// again.
//
// The probe is `ps -ww -o command= -p <pgid>` (POSIX ps; macOS and Linux
// both print the full argv for these flags). The line must END with the
// worker's own arguments exactly as configured - workerCmd[1:] joined by
// spaces ("-m kahya_worker" for the real worker, the script path for the
// test fixtures; the executable path itself for a one-element workerCmd) -
// and the text before them must name the executable. The name is compared
// case-insensitively with any trailing version digits dropped: a Homebrew
// framework Python re-execs itself as .../Python.app/Contents/MacOS/Python
// (argv[1:] untouched), so a worker started as worker/.venv/bin/python3.14
// shows up under "Python".
//
// Fail-closed: any ps error, timeout, empty output, exited process or
// mismatch reports false - the caller then clears the stale record and
// never kills.
func NewWorkerIdentity(workerCmd []string) func(pgid int) bool {
	exeName, marker := workerIdentityMarkers(workerCmd)
	return func(pgid int) bool {
		if pgid <= 1 || exeName == "" || marker == "" {
			return false
		}
		out, err := psCommandLine(pgid)
		if err != nil {
			return false
		}
		return commandLooksLikeWorker(out, exeName, marker)
	}
}

// workerIdentityMarkers derives NewWorkerIdentity's two match keys from
// workerCmd: the normalized executable name and the argv tail the command
// line must end with.
func workerIdentityMarkers(workerCmd []string) (exeName, marker string) {
	if len(workerCmd) == 0 || workerCmd[0] == "" {
		return "", ""
	}
	exeName = normalizeExeName(workerCmd[0])
	if len(workerCmd) == 1 {
		return exeName, workerCmd[0]
	}
	return exeName, strings.Join(workerCmd[1:], " ")
}

// normalizeExeName lowercases path's base name and drops a trailing
// version suffix ("python3.14" -> "python"); a name that is nothing but
// version characters is kept as is.
func normalizeExeName(path string) string {
	base := strings.ToLower(filepath.Base(path))
	trimmed := strings.TrimRight(base, "0123456789.")
	if trimmed == "" {
		return base
	}
	return trimmed
}

// commandLooksLikeWorker reports whether one `ps -o command=` line belongs
// to the worker: it ends with marker (the configured argv tail), and the
// text before it - the executable path, spaces and all - names exeName. A
// line that is exactly marker is a one-element workerCmd's executable.
func commandLooksLikeWorker(cmdline, exeName, marker string) bool {
	line := strings.TrimSpace(cmdline)
	if line == "" || marker == "" || exeName == "" || !strings.HasSuffix(line, marker) {
		return false
	}
	prefix := line[:len(line)-len(marker)]
	exe := marker
	if prefix != "" {
		if !strings.HasSuffix(prefix, " ") {
			return false // marker is only the tail of a longer argument
		}
		exe = strings.TrimSpace(prefix)
	}
	return exe != "" && normalizeExeName(exe) == exeName
}

// psCommandLine returns pid's full command line via ps.
func psCommandLine(pid int) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), psTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, psPath(), "-ww", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// psPath prefers the system ps by absolute path (never a PATH lookup a
// hostile environment could redirect), falling back to "ps".
func psPath() string {
	for _, p := range []string{"/bin/ps", "/usr/bin/ps"} {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return "ps"
}
