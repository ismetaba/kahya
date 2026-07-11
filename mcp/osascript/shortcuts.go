// shortcuts.go implements shortcuts_run: running a NAMED, EXISTING
// Shortcut (`shortcuts run <name> [--input-path <file>]`). Arbitrary
// Shortcut CREATION is explicitly out of scope (this task's own spec) —
// shortcut BODIES are opaque to this package by design (they may contain
// arbitrary actions we have no way to statically scan, which is exactly
// why only a user-created, already-named shortcut is ever runnable at
// all): there is no Scan step here at all, unlike applescript_run/
// jxa_run's runner.go. The approval payload this gate mints/consumes a
// token against is therefore the shortcut's NAME plus its canonicalized
// --input-path, and NOTHING else (kahyad/internal/approval.BuildShortcut).
package osascript

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	mcpfs "kahya/mcp/fs"
)

// reasonShortcutNameEmpty is a mechanical, pre-policy rejection (mirrors
// mcp/shell.HostExec's own "validate before any policy decision"
// ordering) — an empty name can never be approved into meaning anything.
const reasonShortcutNameEmpty = "shortcuts_run reddedildi: isim boş olamaz."

// ShortcutInput is shortcuts_run's invocation contract.
type ShortcutInput struct {
	Name      string `json:"name" jsonschema:"çalıştırılacak, VAR OLAN, isimli shortcut'ın adı"`
	InputPath string `json:"input_path,omitempty" jsonschema:"--input-path olarak geçirilecek dosya yolu (opsiyonel, mutlak veya ~ ile başlayan yol)"`
}

// ShortcutOutput is shortcuts_run's result.
type ShortcutOutput struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	TimedOut bool   `json:"timed_out"`
}

// RunShortcut implements shortcuts_run end to end: validate (mechanical,
// pre-policy) → canonicalize InputPath → Policy.Check → Policy.
// ConsumeToken (with the hash of EXACTLY {name, canonical_input_path},
// nothing more) → `shortcuts run` under the SAME hard 120s/process-group-
// kill timeout applescript_run/jxa_run use → ledger shortcuts_exec.
func (r *Runner) RunShortcut(ctx context.Context, traceID, taskID string, in ShortcutInput) (ShortcutOutput, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return ShortcutOutput{}, errors.New(reasonShortcutNameEmpty)
	}

	var canonInputPath string
	if in.InputPath != "" {
		cp, err := mcpfs.Canonicalize(r.Home, in.InputPath)
		if err != nil {
			return ShortcutOutput{}, fmt.Errorf("shortcuts_run: %w", err)
		}
		canonInputPath = cp.Match
	}

	toolInput := buildShortcutToolInput(name, canonInputPath)
	decision, err := r.Policy.Check(ctx, "shortcuts_run", defaultScope, taskID, traceID, toolInput)
	if err != nil {
		return ShortcutOutput{}, fmt.Errorf("shortcuts_run: %w", err)
	}
	if decision.Result != PolicyResultAllow {
		return ShortcutOutput{}, errors.New(decision.Reason)
	}
	if err := r.Policy.ConsumeToken(ctx, decision.Token, "shortcuts_run", decision.Class, defaultScope, taskID, traceID, toolInput); err != nil {
		return ShortcutOutput{}, fmt.Errorf("shortcuts_run: onay jetonu tüketilemedi: %w", err)
	}

	argv := []string{"run", name}
	if canonInputPath != "" {
		argv = append(argv, "--input-path", canonInputPath)
	}

	unit := r.timeoutUnit
	if unit <= 0 {
		unit = time.Second
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, hardTimeoutSeconds*unit)
	defer cancel()

	execRes, execErr := r.Exec.Run(timeoutCtx, "shortcuts", argv, nil)
	if errors.Is(execErr, context.DeadlineExceeded) {
		logAndLedger(ctx, r.Ledger, r.Log, traceID, "osascript_timeout", map[string]any{
			"event": "osascript_timeout", "tool": "shortcuts_run", "shortcut_name": name,
			"task_id": taskID, "trace_id": traceID, "timeout_s": hardTimeoutSeconds,
		})
		return ShortcutOutput{TimedOut: true}, errors.New(reasonTimeout)
	}
	if execErr != nil {
		return ShortcutOutput{}, fmt.Errorf("shortcuts_run: %w", execErr)
	}

	out := ShortcutOutput{ExitCode: execRes.ExitCode, Stdout: string(execRes.Stdout), Stderr: string(execRes.Stderr)}
	logAndLedger(ctx, r.Ledger, r.Log, traceID, "shortcuts_exec", map[string]any{
		"event": "shortcuts_exec", "shortcut_name": name, "input_path": canonInputPath,
		"exit_code": out.ExitCode, "trace_id": traceID, "task_id": taskID,
	})
	return out, nil
}

// shortcutToolInputEnvelope is the deterministic JSON shape shortcuts_run
// hashes — {name, input_path} and NOTHING else (this task's own spec,
// verbatim: "the approval payload is the shortcut name + canonicalized
// input path"). kahyad/internal/server/approvals.go mirrors this SAME
// shape by hand to render the WYSIWYE approval card via
// kahyad/internal/approval.BuildShortcut.
type shortcutToolInputEnvelope struct {
	Name      string `json:"name"`
	InputPath string `json:"input_path,omitempty"`
}

func buildShortcutToolInput(name, inputPath string) []byte {
	env := shortcutToolInputEnvelope{Name: name, InputPath: inputPath}
	b, _ := json.Marshal(env)
	return b
}
