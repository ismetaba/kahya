// devstub.go implements w2_slow_stub - a DEV-ONLY, W4-07-acceptance-gate-
// only MCP tool that exists purely so the gate can drive the REAL
// kahyad/internal/task.Receipts intent->executing->receipt/idempotent-
// replay lifecycle end to end against a genuinely running kahyad, worker,
// and kahya-mcp bridge with a slow, countable side effect.
//
// FIX-I-4: the receipt is no longer written HERE. Since the W4 receipt gap
// was closed, every non-R tool on the generic /v1/mcp path (memory_write,
// memory_forget, this stub) runs its handler inside
// Receipts.ExecuteDetached in policyGateMiddleware (mcp.go / receipts.go),
// and the real fs/shell/osascript tools do the same through their own
// receipt seam - so this handler is a plain effect, and W4-07 scenario A now
// proves the GENERIC path's receipt + replay (an inner Execute here would
// have deadlocked on the same keyed lock, or written a second tool_calls
// row for the one call).
//
// Effect = sleep(duration_ms), then append exactly one line to
// counter_file - the task spec's own "side effect" definition
// (tasks/w4-durability/W4-07-w4-acceptance.md, "Dev-profile stub W2 tool").
// Registered onto the SAME shared MCP server every other tool uses
// (mcp.go's buildMCPHandler), but deliberately NOT added to
// fsOwnedTools/shellOwnedTools/osascriptOwnedTools: this tool has no
// separate gate chain of its own, so it goes through policyGateMiddleware's
// GENERIC Check+ConsumeToken(+receipt) path - exactly like memory_write/
// memory_forget.
//
// Class is hardcoded W2 (never configurable via the MCP call's own
// arguments - HANDOFF §5's WYSIWYE posture: a tool's action class is
// server-side metadata, never something a caller's own input could ever
// widen or narrow) and reversible:false in the dev policy.yaml overlay
// this tool is declared under (tests/acceptance/w4/fixtures/policy.yaml) -
// this tool is NEVER declared in the repo-root production policy.yaml, and
// is unreachable in production regardless (SetDevStub is only ever called
// under KAHYA_ENV=dev, main.go's own call-site comment).
package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"kahya/kahyad/internal/task"
)

// devStubToolName is the exact MCP tool name (and tool_calls.tool_name
// value) the W4-07 gate's dev policy.yaml overlay declares and every
// acceptance-gate assertion greps for.
const devStubToolName = "w2_slow_stub"

// devStubMaxDuration bounds DevStubArgs.DurationMs - a safety valve so a
// malformed/malicious argument can never hang this tool (and, by
// extension, the HTTP goroutine handling it) indefinitely; the W4_REAL
// real-time evidence run's own >=600s duration comfortably fits under it.
const devStubMaxDuration = 30 * time.Minute

// DevStubArgs is w2_slow_stub's input.
type DevStubArgs struct {
	DurationMs  int64  `json:"duration_ms" jsonschema:"efekt calismadan once beklenecek sure (ms)"`
	CounterFile string `json:"counter_file" jsonschema:"tek bir satirin ekleneceği sayaç dosyasının mutlak yolu"`
}

// DevStubOutput is w2_slow_stub's output (the structured content of the
// CallToolResult the generic /v1/mcp path persists in tool_calls.
// receipt_json - a replay hit returns that SAME result).
type DevStubOutput struct {
	CounterFile string `json:"counter_file"`
	DurationMs  int64  `json:"duration_ms"`
}

// SetDevStub wires the W4-07 acceptance-gate-only w2_slow_stub tool.
// receipts must be non-nil for the tool to actually register -
// buildMCPHandler (mcp.go) only calls registerDevStubTool when this has
// been set. Call before Prepare(). main.go calls this ONLY when
// cfg.Env == config.EnvDev (see its own call-site comment) - never in
// production, regardless of what any policy.yaml declares. FIX-I-4: the
// stub's receipt is written by the generic /v1/mcp path, so the same
// receipts also become the server's SetReceipts when none were wired yet -
// the W2 stub is never registered without a receipt lifecycle around it.
func (s *Server) SetDevStub(receipts *task.Receipts) {
	s.devStubReceipts = receipts
	if s.receipts == nil {
		s.receipts = receipts
	}
}

