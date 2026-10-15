// localsession.go implements the production Session for the SECRET lane:
// a direct OpenAI-compatible HTTP call to the kahyad-supervised local
// Qwen3-30B-A3B server (kahyad/internal/mlx.Supervisor), reusing
// kahyad/internal/secretlane.PostChatJSON's exact wire protocol - NEVER
// through ClaudeSDKClient/the worker process (HANDOFF §4 gizli-serit row:
// "(makineden cikmaz)"). This mirrors kahyad/internal/secretlane's own
// classifier/answerer local-lane transport and kahyad/internal/reader's
// LocalModel contract exactly: the ONLY way a secret-lane file's bytes
// ever reach a model is this local, loopback-only call.
package consolidation

import (
	"context"
	"net/http"
	"time"

	"kahya/kahyad/internal/secretlane"
)

// LocalRunner is the narrow fail-closed-load-gate + idle-tracking seam
// LocalSession needs - *kahyad/internal/mlx.Supervisor satisfies this
// directly (Do/BaseURL, unmodified). Do MUST return
// kahyad/internal/mlx.ErrLocalModelUnavailable (errors.Is-comparable,
// wrapped or bare) WITHOUT ever invoking fn whenever the fail-closed
// free-memory gate or the underlying spawn/health-poll fails - Supervisor.
// Do already implements exactly this contract; LocalSession adds no
// timeout/retry logic of its own on top of it (task spec ordering
// invariant: "cold model means the classification WAITS for load or
// fails closed - it never skips ahead to cloud", applied here to
// consolidation calls instead of classification calls).
type LocalRunner interface {
	Do(ctx context.Context, warmBudget time.Duration, fn func(ctx context.Context) error) error
	BaseURL() string
}

// localWarmBudget bounds one ALREADY-WARM local call (Supervisor.Do's own
// "no bound at all on a cold load" contract - see that method's doc
// comment). One batch (FIX-B-5) may answer with up to localMaxTokens; at a
// conservative ~30 tokens/s for Qwen3-30B-A3B on MLX that is about 4.5
// minutes, so the budget is 5 minutes (it was 60 seconds, which a full
// local rewrite could not meet and which failed the whole night).
const localWarmBudget = 5 * time.Minute

// localMaxTokens is the max_tokens budget for one local-lane consolidation
// call (one batch, FIX-B-5: the local batch budget is sized against it, see
// tokens.go). The exact figure is not safety-relevant: a too-small budget
// just means a truncated/invalid JSON response, which parseRewriteResponse
// rejects (ErrRewriteMalformed) and the orchestrator drops that batch only.
const localMaxTokens = 8192

// LocalSession is the production Session for the secret lane.
type LocalSession struct {
	Sup    LocalRunner
	Model  string
	Client *http.Client
}

var _ Session = LocalSession{}

func (s LocalSession) Consolidate(ctx context.Context, traceID string, files map[string]string) (map[string]string, error) {
	if len(files) == 0 {
		return map[string]string{}, nil
	}
	_, userContent, err := buildRewriteHalves(files)
	if err != nil {
		return nil, err
	}

	var raw string
	err = s.Sup.Do(ctx, localWarmBudget, func(callCtx context.Context) error {
		var innerErr error
		raw, innerErr = secretlane.PostChatJSON(callCtx, s.Client, s.Sup.BaseURL(), s.Model, rewriteSystemPrompt, userContent, 0, localMaxTokens)
		return innerErr
	})
	if err != nil {
		// mlx.ErrLocalModelUnavailable (fail-closed: insufficient memory or
		// spawn/health failure) propagates UNCHANGED (errors.Is-comparable) -
		// the orchestrator (consolidation.go) is the ONLY thing that
		// interprets this sentinel; this type never itself falls back to
		// anything.
		return nil, err
	}
	return parseRewriteResponse(raw, LaneLocal, files)
}

// buildRewriteHalves splits buildRewritePrompt's combined string back into
// its fixed system-instruction prefix and the per-call JSON body, since
// secretlane.PostChatJSON (unlike the cloud lane's single-Prompt-field
// envelope) takes a system/user pair rather than one combined string.
// Returned systemPrompt is always exactly rewriteSystemPrompt (kept as a
// return value purely so callers never need to reference the package
// constant directly by name).
func buildRewriteHalves(files map[string]string) (systemPrompt, userContent string, err error) {
	full, err := buildRewritePrompt(files)
	if err != nil {
		return "", "", err
	}
	userContent = full[len(rewriteSystemPrompt)+len(separatorBetweenInstructionsAndContent):]
	return rewriteSystemPrompt, userContent, nil
}
