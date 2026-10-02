// Package llm provides a client for chat completions via OpenRouter,
// which proxies many providers (including free-tier models) behind
// an OpenAI-compatible API. It supports tool/function calling so the
// model itself can decide which external data sources to query.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"time"
)

const openRouterURL = "https://openrouter.ai/api/v1/chat/completions"

const (
	maxRetries     = 4
	baseRetryDelay = 500 * time.Millisecond
	maxRetryDelay  = 8 * time.Second
)

// Client talks to OpenRouter's chat completions endpoint.
type Client struct {
	APIKey     string
	Model      string
	HTTPClient *http.Client
}

// NewClient creates an OpenRouter-backed LLM client.
func NewClient(apiKey, model string) *Client {
	return &Client{
		APIKey:     apiKey,
		Model:      model,
		HTTPClient: &http.Client{Timeout: 60 * time.Second},
	}
}

// Message is a single chat message in OpenAI-compatible format.
//
// Role is one of "system", "user", "assistant", or "tool".
// Assistant messages that invoke tools populate ToolCalls and leave Content
// empty; tool-result messages populate ToolCallID and Content and leave
// Role "tool".
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall is a single function invocation requested by the model.
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // always "function"
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"` // JSON-encoded arguments
	} `json:"function"`
}

// Tool describes a function the model may choose to call.
type Tool struct {
	Type     string       `json:"type"` // always "function"
	Function ToolFunction `json:"function"`
}

// ToolFunction is the JSON-schema description of a callable function.
type ToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type chatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Tools    []Tool    `json:"tools,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// CompleteResult is the model's reply: either a final text answer, or a set
// of tool calls the caller must execute before continuing the conversation.
type CompleteResult struct {
	Content   string
	ToolCalls []ToolCall
}

// Complete sends the conversation (optionally with available tools) to the
// LLM and returns either a final answer or requested tool calls. Transient
// upstream failures (overload, rate limiting, 5xx) are retried with
// exponential backoff and jitter.
func (c *Client) Complete(ctx context.Context, messages []Message, tools []Tool) (CompleteResult, error) {
	var lastErr error

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			delay := backoffDelay(attempt)
			log.Printf("[llm] retrying after transient error (attempt %d/%d, waiting %s): %v", attempt, maxRetries, delay, lastErr)
			select {
			case <-ctx.Done():
				return CompleteResult{}, ctx.Err()
			case <-time.After(delay):
			}
		}

		result, retryable, err := c.doComplete(ctx, messages, tools)
		if err == nil {
			return result, nil
		}
		lastErr = err
		if !retryable {
			return CompleteResult{}, err
		}
	}

	return CompleteResult{}, fmt.Errorf("gave up after %d retries: %w", maxRetries, lastErr)
}

// backoffDelay returns an exponential delay (capped) with +/-20% jitter.
func backoffDelay(attempt int) time.Duration {
	delay := baseRetryDelay * time.Duration(1<<uint(attempt-1))
	if delay > maxRetryDelay {
		delay = maxRetryDelay
	}
	jitter := time.Duration(rand.Int63n(int64(delay) / 2))
	return delay/2 + jitter
}

// isRetryableStatus reports whether an HTTP status code indicates a
// transient failure worth retrying (rate limiting or server-side errors).
func isRetryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// isRetryableMessage reports whether an OpenRouter/provider error message
// describes a transient condition (model overloaded, rate limited, etc.).
func isRetryableMessage(msg string) bool {
	lower := strings.ToLower(msg)
	for _, substr := range []string{"overloaded", "temporarily", "rate limit", "try again", "timeout", "timed out"} {
		if strings.Contains(lower, substr) {
			return true
		}
	}
	return false
}

// doComplete performs a single request attempt. The retryable return value
// indicates whether the caller should retry on error.
func (c *Client) doComplete(ctx context.Context, messages []Message, tools []Tool) (result CompleteResult, retryable bool, err error) {
	reqBody, err := json.Marshal(chatRequest{Model: c.Model, Messages: messages, Tools: tools})
	if err != nil {
		return CompleteResult{}, false, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, openRouterURL, bytes.NewReader(reqBody))
	if err != nil {
		return CompleteResult{}, false, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	// Optional but recommended by OpenRouter for routing/analytics.
	req.Header.Set("HTTP-Referer", "https://github.com/local-slackbot")
	req.Header.Set("X-Title", "Slackbot ASC+CF Assistant")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		// Network-level errors (timeouts, connection resets) are worth retrying.
		return CompleteResult{}, true, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return CompleteResult{}, true, fmt.Errorf("read response: %w", err)
	}

	if isRetryableStatus(resp.StatusCode) {
		return CompleteResult{}, true, fmt.Errorf("transient http status %d: %s", resp.StatusCode, string(body))
	}

	var parsed chatResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return CompleteResult{}, false, fmt.Errorf("parse response (status %d): %s", resp.StatusCode, string(body))
	}
	if parsed.Error != nil {
		retryable := isRetryableMessage(parsed.Error.Message)
		return CompleteResult{}, retryable, fmt.Errorf("openrouter error: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return CompleteResult{}, true, fmt.Errorf("no choices returned (status %d): %s", resp.StatusCode, string(body))
	}

	msg := parsed.Choices[0].Message
	return CompleteResult{Content: msg.Content, ToolCalls: msg.ToolCalls}, false, nil
}
