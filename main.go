package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/joho/godotenv"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"slackbot/appstoreconnect"
	"slackbot/cloudflare"
	"slackbot/llm"
)

const systemPrompt = `You are a helpful Slack bot assistant for a mobile app team.
If a tool call fails or isn't relevant, answer from what you know and say plainly what data you lack.`

const maxToolIterations = 4

func main() {
	_ = godotenv.Load()

	botToken := mustEnv("SLACK_BOT_TOKEN")
	appToken := mustEnv("SLACK_APP_TOKEN")
	orKey := mustEnv("OPENROUTER_API_KEY")
	orModel := envOr("OPENROUTER_MODEL", "nvidia/nemotron-3-ultra-550b-a55b:free")

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
	client := socketmode.New(api)

	go handleEvents(client, api, llmClient, ascClient, cfClient, ascAppID)

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
	ascAppID string,
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
					go respond(api, llmClient, ascClient, cfClient, ascAppID, ev.Channel, ev.User, ev.Text, receivedAt)
				}
			}
		case socketmode.EventTypeConnecting:
			log.Println("Connecting to Slack...")
		case socketmode.EventTypeConnected:
			log.Println("Connected to Slack.")
		}
	}
}

func respond(
	api *slack.Client,
	llmClient *llm.Client,
	ascClient *appstoreconnect.Client,
	cfClient *cloudflare.Client,
	ascAppID string,
	channel, user, text string,
	receivedAt time.Time,
) {
	ctx := context.Background()
	tools := availableTools()

	messages := []llm.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: text},
	}

	var reply string
	for i := 0; i < maxToolIterations; i++ {
		llmStart := time.Now()
		result, err := llmClient.Complete(ctx, messages, tools)
		log.Printf("[timing] LLM completion took %s (iteration=%d, tool_calls=%d, err=%v)",
			time.Since(llmStart), i, len(result.ToolCalls), err)
		if err != nil {
			reply = fmt.Sprintf("Sorry <@%s>, I hit an error calling the LLM: %v", user, err)
			break
		}

		if len(result.ToolCalls) == 0 {
			reply = result.Content
			break
		}

		// Record the assistant's tool-call request, then execute each tool
		// and feed results back as "tool" messages before asking again.
		messages = append(messages, llm.Message{Role: "assistant", ToolCalls: result.ToolCalls})
		for _, call := range result.ToolCalls {
			log.Printf("[tool] calling %s(%s)", call.Function.Name, call.Function.Arguments)
			toolStart := time.Now()
			output := executeTool(ascClient, cfClient, ascAppID, call.Function.Name, call.Function.Arguments)
			log.Printf("[timing] tool %s took %s", call.Function.Name, time.Since(toolStart))
			messages = append(messages, llm.Message{
				Role:       "tool",
				ToolCallID: call.ID,
				Content:    output,
			})
		}

		if i == maxToolIterations-1 {
			reply = "Sorry, I made too many tool calls trying to answer that without reaching a final answer."
		}
	}

	postStart := time.Now()
	if _, _, err := api.PostMessage(channel, slack.MsgOptionText(reply, false)); err != nil {
		log.Printf("failed to post message: %v", err)
	}
	log.Printf("[timing] Slack post took %s", time.Since(postStart))
	log.Printf("[timing] total time from mention to reply posted: %s", time.Since(receivedAt))
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
