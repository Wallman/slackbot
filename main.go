package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/joho/godotenv"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"slackbot/appstoreconnect"
	"slackbot/cloudflare"
	"slackbot/contextbudget"
	"slackbot/llm"
	"slackbot/store"
)

const systemPrompt = `You are Svante, a helpful Slack bot assistant.
If a tool call fails or isn't relevant, answer from what you know and say plainly what data you lack.
Format your output using Slack syntax.`

const maxToolIterations = 4
const contextBudgetThreshold = 0.8

func main() {
	_ = godotenv.Load()

	botToken := mustEnv("SLACK_BOT_TOKEN")
	appToken := mustEnv("SLACK_APP_TOKEN")
	orKey := mustEnv("OPENROUTER_API_KEY")
	orModel := envOr("OPENROUTER_MODEL", "nvidia/nemotron-3.5-lightning:free")
	dbURL := mustEnv("DATABASE_URL")
	maxContextTokens := envOrInt("MAX_CONTEXT_TOKENS", 32000)

	ctx := context.Background()

	db, err := store.Open(ctx, dbURL)
	if err != nil {
		log.Fatalf("failed to connect to postgres: %v", err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		log.Fatalf("failed to migrate postgres schema: %v", err)
	}

	ascClient := appstoreconnect.NewClient(
		os.Getenv("ASC_ISSUER_ID"),
		os.Getenv("ASC_KEY_ID"),
		os.Getenv("ASC_PRIVATE_KEY_PATH"),
	)
	ascAppID := os.Getenv("ASC_APP_ID")

	cfClient := cloudflare.NewClient(
		os.Getenv("CF_API_TOKEN"),
		os.Getenv("CF_ZONE_ID"),
	)

	llmClient := llm.NewClient(orKey, orModel)

	api := slack.New(botToken, slack.OptionAppLevelToken(appToken))

	authResp, err := api.AuthTest()
	if err != nil {
		log.Fatalf("slack auth test failed: %v", err)
	}
	botUserID := authResp.UserID
	log.Printf("Authenticated as bot user %s", botUserID)

	client := socketmode.New(api)
	names := newUserNameCache()

	go handleEvents(client, api, llmClient, ascClient, cfClient, db, names, ascAppID, botUserID, maxContextTokens)

	log.Println("Starting Slack bot...")
	if err := client.Run(); err != nil {
		log.Fatalf("socketmode run error: %v", err)
	}
}

func handleEvents(
	client *socketmode.Client,
	api *slack.Client,
	llmClient *llm.Client,
	ascClient *appstoreconnect.Client,
	cfClient *cloudflare.Client,
	db *store.Store,
	names *userNameCache,
	ascAppID, botUserID string,
	maxContextTokens int,
) {
	for event := range client.Events {
		switch event.Type {
		case socketmode.EventTypeEventsAPI:
			client.Ack(*event.Request)

			eventsAPIEvent, ok := event.Data.(slackevents.EventsAPIEvent)
			if !ok {
				continue
			}

			if eventsAPIEvent.Type == slackevents.CallbackEvent {
				innerEvent := eventsAPIEvent.InnerEvent
				switch ev := innerEvent.Data.(type) {
				case *slackevents.AppMentionEvent:
					receivedAt := time.Now()
					log.Printf("[timing] received app_mention from user=%s channel=%s", ev.User, ev.Channel)
					threadTS := ev.ThreadTimeStamp
					if threadTS == "" {
						threadTS = ev.TimeStamp // first message in a new thread
					}
					go respond(api, llmClient, ascClient, cfClient, db, names, ascAppID, ev.Channel, threadTS, ev.TimeStamp, ev.User, ev.Text, receivedAt, maxContextTokens)

				case *slackevents.MessageEvent:
					// Only consider plain replies inside an existing
					// thread, from real users (not bots/ourselves), that
					// don't already contain a mention (which app_mention
					// above already handles - avoids double-processing the
					// same message).
					if ev.BotID != "" || ev.User == "" || ev.User == botUserID {
						continue
					}
					if ev.ThreadTimeStamp == "" {
						continue
					}
					if containsMention(ev.Text, botUserID) {
						continue
					}

					receivedAt := time.Now()
					known, err := db.IsKnownThread(context.Background(), ev.Channel, ev.ThreadTimeStamp)
					if err != nil {
						log.Printf("failed to check known thread: %v", err)
						continue
					}
					if !known {
						continue
					}
					log.Printf("[timing] received threaded reply from user=%s channel=%s thread=%s", ev.User, ev.Channel, ev.ThreadTimeStamp)
					go respond(api, llmClient, ascClient, cfClient, db, names, ascAppID, ev.Channel, ev.ThreadTimeStamp, ev.TimeStamp, ev.User, ev.Text, receivedAt, maxContextTokens)
				}
			}
		case socketmode.EventTypeConnecting:
			log.Println("Connecting to Slack...")
		case socketmode.EventTypeConnected:
			log.Println("Connected to Slack.")
		}
	}
}

// containsMention reports whether text contains a Slack mention of the
// given user ID, e.g. "<@U123ABC>".
func containsMention(text, userID string) bool {
	if userID == "" {
		return false
	}
	return strings.Contains(text, "<@"+userID+">")
}

// respond loads the thread's persisted history, appends the new user
// message, runs the tool-calling conversation loop with the LLM, persists
// the result, and posts the final answer back into the thread.
func respond(
	api *slack.Client,
	llmClient *llm.Client,
	ascClient *appstoreconnect.Client,
	cfClient *cloudflare.Client,
	db *store.Store,
	names *userNameCache,
	ascAppID string,
	channel, threadTS, msgTS, user, text string,
	receivedAt time.Time,
	maxContextTokens int,
) {
	ctx := context.Background()
	tools := availableTools()

	known, err := db.IsKnownThread(ctx, channel, threadTS)
	if err != nil {
		log.Printf("failed to check known thread: %v", err)
	}

	if err := db.EnsureThread(ctx, channel, threadTS); err != nil {
		log.Printf("failed to ensure thread: %v", err)
	}

	if !known {
		backfillThreadHistory(ctx, api, db, names, channel, threadTS, msgTS)
	}

	history, err := db.LoadHistory(ctx, channel, threadTS)
	if err != nil {
		log.Printf("failed to load thread history: %v", err)
	}

	authorName := names.resolveName(api, user)
	userMsg := llm.Message{Role: "user", Content: fmt.Sprintf("%s: %s", authorName, text)}
	if err := db.AppendMessage(ctx, channel, threadTS, userMsg); err != nil {
		log.Printf("failed to persist user message: %v", err)
	}

	messages := make([]llm.Message, 0, len(history)+2)
	messages = append(messages, llm.Message{Role: "system", Content: systemPrompt})
	messages = append(messages, history...)
	messages = append(messages, userMsg)

	if before := contextbudget.EstimateTokens(messages); float64(before) > float64(maxContextTokens)*contextBudgetThreshold {
		messages = contextbudget.Trim(messages, int(float64(maxContextTokens)*contextBudgetThreshold))
		after := contextbudget.EstimateTokens(messages)
		log.Printf("[context] trimmed thread %s/%s: %d -> %d estimated tokens (budget %d)",
			channel, threadTS, before, after, maxContextTokens)
	}

	var reply string
	for i := 0; i < maxToolIterations; i++ {
		llmStart := time.Now()
		result, err := llmClient.Complete(ctx, messages, tools)
		log.Printf("[timing] LLM completion took %s (iteration=%d, tool_calls=%d, err=%v)",
			time.Since(llmStart), i, len(result.ToolCalls), err)
		if err != nil {
			reply = fmt.Sprintf("Error: %v", err)
			break
		}

		if len(result.ToolCalls) == 0 {
			reply = result.Content
			assistantMsg := llm.Message{Role: "assistant", Content: reply}
			messages = append(messages, assistantMsg)
			if err := db.AppendMessage(ctx, channel, threadTS, assistantMsg); err != nil {
				log.Printf("failed to persist assistant message: %v", err)
			}
			break
		}

		// Record the assistant's tool-call request, then execute each tool
		// and feed results back as "tool" messages before asking again.
		assistantMsg := llm.Message{Role: "assistant", ToolCalls: result.ToolCalls}
		messages = append(messages, assistantMsg)
		if err := db.AppendMessage(ctx, channel, threadTS, assistantMsg); err != nil {
			log.Printf("failed to persist assistant tool-call message: %v", err)
		}

		for _, call := range result.ToolCalls {
			log.Printf("[tool] calling %s(%s)", call.Function.Name, call.Function.Arguments)
			toolStart := time.Now()
			output := executeTool(ascClient, cfClient, ascAppID, call.Function.Name, call.Function.Arguments)
			log.Printf("[timing] tool %s took %s", call.Function.Name, time.Since(toolStart))

			toolMsg := llm.Message{Role: "tool", ToolCallID: call.ID, Content: output}
			messages = append(messages, toolMsg)
			if err := db.AppendMessage(ctx, channel, threadTS, toolMsg); err != nil {
				log.Printf("failed to persist tool result message: %v", err)
			}
		}

		if i == maxToolIterations-1 {
			reply = "Error: Too many tool calls."
		}
	}

	postStart := time.Now()
	if _, _, err := api.PostMessage(channel, slack.MsgOptionText(reply, false), slack.MsgOptionTS(threadTS)); err != nil {
		log.Printf("failed to post message: %v", err)
	}
	log.Printf("[timing] Slack post took %s", time.Since(postStart))
	log.Printf("[timing] total time from mention to reply posted: %s", time.Since(receivedAt))
}

func backfillThreadHistory(ctx context.Context, api *slack.Client, db *store.Store, names *userNameCache, channel, threadTS, currentMsgTS string) {
	replies, _, _, err := api.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{
		ChannelID: channel,
		Timestamp: threadTS,
	})
	if err != nil {
		log.Printf("failed to fetch thread history for backfill: %v", err)
		return
	}

	for _, m := range replies {
		if m.Timestamp == currentMsgTS {
			continue // this is the message that triggered respond(); it's added separately below
		}
		if m.Text == "" {
			continue
		}

		var msg llm.Message
		if m.BotID != "" {
			// Treat prior bot messages as assistant turns, unprefixed.
			msg = llm.Message{Role: "assistant", Content: m.Text}
		} else {
			authorName := names.resolveName(api, m.User)
			msg = llm.Message{Role: "user", Content: fmt.Sprintf("%s: %s", authorName, m.Text)}
		}
		if err := db.AppendMessage(ctx, channel, threadTS, msg); err != nil {
			log.Printf("failed to persist backfilled message: %v", err)
		}
	}
}