// registerDevStubTool adds w2_slow_stub onto srv - called from
// buildMCPHandler only when s.devStubReceipts != nil. Deliberately NOT
// added to fsOwnedTools/shellOwnedTools/osascriptOwnedTools - see this
// file's own package doc comment for why the generic policyGateMiddleware
// path is exactly right here.
func (s *Server) registerDevStubTool(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        devStubToolName,
		Description: "SADECE GELİŞTİRME (W4-07 kabul kapısı): bekler, sayaç dosyasına bir satır ekler, makbuzla biter.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args DevStubArgs) (*mcp.CallToolResult, DevStubOutput, error) {
		traceID := traceIDFromMCPRequest(req)
		taskID := taskIDFromMCPRequest(req)
		out, err := s.HandleDevStub(traceID, taskID, args)
		return nil, out, err
	})
}

// traceIDFromMCPRequest/taskIDFromMCPRequest read the propagated headers
// off an incoming CallToolRequest - mirrors mcp/fs.traceIDFromRequest/
// taskIDFromRequest exactly (that package cannot be imported here to share
// the helper - it lives outside the kahyad/internal/* boundary - so this
// is the SAME tiny logic, kept in sync by hand, against this package's own
// already-defined traceHeader/taskHeader constants, mcp.go).
func traceIDFromMCPRequest(req *mcp.CallToolRequest) string {
	if req == nil || req.Extra == nil || req.Extra.Header == nil {
		return ""
	}
	return req.Extra.Header.Get(traceHeader)
}

func taskIDFromMCPRequest(req *mcp.CallToolRequest) string {
	if req == nil || req.Extra == nil || req.Extra.Header == nil {
		return ""
	}
	return req.Extra.Header.Get(taskHeader)
}

// HandleDevStub implements w2_slow_stub's whole effect: sleep, then append
// one line to args.CounterFile - exported so a test can drive it directly,
// below the MCP transport, matching every other tool handler's convention
// in this package (mcp/fs.Server.HandleWrite, mcp/shell.Server.Handle*,
// ...). It takes no context at all: once this side effect begins it runs to
// completion REGARDLESS of whether the calling worker process (or its
// kahya-mcp bridge) is still alive - the exact mechanism W4-07 scenario A
// exercises. The durable receipt around it is written by the generic
// /v1/mcp path (FIX-I-4: policyGateMiddleware -> Receipts.ExecuteDetached,
// itself on a context detached from the request), never here.
func (s *Server) HandleDevStub(traceID, taskID string, args DevStubArgs) (DevStubOutput, error) {
	if s.devStubReceipts == nil {
		return DevStubOutput{}, errors.New("w2_slow_stub: bu araç yalnız KAHYA_ENV=dev altında kullanılabilir")
	}
	if taskID == "" {
		return DevStubOutput{}, errors.New("w2_slow_stub: task_id çözülemedi (X-Kahya-Task-Id başlığı yok)")
	}
	if args.CounterFile == "" {
		return DevStubOutput{}, errors.New("w2_slow_stub: counter_file boş olamaz")
	}
	duration := time.Duration(args.DurationMs) * time.Millisecond
	if duration < 0 || duration > devStubMaxDuration {
		return DevStubOutput{}, fmt.Errorf("w2_slow_stub: duration_ms sınır dışı (0..%d ms)", devStubMaxDuration.Milliseconds())
	}

	// The receipt lifecycle around this call (generic /v1/mcp path) holds no
	// DB transaction or connection (FIX-I-12 / FIX-I-4), so this sleep never
	// blocks /policy/check or the ledger.
	time.Sleep(duration)
	if err := appendCounterLine(args.CounterFile); err != nil {
		return DevStubOutput{}, fmt.Errorf("w2_slow_stub: append counter file: %w", err)
	}
	return DevStubOutput{CounterFile: args.CounterFile, DurationMs: args.DurationMs}, nil
}

// appendCounterLine appends exactly one "1\n" line to path, creating it
// (and never truncating an existing one) if absent - the task spec's own
// "the side effect" definition. O_APPEND makes each call's write atomic
// with respect to any other appender (though Receipts.Execute's own
// idempotent-replay guarantee is what actually prevents this from ever
// being called twice for the same logical attempt - this is simply the
// correct, ordinary way to implement "append one line" regardless).
func appendCounterLine(path string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString("1\n")
	return err
}
