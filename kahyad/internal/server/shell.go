// shell.go wires the W3-04 shell_docker/shell_host MCP tool set
// (kahya/mcp/shell) into this package — the SAME in-process pattern fs.go
// uses for mcp/fs (W3-03). mcp/shell.PolicyClient/Ledger/Logger are type
// ALIASES of mcp/fs's own identically-named interfaces (mcp/shell's
// package doc comment), so fs.go's enginePolicyClient/fsLoggerAdapter —
// already constructed via NewFSPolicyClient/NewFSLogger for the fs tool —
// satisfy mcp/shell's dependencies directly, with ZERO new adapter code:
// exactly the "a LATER out-of-process tool (W3-04's shell tool) can
// satisfy the exact same interface" seam mcp/fs's own doc comment
// anticipated, realized here as reuse of the SAME interface value rather
// than a new implementation of it.
package server

import (
	mcpshell "kahya/mcp/shell"
)

// shellOwnedTools mirrors fsOwnedTools (fs.go): shell_docker/shell_host
// each run their OWN full gate chain (mcp/shell's own mechanical-checks-
// before-approval + Check + ConsumeToken, in the fixed order mcp/shell's
// package doc comment describes) instead of policyGateMiddleware's
// generic Check+auto-consume step.
var shellOwnedTools = map[string]bool{
	"shell_docker": true,
	"shell_host":   true,
}

// SetShellTool wires POST /v1/mcp's shell_docker/shell_host tools (W3-04).
// Call before Prepare(); buildMCPHandler registers shellSrv's tools onto
// the SAME shared MCP server memory's/fs's tools use whenever shellSrv is
// non-nil. Server.Shutdown calls shellSrv.Shutdown to kill every
// kahya.task_id-labeled container this daemon may have left running.
func (s *Server) SetShellTool(shellSrv *mcpshell.Server) {
	s.shellServer = shellSrv
}
