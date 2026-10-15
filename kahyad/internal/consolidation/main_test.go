package consolidation

import (
	"os"
	"testing"
)

// TestMain makes every git invocation in this package's tests hermetic
// (same shape as kahyad/internal/anchor's): the runGit helper and the
// production backup.NewExecGitRunner both build the child environment from
// os.Environ(), so pinning it here covers both. Without this the host's
// system/global git config leaks in - e.g. a developer's core.quotePath=false
// would hide the FIX-P-2 C-quoting bug, Xcode's system gitconfig sets
// init.defaultBranch=main, and ~/.gitconfig supplies a commit identity that
// masks a missing one. Every repo here lives under t.TempDir(); nothing
// reaches the network.
func TestMain(m *testing.M) {
	for _, kv := range [][2]string{
		{"GIT_CONFIG_NOSYSTEM", "1"},
		{"GIT_CONFIG_GLOBAL", "/dev/null"},
		{"GIT_TERMINAL_PROMPT", "0"},
	} {
		if err := os.Setenv(kv[0], kv[1]); err != nil {
			panic(err)
		}
	}
	for _, k := range []string{
		"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR", "GIT_CONFIG",
		"GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS", "GIT_SSH", "GIT_SSH_COMMAND",
		"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL",
	} {
		if err := os.Unsetenv(k); err != nil {
			panic(err)
		}
	}
	os.Exit(m.Run())
}
