package policy

import (
	"context"
	"testing"
)

// TestCheckDeniesForeignMCPServerTool (FIX-FINAL worker-cli-loads-
// claudeai-connectors): a tool from any MCP server other than kahya_memory
// (a claude.ai connector, an enterprise managed-mcp server) is an unknown
// tool, even when stripping its server segment would collide with a real
// policy tool name - an ALLOW would let the worker CLI run the remote
// connector outside kahyad. kahya_memory names still canonicalize.
func TestCheckDeniesForeignMCPServerTool(t *testing.T) {
	e, st := testEngine(t)
	seedState(t, st, "fs_read", string(ClassR), "global", L1)
	ctx := context.Background()
	for _, name := range []string{
		"mcp__claude_ai_Gmail__fs_read",
		"mcp__claude_ai_Drive__memory_search",
		"mcp__enterprise__fs_read",
	} {
		d, err := e.Check(ctx, CheckInput{Tool: name, TaskID: "t1", TraceID: "tr1", Preflight: true})
		if err != nil {
			t.Fatalf("%s: Check: %v", name, err)
		}
		if d.Result != ResultDeny || d.Reason != ReasonUnknownTool {
			t.Errorf("%s: decision = %+v, want deny %q", name, d, ReasonUnknownTool)
		}
	}
	d, err := e.Check(ctx, CheckInput{Tool: "mcp__kahya_memory__fs_read", TaskID: "t1", TraceID: "tr1", Preflight: true})
	if err != nil {
		t.Fatalf("kahya_memory Check: %v", err)
	}
	if d.Result != ResultAllow {
		t.Errorf("mcp__kahya_memory__fs_read decision = %+v, want allow (class R)", d)
	}
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM events WHERE kind = 'policy_decision' AND json_extract(payload, '$.tool') = 'mcp__claude_ai_Gmail__fs_read'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("foreign tool deny ledgered under the raw name %d times, want 1", n)
	}
}
