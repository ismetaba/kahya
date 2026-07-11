package shell

import (
	"fmt"
	"os"
	"testing"
)

// TestMain holds LockDockerSharedResources for this package's ENTIRE test
// run whenever KAHYA_DOCKER_TESTS=1 (a no-op, un-locked m.Run() otherwise -
// every individual live Docker test still skips itself via
// requireDockerTests when the var is unset).
//
// Per-test locking (acquire/release inside requireDockerTests) turned out
// to be insufficient: it correctly serializes two tests that touch the
// SAME fixed-name Docker resource (kahya-egress-fwd/kahya-egress) against
// each other, but `go test ./...` still runs this package's own test
// BINARY concurrently with tests/w3's (a completely separate binary) by
// default, and general Docker-daemon/CPU contention from tests/w3's own
// Docker work was enough to occasionally perturb THIS package's
// container-timing assertions (TestLive_TimeoutKillsRealContainer) even
// though that test never touches the shared egress network/sidecar at
// all. Holding the lock for the whole package run - not just the one
// Docker-heavy test - removes that contention source entirely; the
// non-Docker tests in this package are fast, so the extra serialization
// cost is small. tests/w3 holds the exact same lock (its own TestMain) for
// the identical reason, so the two packages' live-Docker work never
// overlaps in wall-clock time, regardless of `go test`'s own package-level
// scheduling.
func TestMain(m *testing.M) {
	var release func()
	if os.Getenv("KAHYA_DOCKER_TESTS") == "1" {
		r, err := LockDockerSharedResources()
		if err != nil {
			fmt.Fprintln(os.Stderr, "mcp/shell TestMain: LockDockerSharedResources:", err)
			os.Exit(1)
		}
		release = r
	}
	code := m.Run()
	if release != nil {
		release()
	}
	os.Exit(code)
}