type userNameCache struct {
	mu    sync.Mutex
	names map[string]string
}

func newUserNameCache() *userNameCache {
	return &userNameCache{names: make(map[string]string)}
}

func (c *userNameCache) resolveName(api *slack.Client, userID string) string {
	if userID == "" {
		return "someone"
	}

	c.mu.Lock()
	if name, ok := c.names[userID]; ok {
		c.mu.Unlock()
		return name
	}
	c.mu.Unlock()

	name := userID
	info, err := api.GetUserInfo(userID)
	if err != nil {
		log.Printf("failed to resolve display name for %s: %v", userID, err)
	} else {
		switch {
		case info.Profile.DisplayName != "":
			name = info.Profile.DisplayName
		case info.RealName != "":
			name = info.RealName
		case info.Name != "":
			name = info.Name
		}
	}

	c.mu.Lock()
	c.names[userID] = name
	c.mu.Unlock()
	return name
}

func availableTools() []llm.Tool {
	return []llm.Tool{
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "get_app_store_connect_info",
				Description: "Fetch App Store Connect metadata (name, bundle ID, SKU, state, etc.) for the configured app. Use this for questions about the app's status, info, or sales/listing details.",
				Parameters: map[string]any{
					"type":       "object",
					"properties": map[string]any{},
				},
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "get_cloudflare_traffic",
				Description: "Fetch Cloudflare zone traffic totals (requests, bytes, threats) grouped by day. Use this for questions about web traffic, requests, or Cloudflare analytics.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"days": map[string]any{
							"type":        "integer",
							"description": "Number of trailing days to fetch (default 7).",
						},
					},
				},
			},
		},
	}
}

