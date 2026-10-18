# Slackbot: App Store Connect + Cloudflare + LLM (OpenRouter, free tier)

A Slack bot that answers questions in Slack by pulling live
context from **App Store Connect** (download trends) and **Cloudflare**
(zone analytics), then asking a free LLM on **OpenRouter** to answer.

## Architecture
- `main.go` — entrypoint; Slack Socket Mode event loop
- `llm` — OpenRouter chat completions and embeddings clients
- `knowledge.go` — Slack knowledge memory: thread chunking, embedding, backfill, vector search
- `appstoreconnect` — JWT(ES256)-authenticated ASC API client
- `cloudflare` — Cloudflare GraphQL Analytics client
- `store` — Postgres-backed thread conversation history and pgvector knowledge chunks

When a user `@mentions` the bot, its message (plus a system prompt and the
thread's prior history) is sent to the LLM along with two **tool/function
definitions**: `get_app_downloads`, `get_cloudflare_traffic` and (when the
`search_knowledge`. The
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

### Slack knowledge memory (pgvector)
Semantic search over past Slack discussions. `KNOWLEDGE_CHANNELS` (required)
is a comma-separated list of channel IDs to index.
- **Chunking**: one chunk per top-level message plus its replies (a lone
  message is its own chunk). Bot messages are skipped, and chunks are
  truncated at 6000 characters.
- **Embeddings**: OpenRouter `/embeddings`, model set by `EMBEDDING_MODEL`
  (default `nvidia/nemotron-3-embed-1b:free`, 2048 dims). The model name is
  stored per row, and search only considers rows from the current model, so
  switching models requires re-running the backfill.
- **Storage/search**: `knowledge_chunks` table with an undimensioned
  `vector` column and cosine distance (`<=>`) search, top 5. There is no ANN
  index: an exact scan is fine for small corpora. pgvector's HNSW limit is
  2000 dims for `vector`, so for large corpora index a `halfvec` cast.
- **Ingestion**: on startup, if `knowledge_chunks` is empty, the bot
  backfills the full history of the configured channels in the background.
  To re-backfill (e.g. after changing `EMBEDDING_MODEL`), empty the table
  (`TRUNCATE knowledge_chunks`) and restart. New messages in those channels
  are indexed live. Edits and deletions are not handled yet.
- **Retrieval**: the model calls `search_knowledge(query)` and gets snippets
  with permalinks.

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
Starts Postgres (the `pgvector/pgvector:pg16` image, which includes the
`vector` extension) on `localhost:5432` (user/pass/db: `slackbot`), matching the
default `DATABASE_URL` in `.env.example`. The bot creates its own schema on
startup (`db.Migrate` and `db.MigrateKnowledge`) — no manual migration step needed. If you have an old volume created
from the `postgres:16-alpine` image, recreate it (`docker compose down -v`,
which deletes stored data): the Debian-based pgvector image uses a different libc.

## 3. Credentials
Copy `.env.example` to `.env` and fill in. Set
`KNOWLEDGE_CHANNELS` and invite the bot to those channels; history is
backfilled automatically on first start.

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
- **ASC downloads**: `get_app_downloads` queries Apple's *Sales and Trends
  Reports* API (`/v1/salesReports`, daily SUMMARY, gzipped TSV) for the
  hardcoded app ID, excluding `3`/`3F` (updates) and `7`/`7F` (in-app purchases)
  that share the same app ID.
- **Cloudflare**: currently pulls 7-day request/bytes/threats totals via the
  GraphQL Analytics API for `CF_ZONE_ID`.
- **Context window**: thread history is trimmed automatically once it
  estimated-exceeds 80% of `MAX_CONTEXT_TOKENS`. Trimming drops the oldest
  turns first (never splitting an assistant tool-call from its tool
  result, and always keeping the system prompt), using `tiktoken-go` for token estimation.
- Secrets are loaded from `.env` via `godotenv`; never commit `.env`.
