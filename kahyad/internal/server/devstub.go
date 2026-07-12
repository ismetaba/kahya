// devstub.go implements w2_slow_stub - a DEV-ONLY, W4-07-acceptance-gate-
// only MCP tool that exists purely so the gate can drive the REAL
// kahyad/internal/task.Receipts intent->executing->receipt/idempotent-
// replay lifecycle end to end against a genuinely running kahyad, worker,
// and kahya-mcp bridge (W4-02 built that machinery and proved it with a
// stub tool of its own - receipts_test.go - but never wired it into the
// REAL side-effectful tools: fs_write/shell_docker/applescript_run still
// execute with no tool_calls row at all - see receipts.go's own doc
// comment history and the "kahya-w4-receipt-gap" note. Closing THAT gap is
// out of scope here; this file's whole reason to exist is to give W4-07's
// scenario A something real to kill-and-resume against without first
// closing it).
//
// Effect = sleep(duration_ms), then append exactly one line to
// counter_file - the task spec's own "side effect" definition
// (tasks/w4-durability/W4-07-w4-acceptance.md, "Dev-profile stub W2 tool").
// Registered onto the SAME shared MCP server every other tool uses
// (mcp.go's buildMCPHandler), but deliberately NOT added to
// fsOwnedTools/shellOwnedTools/osascriptOwnedTools: this tool has no
// separate gate chain of its own, so it goes through policyGateMiddleware's
// GENERIC Check+ConsumeToken path - exactly like memory_write/
// memory_forget (mcp.go's own doc comment names those as the precedent for
// "kahyad owns both the decision and the execution of this tool in the
// same process").
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
	"database/sql"
	"encoding/json"
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

// DevStubOutput is w2_slow_stub's output (also the exact shape durably
// persisted in tool_calls.receipt_json - task.Receipts.Execute's own
// idempotent-replay guarantee returns THIS SAME value, byte-for-byte, on a
// replay hit).
type DevStubOutput struct {
	CounterFile string `json:"counter_file"`
	DurationMs  int64  `json:"duration_ms"`
}

// SetDevStub wires the W4-07 acceptance-gate-only w2_slow_stub tool.
// receipts must be non-nil for the tool to actually register -
// buildMCPHandler (mcp.go) only calls registerDevStubTool when this has
// been set. Call before Prepare(). main.go calls this ONLY when
// cfg.Env == config.EnvDev (see its own call-site comment) - never in
// production, regardless of what any policy.yaml declares.
func (s *Server) SetDevStub(receipts *task.Receipts) {
	s.devStubReceipts = receipts
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

// HandleDevStub implements w2_slow_stub's whole effect: sleep, append one
// line to args.CounterFile, then a durable receipt via
// task.Receipts.Execute - exported so a test can drive it directly, below
// the MCP transport, matching every other tool handler's convention in
// this package (mcp/fs.Server.HandleWrite, mcp/shell.Server.Handle*, ...).
//
// Deliberately detached from the caller's own request context (uses
// context.Background() for the Execute call, not the incoming ctx): this
// is the exact mechanism W4-07 scenario A exercises - once this side
// effect begins, it must run to completion and its receipt must commit
// REGARDLESS of whether the calling worker process (or its kahya-mcp
// bridge) is still alive to ever read the response. Tying the sleep to the
// request's own context would make the effect abort the moment that
// connection ever closes for any reason, which would make the tool_calls
// row strand at 'executing' forever instead of ever reaching 'receipt' -
// exactly the double-execution hazard this whole gate exists to disprove.
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

	argBytes, err := json.Marshal(args)
	if err != nil {
		return DevStubOutput{}, fmt.Errorf("w2_slow_stub: marshal args: %w", err)
	}
	argsHash := task.HashArgs(argBytes)

	effect := func(_ context.Context, _ *sql.Tx) (json.RawMessage, error) {
		time.Sleep(duration)
		if err := appendCounterLine(args.CounterFile); err != nil {
			return nil, fmt.Errorf("w2_slow_stub: append counter file: %w", err)
		}
		return json.Marshal(DevStubOutput{CounterFile: args.CounterFile, DurationMs: args.DurationMs})
	}

	resultJSON, _, err := s.devStubReceipts.Execute(context.Background(), task.ExecuteInput{
		TaskID: taskID, TraceID: traceID, ToolName: devStubToolName, Class: task.ClassW2, ArgsHash: argsHash,
	}, effect)
	if err != nil {
		return DevStubOutput{}, fmt.Errorf("w2_slow_stub: %w", err)
	}
	var out DevStubOutput
	if uerr := json.Unmarshal(resultJSON, &out); uerr != nil {
		return DevStubOutput{}, fmt.Errorf("w2_slow_stub: decode result: %w", uerr)
	}
	return out, nil
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
