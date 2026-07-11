// server.go mounts applescript_run/jxa_run/shortcuts_run onto a shared
// *mcp.Server — the exact same kahyad-owned, in-process pattern mcp/fs.
// Server/mcp/shell.Server use (RegisterTools onto an EXISTING server so
// kahyad can combine this package's tools with mcp/memory's/mcp/fs's/
// mcp/shell's on ONE shared MCP session, kahyad/internal/server/mcp.go).
package osascript

import (
	"context"
	"os/exec"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// traceHeader/taskHeader mirror mcp/fs's/mcp/shell's identical
// unexported constants — the HTTP headers kahyad's /v1/mcp wiring
// propagates a request's trace_id/task_id under (kept in sync by hand
// across packages that intentionally don't share code across the
// internal boundary).
const (
	traceHeader = "X-Kahya-Trace-Id"
	taskHeader  = "X-Kahya-Task-Id"
)

// Server implements applescript_run/jxa_run/shortcuts_run: a thin
// MCP-transport wrapper around Runner, which does ALL of the actual
// gate-chain/policy/ledger work — this file only adapts the MCP wire
// transport onto it (mirrors mcp/fs.Server's/mcp/shell.Server's own
// role).
type Server struct {
	Runner *Runner

	// LookPath resolves a command name to its full path (defaults to
	// exec.LookPath) — overridable in tests so "shortcuts CLI absent" is
	// unit-testable without mutating the real PATH. This task's own spec,
	// verbatim: "check `command -v shortcuts` at registration; disable
	// shortcuts_run cleanly if absent".
	LookPath func(string) (string, error)
}

// New constructs a Server from an already-configured Runner (kahyad's
// wiring — kahyad/internal/server/osascript.go — builds it via NewRunner
// and passes it here).
func New(runner *Runner) *Server {
	return &Server{Runner: runner, LookPath: exec.LookPath}
}

// shortcutsAvailable reports whether the `shortcuts` CLI is on PATH
// (macOS 12+ ships it; this task's spec: "check command -v shortcuts at
// registration").
func (s *Server) shortcutsAvailable() bool {
	lookup := s.LookPath
	if lookup == nil {
		lookup = exec.LookPath
	}
	_, err := lookup("shortcuts")
	return err == nil
}

// RegisterTools adds applescript_run/jxa_run onto srv unconditionally,
// and shortcuts_run only when the `shortcuts` CLI is actually present
// (shortcutsAvailable) — an EXISTING *mcp.Server, so kahyad can combine
// them with mcp/memory's/mcp/fs's/mcp/shell's own tools on ONE shared MCP
// server/session (kahyad/internal/server/mcp.go's buildMCPHandler). Use
// MCPServer below when a standalone server is wanted instead (tests).
func (s *Server) RegisterTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "applescript_run",
		Description: "AppleScript gövdesini onaylanmış WYSIWYE bayt-tam diff'i sonrası çalıştırır (osascript, STDIN üzerinden).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args ScriptInput) (*mcp.CallToolResult, ScriptOutput, error) {
		out, err := s.Runner.RunApplescript(ctx, traceIDFromRequest(req), taskIDFromRequest(req), args)
		return nil, out, err
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "jxa_run",
		Description: "JavaScript for Automation (JXA) gövdesini onaylanmış WYSIWYE bayt-tam diff'i sonrası çalıştırır (osascript -l JavaScript, STDIN üzerinden).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args ScriptInput) (*mcp.CallToolResult, ScriptOutput, error) {
		out, err := s.Runner.RunJXA(ctx, traceIDFromRequest(req), taskIDFromRequest(req), args)
		return nil, out, err
	})

	if !s.shortcutsAvailable() {
		if s.Runner != nil && s.Runner.Log != nil {
			s.Runner.Log.Warn("shortcuts_run_disabled", "reason", "shortcuts CLI not found on PATH")
		}
		return
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "shortcuts_run",
		Description: "VAR OLAN, isimli bir Shortcut'ı çalıştırır (shortcuts run <name> [--input-path <file>]); yeni shortcut oluşturmaz.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args ShortcutInput) (*mcp.CallToolResult, ShortcutOutput, error) {
		out, err := s.Runner.RunShortcut(ctx, traceIDFromRequest(req), taskIDFromRequest(req), args)
		return nil, out, err
	})
}

// MCPServer builds a fresh standalone *mcp.Server with every available
// tool registered (test convenience — kahyad's real wiring uses
// RegisterTools on its ALREADY-shared server instead, see this file's doc
// comment).
func (s *Server) MCPServer(impl *mcp.Implementation) *mcp.Server {
	if impl == nil {
		impl = &mcp.Implementation{Name: "kahya-osascript", Version: "0.1.0"}
	}
	srv := mcp.NewServer(impl, nil)
	s.RegisterTools(srv)
	return srv
}

// traceIDFromRequest/taskIDFromRequest extract the propagated headers
// from an incoming CallToolRequest (mirrors mcp/fs's/mcp/shell's
// identical helpers). Both return "" when unavailable (req.Extra nil,
// e.g. a direct Go-level test call never routed through a real HTTP
// request).
func traceIDFromRequest(req *mcp.CallToolRequest) string {
	if req == nil || req.Extra == nil || req.Extra.Header == nil {
		return ""
	}
	return req.Extra.Header.Get(traceHeader)
}

func taskIDFromRequest(req *mcp.CallToolRequest) string {
	if req == nil || req.Extra == nil || req.Extra.Header == nil {
		return ""
	}
	return req.Extra.Header.Get(taskHeader)
}