func executeTool(ascClient *appstoreconnect.Client, cfClient *cloudflare.Client, ascAppID, name, argsJSON string) string {
	switch name {
	case "get_app_store_connect_info":
		if ascAppID == "" {
			return "error: ASC_APP_ID is not configured"
		}
		t0 := time.Now()
		info, err := ascClient.GetAppInfo(ascAppID)
		log.Printf("[timing] App Store Connect fetch took %s (err=%v)", time.Since(t0), err)
		if err != nil {
			return fmt.Sprintf("error fetching App Store Connect info: %v", err)
		}
		return truncate(info, 3000)

	case "get_cloudflare_traffic":
		var args struct {
			Days int `json:"days"`
		}
		_ = json.Unmarshal([]byte(argsJSON), &args) // ignore error: fall back to default below
		days := args.Days
		if days <= 0 {
			days = 7
		}
		t0 := time.Now()
		data, err := cfClient.GetZoneRequestTotals(days)
		log.Printf("[timing] Cloudflare fetch took %s (err=%v)", time.Since(t0), err)
		if err != nil {
			return fmt.Sprintf("error fetching Cloudflare traffic: %v", err)
		}
		return truncate(data, 3000)

	default:
		return fmt.Sprintf("error: unknown tool %q", name)
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("missing required environment variable: %s", key)
	}
	return v
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envOrInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("invalid integer for %s=%q, using default %d: %v", key, v, fallback, err)
		return fallback
	}
	return n
}
