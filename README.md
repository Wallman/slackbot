# Slackbot: App Store Connect + Cloudflare + LLM (OpenRouter, free tier)

A Slack bot that answers questions in Slack by pulling live
context from **App Store Connect** (app info) and **Cloudflare** (zone
analytics), then asking a free LLM on **OpenRouter** to answer.

## Architecture
- `main.go` — entrypoint; Slack Socket Mode event loop
- `llm` — OpenRouter chat completions client
- `appstoreconnect` — JWT(ES256)-authenticated ASC API client
- `cloudflare` — Cloudflare GraphQL Analytics client
- `store` — Postgres-backed thread conversation history

When a user `@mentions` the bot, its message (plus a system prompt and the
thread's prior history) is sent to the LLM along with two **tool/function
definitions**: `get_app_store_connect_info` and `get_cloudflare_traffic`. The
model itself decides whether it needs live data and which tool(s) to call.
The bot executes any requested tool calls, feeds the results back to the
model, and repeats (up to `maxToolIterations`) until it returns a final
answer, which is posted back to Slack **inside a thread**.

### Threaded conversations
- Every reply is posted in a thread (`slack.MsgOptionTS`) — even the first
  reply to a plain `@mention`, which starts a new thread rooted at that
  message.
- Full conversation history per thread is persisted in Postgres
  (`threads` + `thread_messages` tables) so follow-up questions in the same
  thread keep context, and history survives bot restarts.
- Replies inside an already-known thread are picked up **even without
  re-mentioning the bot** — the bot subscribes to plain `message` events and
  checks Postgres (`IsKnownThread`) to decide whether to respond, so it
  behaves like a normal participant once added to a thread.
- If the bot is `@mentioned` inside a thread that already had prior
  messages (i.e. it's being looped in mid-conversation), it fetches the
  full thread via `conversations.replies` and backfills that history into
  Postgres before responding, so it has context from before it joined.
- Every stored/forwarded user message is tagged with the author's Slack
  display name so the LLM can tell different people apart in a multi-person
  thread. Display names are resolved via `users.info` and cached in memory.


## 1. Slack app setup (Socket Mode)
You said you already have a Slack app — make sure it has:
- **Socket Mode** enabled, with an **app-level token** (`xapp-...`) that has the `connections:write` scope
- **Bot token scopes**: `app_mentions:read`, `chat:write`, `channels:history`, `users:read` (add `groups:history`/`im:history`/`mpim:history` too if the bot is used in private channels/DMs)
- **Event Subscriptions** → subscribe to bot events `app_mention` and `message.channels` (+ `message.groups`/`message.im`/`message.mpim` as needed)
- Install/reinstall the app to your workspace to get the **bot token** (`xoxb-...`)

## 2. Local Postgres
```bash
docker compose up -d
```
Starts Postgres on `localhost:5432` (user/pass/db: `slackbot`), matching the
default `DATABASE_URL` in `.env.example`. The bot creates its own schema on
startup (`db.Migrate`) — no manual migration step needed.

## 3. Credentials
Copy `.env.example` to `.env` and fill in.

## 4. Run locally
```bash
go run .
```
In Slack, invite the bot to a channel and mention it:
```
@YourBot what's our app's current status?
@YourBot show me cloudflare traffic
```
Then just reply in the thread (no need to @mention again) to continue the conversation.

## 5. Notes / next steps
- **ASC sales reports**: the current `appstoreconnect` client only fetches
  app metadata (`/v1/apps`) as a smoke test. Apple's actual *Sales and Trends
  Reports* API returns gzipped TSV files
- **Cloudflare**: currently pulls 7-day request/bytes/threats totals via the
  GraphQL Analytics API for `CF_ZONE_ID`.
- **Context window**: thread history is sent to the LLM in full with no
  trimming/token budgeting yet — fine for testing, but long threads will
  eventually exceed the model's context window.
- Secrets are loaded from `.env` via `godotenv`; never commit `.env`.
