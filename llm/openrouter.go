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
	"net/http"
	"time"
)

const openRouterURL = "https://openrouter.ai/api/v1/chat/completions"

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
// LLM and returns either a final answer or requested tool calls.
func (c *Client) Complete(ctx context.Context, messages []Message, tools []Tool) (CompleteResult, error) {
	reqBody, err := json.Marshal(chatRequest{Model: c.Model, Messages: messages, Tools: tools})
	if err != nil {
		return CompleteResult{}, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, openRouterURL, bytes.NewReader(reqBody))
	if err != nil {
		return CompleteResult{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	// Optional but recommended by OpenRouter for routing/analytics.
	req.Header.Set("HTTP-Referer", "https://github.com/local-slackbot")
	req.Header.Set("X-Title", "Slackbot ASC+CF Assistant")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return CompleteResult{}, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return CompleteResult{}, fmt.Errorf("read response: %w", err)
	}

	var parsed chatResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return CompleteResult{}, fmt.Errorf("parse response (status %d): %s", resp.StatusCode, string(body))
	}
	if parsed.Error != nil {
		return CompleteResult{}, fmt.Errorf("openrouter error: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return CompleteResult{}, fmt.Errorf("no choices returned (status %d): %s", resp.StatusCode, string(body))
	}

	msg := parsed.Choices[0].Message
	return CompleteResult{Content: msg.Content, ToolCalls: msg.ToolCalls}, nil
}
