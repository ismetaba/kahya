// answer.go implements W3-08's other half of the routing branch: actually
// ANSWERING a secret-lane task entirely on the local Qwen3-30B-A3B server.
//
// HANDOFF §4 routing table says the Reader/secret-lane path uses Qwen3-
// 30B-A3B locally, "değilse claude-haiku-4-5" otherwise - but the worker
// (worker/kahya_worker, W12-09) is built on claude-agent-sdk, which speaks
// the Anthropic Messages API, not an OpenAI-compatible endpoint like
// mlx_lm.server exposes. Rewiring that SDK to talk to a local OpenAI
// endpoint instead of Anthropic's API is out of reach for this task (the
// task spec's own explicit escape hatch: "If wiring the SDK to a local
// OpenAI endpoint is complex, at minimum the envelope carries the lane and
// the proxy backstop enforces no-cloud").
//
// Rather than spawn the cloud-shaped worker for a task that must never
// reach the cloud (which would only ever fail once the W12-08 proxy
// backstop's 403 fires - correct, but a poor user experience, and it still
// pays the worker-spawn cost for nothing), kahyad answers secret-lane
// tasks DIRECTLY: kahyad/internal/server's POST /v1/task handler never
// spawns a worker at all for a lane=="secret" task - it calls THIS type
// instead, which talks to the local Qwen server exactly the same way
// classifier.go's HTTPQwenClassifier does. This is strictly STRONGER than
// the task spec's minimum bar: there is no code path through which a
// secret-lane task's content could even reach the worker process, let
// alone the Anthropic proxy - not merely a backstop that blocks it after
// the fact.
package secretlane

import (
	"context"
	"fmt"
	"net/http"
)

// Answerer answers a prompt entirely on-device - the abstraction
// kahyad/internal/server depends on, mirroring QwenClassifier's identical
// shape/rationale (LocalAnswerer is the production implementation; tests
// use AnswererFunc).
type Answerer interface {
	Answer(ctx context.Context, prompt string) (string, error)
}

// answererSystemPrompt instructs Qwen to behave like Kâhya's own assistant
// persona for a secret-lane answer - plain free text, no JSON contract
// (unlike classifier.go's classifierSystemPrompt).
const answererSystemPrompt = `You are Kâhya, a private local assistant running entirely on the user's own Mac. Answer the user's request directly and helpfully, in the same language they wrote in (usually Turkish). This request was classified as sensitive (finans/sağlık/kimlik) and is being answered entirely on-device for privacy - do not mention this classification process itself unless the user asks about it directly.`

// LocalAnswerer answers a prompt entirely via the local Qwen3-30B-A3B
// server's OpenAI-compatible /chat/completions endpoint - fully
// unit-testable against a fake httptest.Server (see answer_test.go); only
// the live, KAHYA_MLX_TESTS=1-gated test exercises this against the real
// server.
type LocalAnswerer struct {
	// BaseURL is the OpenAI-compatible base ("http://127.0.0.1:<port>/v1").
	BaseURL string
	// Model is the model name field the chat-completion request body
	// carries (see HTTPQwenClassifier.Model's identical doc comment).
	Model  string
	Client *http.Client
}

// NewLocalAnswerer constructs a LocalAnswerer with a default *http.Client.
func NewLocalAnswerer(baseURL, model string) *LocalAnswerer {
	return &LocalAnswerer{BaseURL: baseURL, Model: model, Client: &http.Client{}}
}

// Answer returns Qwen's free-text answer to prompt. maxTokens bounds the
// response length (0 uses a generous default - task spec does not fix a
// specific ceiling for the ANSWER path the way it does for the 64-token
// classification JSON).
func (a *LocalAnswerer) Answer(ctx context.Context, prompt string) (string, error) {
	maxTokens := 2048
	content, err := postChatCompletion(ctx, a.Client, a.BaseURL, a.Model, []chatMessage{
		{Role: "system", Content: answererSystemPrompt},
		{Role: "user", Content: prompt},
	}, 0.7, maxTokens)
	if err != nil {
		return "", fmt.Errorf("secretlane: local answer: %w", err)
	}
	return content, nil
}

// AnswererFunc adapts a plain function to Answerer - used by this
// package's OWN tests (hanging/erroring fakes), mirroring
// QwenClassifierFunc's identical pattern.
type AnswererFunc func(ctx context.Context, prompt string) (string, error)

func (f AnswererFunc) Answer(ctx context.Context, prompt string) (string, error) {
	return f(ctx, prompt)
}
