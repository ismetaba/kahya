// osascript.go wires the W3-09 applescript_run/jxa_run/shortcuts_run MCP
// tool set (kahya/mcp/osascript) into this package — the SAME in-process
// pattern shell.go uses for mcp/shell (W3-04) and fs.go uses for mcp/fs
// (W3-03). mcp/osascript.PolicyClient/Ledger/Logger are type ALIASES of
// mcp/fs's own identically-named interfaces (mcp/osascript's package doc
// comment), so fs.go's enginePolicyClient/fsLoggerAdapter — already
// constructed via NewFSPolicyClient/NewFSLogger for the fs tool — satisfy
// mcp/osascript's dependencies directly, with ZERO new adapter code,
// exactly like shell.go's identical reuse.
package server

import (
	mcposascript "kahya/mcp/osascript"
)

// osascriptOwnedTools mirrors fsOwnedTools/shellOwnedTools (fs.go/
// shell.go): applescript_run/jxa_run/shortcuts_run each run their OWN
// full gate chain (mcp/osascript's own scan-before-approval + Check +
// ConsumeToken, in the fixed order mcp/osascript's package doc comment
// describes) instead of policyGateMiddleware's generic Check+auto-consume
// step.
var osascriptOwnedTools = map[string]bool{
	"applescript_run": true,
	"jxa_run":         true,
	"shortcuts_run":   true,
}

// SetOsascriptTool wires POST /v1/mcp's applescript_run/jxa_run/
// shortcuts_run tools (W3-09). Call before Prepare(); buildMCPHandler
// registers osascriptSrv's tools onto the SAME shared MCP server
// memory's/fs's/shell's own tools use whenever osascriptSrv is non-nil.
func (s *Server) SetOsascriptTool(osascriptSrv *mcposascript.Server) {
	s.osascriptServer = osascriptSrv
}
