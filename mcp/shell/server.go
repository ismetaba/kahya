// server.go mounts shell_docker/shell_host onto a shared *mcp.Server —
// the exact same kahyad-owned, in-process pattern mcp/fs.Server uses
// (RegisterTools onto an EXISTING server so kahyad can combine this
// package's two tools with mcp/memory's and mcp/fs's on ONE shared MCP
// session, kahyad/internal/server/mcp.go).
package shell

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// traceHeader/taskHeader mirror mcp/fs's identical unexported constants —
// the HTTP headers kahyad's /v1/mcp wiring propagates a request's
// trace_id/task_id under (kept in sync by hand across packages that
// intentionally don't share code across the internal boundary).
const (
	traceHeader = "X-Kahya-Trace-Id"
	taskHeader  = "X-Kahya-Task-Id"
)

// Server implements shell_docker/shell_host: a thin MCP-transport wrapper
// around Runner (shell_docker's container lifecycle, runner.go) and
// HostExec (shell_host's narrow argv set, hostexec.go) — both of which do
// ALL of their own gate-chain/policy/ledger work; this file only adapts
// the MCP wire transport onto them (mirrors mcp/fs.Server's own role).
type Server struct {
	Runner   *Runner
	HostExec *HostExec
}

// New constructs a Server from an already-configured Runner/HostExec
// (kahyad's wiring — kahyad/internal/server/shell.go — builds both via
// NewRunner/NewHostExec and passes them here).
func New(runner *Runner, hostExec *HostExec) *Server {
	return &Server{Runner: runner, HostExec: hostExec}
}

// RegisterTools adds shell_docker/shell_host onto srv — an EXISTING
// *mcp.Server, so kahyad can combine them with mcp/memory's and mcp/fs's
// own tools on ONE shared MCP server/session (kahyad/internal/server/
// mcp.go's buildMCPHandler). Use MCPServer below when a standalone server
// is wanted instead (tests).
func (s *Server) RegisterTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "shell_docker",
		Description: "Model-yazımı shell script'ini pinlenmiş Docker sandbox imajı içinde (varsayılan --network none, salt-okunur kök, yalnız görev iş-dizini rw) çalıştırır.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args RunInput) (*mcp.CallToolResult, RunOutput, error) {
		out, err := s.Runner.Run(ctx, traceIDFromRequest(req), taskIDFromRequest(req), args)
		return nil, out, err
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "shell_host",
		Description: "Dar, argüman-doğrulamalı host komut seti çalıştırır: git (yalnız status|log|diff|show), ls, stat.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args HostExecArgs) (*mcp.CallToolResult, HostExecOutput, error) {
		out, err := s.HostExec.Handle(ctx, traceIDFromRequest(req), taskIDFromRequest(req), args)
		return nil, out, err
	})
}

// MCPServer builds a fresh standalone *mcp.Server with both tools
// registered (test convenience — kahyad's real wiring uses RegisterTools
// on its ALREADY-shared server instead, see this file's doc comment).
func (s *Server) MCPServer(impl *mcp.Implementation) *mcp.Server {
	if impl == nil {
		impl = &mcp.Implementation{Name: "kahya-shell", Version: "0.1.0"}
	}
	srv := mcp.NewServer(impl, nil)
	s.RegisterTools(srv)
	return srv
}

// Shutdown kills every kahya.task_id-labeled container this daemon may
// have left running (this task's spec step 7) — called from kahyad's own
// graceful-shutdown path (kahyad/internal/server/shell.go /
// server.go's Shutdown).
func (s *Server) Shutdown(ctx context.Context) error {
	if s == nil || s.Runner == nil {
		return nil
	}
	return s.Runner.KillAllLabeled(ctx)
}

// traceIDFromRequest/taskIDFromRequest extract the propagated headers
// from an incoming CallToolRequest (mirrors mcp/fs.traceIDFromRequest/
// taskIDFromRequest exactly). Both return "" when unavailable (req.Extra
// nil, e.g. a direct Go-level test call never routed through a real HTTP
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
