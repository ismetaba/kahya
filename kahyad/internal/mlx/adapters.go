// adapters.go wires this package's Supervisor (spawn/health/memcheck/
// idle-TTL) into kahyad/internal/secretlane's two Qwen-backed interfaces
// (QwenClassifier and Answerer): every real call ensures the local server
// is running (fail-closed memcheck + spawn/health-poll) and tracks the
// call as in-flight against the idle-TTL monitor BEFORE ever reaching the
// HTTP layer - see Supervisor.Do's own doc comment for the full contract.
package mlx

import (
	"context"

	"kahya/kahyad/internal/secretlane"
)

// QwenClassifierAdapter implements secretlane.QwenClassifier on top of one
// Supervisor - the ONLY thing kahyad/internal/server needs to wire the
// real local classification path in (see NewQwenClassifierAdapter).
type QwenClassifierAdapter struct {
	sup  *Supervisor
	http *secretlane.HTTPQwenClassifier
}

// NewQwenClassifierAdapter constructs a QwenClassifierAdapter. model is the
// model name field every chat-completion request carries (mlx_lm.server
// accepts any string here - see secretlane.HTTPQwenClassifier.Model's own
// doc comment).
func NewQwenClassifierAdapter(sup *Supervisor, model string) *QwenClassifierAdapter {
	return &QwenClassifierAdapter{sup: sup, http: secretlane.NewHTTPQwenClassifier(sup.BaseURL(), model)}
}

// Classify implements secretlane.QwenClassifier.
func (a *QwenClassifierAdapter) Classify(ctx context.Context, text string) (secretlane.Verdict, error) {
	var v secretlane.Verdict
	err := a.sup.Do(ctx, secretlane.DefaultBudget, func(callCtx context.Context) error {
		var innerErr error
		v, innerErr = a.http.Classify(callCtx, text)
		return innerErr
	})
	if err != nil {
		return secretlane.Verdict{}, err
	}
	return v, nil
}

// AnswererAdapter implements secretlane.Answerer on top of one Supervisor -
// the local-only answer path for a task already labeled lane=="secret"
// (kahyad/internal/secretlane/answer.go's own doc comment explains why
// this bypasses the claude-agent-sdk worker entirely for such a task).
type AnswererAdapter struct {
	sup  *Supervisor
	http *secretlane.LocalAnswerer
}

// NewAnswererAdapter constructs an AnswererAdapter sharing the SAME
// Supervisor a QwenClassifierAdapter uses (one Qwen3-30B-A3B process,
// HANDOFF §4 ⚑ local fleet locked to exactly three models).
func NewAnswererAdapter(sup *Supervisor, model string) *AnswererAdapter {
	return &AnswererAdapter{sup: sup, http: secretlane.NewLocalAnswerer(sup.BaseURL(), model)}
}

// Answer implements secretlane.Answerer. No warm-load budget is applied
// here (0 disables Supervisor.Do's budget) - task spec's 300ms figure is
// specifically the CLASSIFICATION budget; answering a full prompt is
// expected to take much longer even once warm, and must never be cut off
// by an unrelated latency target.
func (a *AnswererAdapter) Answer(ctx context.Context, prompt string) (string, error) {
	var out string
	err := a.sup.Do(ctx, 0, func(callCtx context.Context) error {
		var innerErr error
		out, innerErr = a.http.Answer(callCtx, prompt)
		return innerErr
	})
	if err != nil {
		return "", err
	}
	return out, nil
}
