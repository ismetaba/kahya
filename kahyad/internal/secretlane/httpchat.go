// httpchat.go is the shared OpenAI-compatible chat-completion plumbing
// both classifier.go's HTTPQwenClassifier and answer.go's LocalAnswerer
// build on - one local Qwen3-30B-A3B server, two different call shapes
// (strict-JSON classification vs. free-text answering) against the exact
// same "/chat/completions" wire protocol.
package secretlane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatCompletionRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens"`
	// ChatTemplateKwargs is mlx_lm.server's own extension (its own
	// --chat-template-args flag's per-REQUEST equivalent): Qwen3 is a
	// hybrid reasoning model whose chat template defaults to emitting a
	// long "thinking" trace BEFORE any actual answer - confirmed live
	// against the real server (KAHYA_MLX_TESTS=1): with thinking left
	// enabled, a 64-token classification budget is entirely consumed by
	// the reasoning trace and the response never reaches the strict-JSON
	// answer at all (finish_reason="length", no "content" field - fails
	// closed correctly, but for the wrong reason and far too slow to ever
	// meet the task spec's 300ms warm-model budget). Passing
	// {"enable_thinking": false} here (verified live: ~0.3-0.4s
	// round-trip once warm) makes the model answer directly - used by
	// BOTH classifier.go (needs to fit in a tiny token budget) and
	// answer.go (needs bounded, predictable latency for an MVP chat
	// reply); a deliberate simplification documented here rather than a
	// per-call toggle.
	ChatTemplateKwargs map[string]any `json:"chat_template_kwargs,omitempty"`
}

type chatCompletionResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
}

// postChatCompletion POSTs an OpenAI-compatible chat-completion request to
// baseURL+"/chat/completions" and returns the first choice's message
// content, verbatim (no JSON-shape assumption about ITS content - that is
// each caller's own concern: classifier.go expects strict JSON, answer.go
// expects free text). client defaults to http.DefaultClient when nil.
func postChatCompletion(ctx context.Context, client *http.Client, baseURL, model string, messages []chatMessage, temperature float64, maxTokens int) (string, error) {
	reqBody := chatCompletionRequest{
		Model:              model,
		Messages:           messages,
		Temperature:        temperature,
		MaxTokens:          maxTokens,
		ChatTemplateKwargs: map[string]any{"enable_thinking": false},
	}
	b, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("secretlane: marshal qwen request: %w", err)
	}

	url := strings.TrimRight(baseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return "", fmt.Errorf("secretlane: build qwen request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("secretlane: qwen request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("secretlane: qwen request: status %d", resp.StatusCode)
	}

	var chatResp chatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return "", fmt.Errorf("secretlane: qwen request: decode chat response: %w", err)
	}
	if len(chatResp.Choices) == 0 {
		return "", fmt.Errorf("secretlane: qwen request: no choices in response")
	}
	return strings.TrimSpace(chatResp.Choices[0].Message.Content), nil
}
