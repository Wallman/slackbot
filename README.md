# Slackbot: App Store Connect + Cloudflare + LLM (OpenRouter, free tier)

A Slack bot that answers questions in Slack by pulling live
context from **App Store Connect** (app info) and **Cloudflare** (zone
analytics), then asking a free LLM on **OpenRouter** to answer.

## Architecture
- `main.go` — entrypoint; Slack Socket Mode event loop
- `llm` — OpenRouter chat completions client
- `appstoreconnect` — JWT(ES256)-authenticated ASC API client
- `cloudflare` — Cloudflare GraphQL Analytics client

When a user `@mentions` the bot, its message (plus a system prompt) is sent
to the LLM along with two **tool/function definitions**: `get_app_store_connect_info`
and `get_cloudflare_traffic`. The model itself decides whether it needs live data
and which tool(s) to call. The bot executes any requested tool calls, feeds the
results back to the model, and repeats (up to `maxToolIterations`) until it returns
a final answer, which is posted back to Slack.

## 1. Slack app setup (Socket Mode)
You said you already have a Slack app — make sure it has:
- **Socket Mode** enabled, with an **app-level token** (`xapp-...`) that has the `connections:write` scope
- **Bot token scopes**: `app_mentions:read`, `chat:write`
- **Event Subscriptions** → subscribe to bot event `app_mention`
- Install/reinstall the app to your workspace to get the **bot token** (`xoxb-...`)

## 2. Credentials
Copy `.env.example` to `.env` and fill in.

## 3. Run locally
```bash
go run .
```
In Slack, invite the bot to a channel and mention it:
```
@YourBot what's our app's current status?
@YourBot show me cloudflare traffic
```

## 4. Notes / next steps
- **ASC sales reports**: the current `appstoreconnect` client only fetches
  app metadata (`/v1/apps`) as a smoke test. Apple's actual *Sales and Trends
  Reports* API returns gzipped TSV files
- **Cloudflare**: currently pulls 7-day request/bytes/threats totals via the
  GraphQL Analytics API for `CF_ZONE_ID`.
- Secrets are loaded from `.env` via `godotenv`; never commit `.env`.
