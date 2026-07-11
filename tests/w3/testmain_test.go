package w3gate

import (
	"fmt"
	"os"
	"testing"

	mcpshell "kahya/mcp/shell"
)

// TestMain holds mcp/shell.LockDockerSharedResources for this package's
// ENTIRE test run whenever KAHYA_DOCKER_TESTS=1 - see mcp/shell/
// testmain_test.go's own doc comment for the full rationale (that package
// and this one both hold the identical cross-process lock for their whole
// respective test runs, not just around Gate 4, so their live-Docker work
// never overlaps regardless of `go test ./...`'s own package-level
// scheduling). A no-op, un-locked m.Run() when the var is unset - Gate 4
// still skips itself via requireDockerTests in that case, and Gates 1/2/3/5
// never touch Docker at all.
func TestMain(m *testing.M) {
	var release func()
	if os.Getenv("KAHYA_DOCKER_TESTS") == "1" {
		r, err := mcpshell.LockDockerSharedResources()
		if err != nil {
			fmt.Fprintln(os.Stderr, "tests/w3 TestMain: LockDockerSharedResources:", err)
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
