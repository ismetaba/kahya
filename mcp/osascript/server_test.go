package osascript

import (
	"context"
	"errors"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func newTestServer() (*Server, *fakeExecutor) {
	r, _, _, exec := newTestRunner()
	return New(r), exec
}

// listToolNames calls srv's tools/list and returns every registered
// tool's name — used to prove which of applescript_run/jxa_run/
// shortcuts_run actually got mounted.
func listToolNames(t *testing.T, srv *mcp.Server) map[string]bool {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.0"}, nil)
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	ctx := context.Background()

	serverSession, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server Connect: %v", err)
	}
	defer serverSession.Close()

	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	defer clientSession.Close()

	res, err := clientSession.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	names := make(map[string]bool, len(res.Tools))
	for _, tool := range res.Tools {
		names[tool.Name] = true
	}
	return names
}

// TestRegisterToolsMountsAllThreeWhenShortcutsAvailable proves every one
// of applescript_run/jxa_run/shortcuts_run is registered when the
// `shortcuts` CLI is present.
func TestRegisterToolsMountsAllThreeWhenShortcutsAvailable(t *testing.T) {
	s, _ := newTestServer()
	s.LookPath = func(string) (string, error) { return "/usr/bin/shortcuts", nil }

	names := listToolNames(t, s.MCPServer(nil))
	for _, want := range []string{"applescript_run", "jxa_run", "shortcuts_run"} {
		if !names[want] {
			t.Errorf("tool %q not registered; got %v", want, names)
		}
	}
}

// TestRegisterToolsDisablesShortcutsWhenAbsent is this task's own spec,
// verbatim: "check command -v shortcuts at registration; disable
// shortcuts_run cleanly if absent" — applescript_run/jxa_run must still
// register normally.
func TestRegisterToolsDisablesShortcutsWhenAbsent(t *testing.T) {
	s, _ := newTestServer()
	s.LookPath = func(string) (string, error) { return "", errors.New("not found") }

	names := listToolNames(t, s.MCPServer(nil))
	if names["shortcuts_run"] {
		t.Error("shortcuts_run registered despite the `shortcuts` CLI being absent")
	}
	for _, want := range []string{"applescript_run", "jxa_run"} {
		if !names[want] {
			t.Errorf("tool %q not registered; got %v", want, names)
		}
	}
}

// TestCallToolAppliescriptRunEndToEnd drives applescript_run through the
// real MCP wire transport (unlike runner_test.go, which calls
// Runner.RunApplescript directly) — proving the tool is wired up so that
// a real tools/call actually reaches the gate chain and its stub
// executor, with no error surfaced end to end for a clean script.
func TestCallToolAppliescriptRunEndToEnd(t *testing.T) {
	s, exec := newTestServer()
	s.LookPath = func(string) (string, error) { return "", errors.New("not found") }
	srv := s.MCPServer(nil)

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.0"}, nil)
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	ctx := context.Background()

	serverSession, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server Connect: %v", err)
	}
	defer serverSession.Close()
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	defer clientSession.Close()

	res, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "applescript_run",
		Arguments: map[string]any{"script": `tell application "Finder" to get name of every window`},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("CallTool result IsError = true, content = %+v", res.Content)
	}
	if exec.callCount() != 1 {
		t.Errorf("Executor.Run called %d times over the wire, want 1", exec.callCount())
	}
}

// TestCallToolShellShapedScriptRejectedOverWire proves a shell-shaped
// script rejected by Scan travels over the wire as a non-error result
// (IsError:false) carrying structured StructuredContent — since a Go
// error would make the MCP SDK drop the Reroute field entirely (this
// package's runner.go doc comment explains why the rejection path
// deliberately avoids returning a Go error).
func TestCallToolShellShapedScriptRejectedOverWire(t *testing.T) {
	s, exec := newTestServer()
	srv := s.MCPServer(nil)

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.0"}, nil)
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	ctx := context.Background()

	serverSession, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server Connect: %v", err)
	}
	defer serverSession.Close()
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	defer clientSession.Close()

	res, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "applescript_run",
		Arguments: map[string]any{"script": `do shell script "whoami"`},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("CallTool result IsError = true, want false (structured rejection, not a protocol error)")
	}
	if res.StructuredContent == nil {
		t.Fatal("StructuredContent = nil, want the ScriptOutput{Rejected:true,...} JSON")
	}
	if exec.callCount() != 0 {
		t.Errorf("Executor.Run called %d times over the wire, want 0 (nothing may execute on a rejection)", exec.callCount())
	}
}
