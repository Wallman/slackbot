package contextbudget

import (
	"encoding/json"
	"sync"

	"github.com/pkoukk/tiktoken-go"
	tiktoken_loader "github.com/pkoukk/tiktoken-go-loader"

	"slackbot/llm"
)

// perMessageOverhead approximates the extra tokens the chat format spends
// per message framing (role, separators, etc.), on top of the content
// itself. This mirrors OpenAI's documented chat-format overhead estimate.
const perMessageOverhead = 4

var (
	initOnce sync.Once
	encoder  *tiktoken.Tiktoken
)

// ensureEncoder registers the offline BPE loader so token counting never
// makes a network call, then lazily loads the cl100k_base encoding used as
// our estimate (the exact tokenizer the target model uses may differ
// slightly, but this is accurate enough for budgeting purposes).
func ensureEncoder() *tiktoken.Tiktoken {
	initOnce.Do(func() {
		tiktoken.SetBpeLoader(tiktoken_loader.NewOfflineLoader())
		enc, err := tiktoken.GetEncoding("cl100k_base")
		if err != nil {
			// Fall back to nil; EstimateTokens degrades to a
			// character-count-based approximation in that case.
			encoder = nil
			return
		}
		encoder = enc
	})
	return encoder
}

// countTokens estimates the token count of a single string.
func countTokens(s string) int {
	if s == "" {
		return 0
	}
	if enc := ensureEncoder(); enc != nil {
		return len(enc.Encode(s, nil, nil))
	}
	// Fallback approximation: ~4 characters per token.
	return len(s)/4 + 1
}

// EstimateTokens returns an approximate total token count for the given
// messages, including a per-message framing overhead.
func EstimateTokens(messages []llm.Message) int {
	total := 0
	for _, m := range messages {
		total += perMessageOverhead
		total += countTokens(m.Content)
		if len(m.ToolCalls) > 0 {
			if b, err := json.Marshal(m.ToolCalls); err == nil {
				total += countTokens(string(b))
			}
		}
	}
	return total
}

// turn groups one logical exchange: either a single user message, or an
// assistant message together with any tool-call/tool-result messages that
// belong to it. Turns are never split, since the API requires an
// assistant tool-call message to be immediately followed by its matching
// tool-result messages.
type turn struct {
	messages []llm.Message
}

// groupTurns splits messages (excluding the leading system message, which
// callers must handle separately) into turns.
func groupTurns(messages []llm.Message) []turn {
	var turns []turn
	for _, m := range messages {
		switch {
		case m.Role == "user":
			turns = append(turns, turn{messages: []llm.Message{m}})
		case m.Role == "assistant":
			turns = append(turns, turn{messages: []llm.Message{m}})
		case m.Role == "tool":
			if len(turns) == 0 {
				// Malformed/unexpected leading tool message; keep it on
				// its own rather than index into an empty slice.
				turns = append(turns, turn{messages: []llm.Message{m}})
				continue
			}
			last := &turns[len(turns)-1]
			last.messages = append(last.messages, m)
		default:
			turns = append(turns, turn{messages: []llm.Message{m}})
		}
	}
	return turns
}

// Trim returns messages trimmed to fit within maxTokens, preferring to
// drop the oldest turns first. The leading system message (if present) is
// always preserved, and trimming stops as soon as the budget is met or
// only the single most recent turn remains.
func Trim(messages []llm.Message, maxTokens int) []llm.Message {
	if len(messages) == 0 || EstimateTokens(messages) <= maxTokens {
		return messages
	}

	var system []llm.Message
	rest := messages
	if len(messages) > 0 && messages[0].Role == "system" {
		system = messages[:1]
		rest = messages[1:]
	}

	turns := groupTurns(rest)

	// Drop oldest turns until under budget or only one turn remains.
	for len(turns) > 1 {
		candidate := flatten(system, turns)
		if EstimateTokens(candidate) <= maxTokens {
			return candidate
		}
		turns = turns[1:]
	}

	return flatten(system, turns)
}

func flatten(system []llm.Message, turns []turn) []llm.Message {
	out := make([]llm.Message, 0, len(system)+len(turns)*2)
	out = append(out, system...)
	for _, t := range turns {
		out = append(out, t.messages...)
	}
	return out
}
