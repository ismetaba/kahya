// session.go defines the "consolidation session" seam this package
// depends on: given a lane's file set (relPath -> whole-file content), get
// back whole-file REWRITES for that same set. Two independent
// implementations exist - CloudSession (worker.go, claude-haiku-4-5 via
// the toolless Reader-mode worker profile, HANDOFF §4 routing row
// "Cikarim . geri-yazim") and LocalSession (localsession.go, the local
// Qwen3-30B-A3B server, HANDOFF §4 gizli-serit row) - and the orchestrator
// (consolidation.go) NEVER calls one with the other's files: lane.go's
// PartitionByLane has already split cloudFiles/localFiles before either
// Session value is even touched.
//
// Neither implementation - nor this Session interface itself - takes a
// *sql.DB or any brain.db handle of any kind: a consolidation session's
// entire job is "text in, text out". This is the structural half of the
// WRITE BOUNDARY invariant (the other half is worktree.go only ever
// writing inside the git worktree kahyad itself created) - see
// consolidation_test.go's write-boundary test.
package consolidation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Session is one lane's whole-file-rewrite transport. Consolidate MUST
// return a map containing an entry for every key in files (a session that
// leaves a file unchanged still echoes it back verbatim) - ApplyRewrites
// (worktree.go) tolerates a MISSING key by treating it as "unchanged", so
// a non-compliant implementation degrades safely rather than losing a
// file, but every implementation in THIS package always echoes every key.
type Session interface {
	Consolidate(ctx context.Context, traceID string, files map[string]string) (rewrites map[string]string, err error)
}

// SessionFunc adapts a plain function to Session (mirrors
// kahyad/internal/secretlane.QwenClassifierFunc's identical pattern) -
// every hermetic test in this package injects one of these instead of a
// real Cloud/LocalSession, so no test ever spawns a real worker process or
// dials a real (or even fake-HTTP) local model server. The func itself
// IS the "request log" a caller's test closure can record into - see
// consolidation_test.go's secret-lane invariant test.
type SessionFunc func(ctx context.Context, traceID string, files map[string]string) (map[string]string, error)

func (f SessionFunc) Consolidate(ctx context.Context, traceID string, files map[string]string) (map[string]string, error) {
	return f(ctx, traceID, files)
}

// rewriteSystemPrompt is the model-facing (English - CLAUDE.md's Turkish-
// user-facing-strings policy does not apply to model prompts)
// consolidation instruction (task spec Deliverables, verbatim intent:
// "merge duplicates, fix headings, fold new notes into topic files").
// Every file this session is given is TRUSTED first-party memory content
// (never mail/web bodies - those go through kahyad/internal/reader's
// separate untrusted-Reader path), so - unlike reader.go's mail/webpage
// prompts - this one carries no "treat as untrusted data" caveat; it does
// still forbid anything beyond a whole-file JSON rewrite (no shell, no
// git, no tool use is even offered - Mode=ModeReader's toolless worker
// profile enforces that structurally, this is defense in depth only).
const rewriteSystemPrompt = `You are a memory-consolidation assistant running on the user's own machine. You will be given a JSON object mapping relative file paths to their CURRENT full markdown content, drawn from the user's personal notes.

Your job: merge duplicate information across files, fix/normalize markdown headings, and fold any new/loose notes into the correct existing topic file. Preserve every fact; never invent new information; never delete content unless it is a verbatim duplicate of content kept elsewhere.

Respond with STRICT JSON ONLY, no markdown code fences, no commentary, exactly this shape:

{"files": {"<path>": "<new full whole-file content>", ...}}

You MUST include an entry for EVERY path you were given, even ones you leave completely unchanged (echo the original content back verbatim in that case). Never add a path you were not given. Never include any key other than "files".`

// separatorBetweenInstructionsAndContent mirrors kahyad/internal/reader/
// cloud_model.go's identical constant: the envelope carries one prompt
// string, never a separate system/user split.
const separatorBetweenInstructionsAndContent = "\n\n--- FILES TO CONSOLIDATE (JSON: path -> content) ---\n\n"

// buildRewritePrompt renders files (sorted by path for determinism) as a
// single JSON object and appends it to rewriteSystemPrompt - the exact
// text every Session implementation in this package sends the model,
// whichever lane it is.
func buildRewritePrompt(files map[string]string) (string, error) {
	ordered := sortedKeys(files)
	obj := make(map[string]string, len(files))
	for _, k := range ordered {
		obj[k] = files[k]
	}
	body, err := json.Marshal(obj)
	if err != nil {
		return "", fmt.Errorf("consolidation: marshal files for prompt: %w", err)
	}
	return rewriteSystemPrompt + separatorBetweenInstructionsAndContent + string(body), nil
}

// parseRewriteResponse decodes a model's raw text response into a
// {"files": {...}} rewrite map, tolerating (task spec: fail-closed for
// SECURITY, not for an honest model-output parsing hiccup) leading/
// trailing whitespace but nothing else - a response that is not exactly
// that one-key JSON object is a hard error, never a partial/best-effort
// parse (mirrors kahyad/internal/reader's own "decode/validation failure
// fails the whole job closed" posture).
func parseRewriteResponse(raw string) (map[string]string, error) {
	var parsed struct {
		Files map[string]string `json:"files"`
	}
	trimmed := strings.TrimSpace(raw)
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return nil, fmt.Errorf("consolidation: decode rewrite response: %w", err)
	}
	if parsed.Files == nil {
		return nil, fmt.Errorf("consolidation: rewrite response missing \"files\" object")
	}
	return parsed.Files, nil
}
