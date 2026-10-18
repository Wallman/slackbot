package main

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/slack-go/slack"

	"slackbot/llm"
	"slackbot/store"
)

const (
	maxChunkChars   = 6000
	searchResultCap = 5
)

// knowledgeIndexer turns Slack threads into embedded chunks stored in
// Postgres. A chunk is a top-level message plus its replies (or just the
// message if it has none).
type knowledgeIndexer struct {
	api      *slack.Client
	db       *store.Store
	embedder *llm.EmbeddingClient
	names    *userNameCache
	channels map[string]bool
}

func newKnowledgeIndexer(api *slack.Client, db *store.Store, embedder *llm.EmbeddingClient, names *userNameCache, channelIDs []string) *knowledgeIndexer {
	ch := make(map[string]bool, len(channelIDs))
	for _, id := range channelIDs {
		ch[id] = true
	}
	return &knowledgeIndexer{api: api, db: db, embedder: embedder, names: names, channels: ch}
}

func (k *knowledgeIndexer) indexes(channel string) bool {
	return k.channels[channel]
}

// indexThread (re)builds and stores the chunk for the thread rooted at rootTS.
func (k *knowledgeIndexer) indexThread(ctx context.Context, channel, rootTS string) error {
	msgs, _, _, err := k.api.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{
		ChannelID: channel,
		Timestamp: rootTS,
	})
	if err != nil {
		return fmt.Errorf("fetch thread %s/%s: %w", channel, rootTS, err)
	}
	return k.indexMessages(ctx, channel, rootTS, msgs)
}

func (k *knowledgeIndexer) indexMessages(ctx context.Context, channel, rootTS string, msgs []slack.Message) error {
	var lines []string
	for _, m := range msgs {
		if m.BotID != "" || m.User == "" || strings.TrimSpace(m.Text) == "" || m.SubType == "channel_join" || m.SubType == "channel_leave" {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s: %s", k.names.resolveName(k.api, m.User), m.Text))
	}
	if len(lines) == 0 {
		return nil
	}
	text := strings.Join(lines, "\n")
	if len(text) > maxChunkChars {
		text = text[:maxChunkChars]
	}

	vecs, err := k.embedder.Embed(ctx, []string{text})
	if err != nil {
		return err
	}

	permalink, err := k.api.GetPermalinkContext(ctx, &slack.PermalinkParameters{Channel: channel, Ts: rootTS})
	if err != nil {
		log.Printf("[knowledge] permalink for %s/%s: %v", channel, rootTS, err)
	}

	return k.db.UpsertChunk(ctx, store.Chunk{
		Channel:   channel,
		ThreadTS:  rootTS,
		Text:      text,
		Permalink: permalink,
		CreatedAt: tsToTime(rootTS),
		Model:     k.embedder.Model,
		Embedding: vecs[0],
	})
}

// backfill indexes the full history of every configured channel. It is
// idempotent, so it can be re-run safely.
func (k *knowledgeIndexer) backfill(ctx context.Context) error {
	var failed []string
	for channel := range k.channels {
		cursor := ""
		count := 0
		for {
			resp, err := k.api.GetConversationHistoryContext(ctx, &slack.GetConversationHistoryParameters{
				ChannelID: channel,
				Cursor:    cursor,
				Limit:     200,
			})
			if err != nil {
				log.Printf("[knowledge] history for %s: %v (is the bot invited to the channel?)", channel, err)
				failed = append(failed, channel)
				break
			}
			for _, m := range resp.Messages {
				var err error
				if m.ReplyCount > 0 {
					err = k.indexThread(ctx, channel, m.Timestamp)
				} else {
					err = k.indexMessages(ctx, channel, m.Timestamp, []slack.Message{m})
				}
				if err != nil {
					log.Printf("[knowledge] backfill %s/%s: %v", channel, m.Timestamp, err)
					continue
				}
				count++
			}
			if !resp.HasMore || resp.ResponseMetaData.NextCursor == "" {
				break
			}
			cursor = resp.ResponseMetaData.NextCursor
		}
		log.Printf("[knowledge] backfilled %d threads from %s", count, channel)
	}
	if len(failed) > 0 {
		return fmt.Errorf("channels failed: %s", strings.Join(failed, ", "))
	}
	return nil
}

// search embeds the query and returns the nearest chunks formatted for the LLM.
func (k *knowledgeIndexer) search(ctx context.Context, query string) string {
	vecs, err := k.embedder.Embed(ctx, []string{query})
	if err != nil {
		return fmt.Sprintf("error embedding query: %v", err)
	}
	results, err := k.db.SearchChunks(ctx, k.embedder.Model, vecs[0], searchResultCap)
	if err != nil {
		return fmt.Sprintf("error searching knowledge base: %v", err)
	}
	if len(results) == 0 {
		return "No relevant past discussions found."
	}
	var b strings.Builder
	for i, r := range results {
		fmt.Fprintf(&b, "[%d] %s (%s, distance %.3f)\n%s\n\n", i+1, r.Permalink, r.CreatedAt.Format("2006-01-02"), r.Distance, r.Text)
	}
	return b.String()
}

func tsToTime(ts string) time.Time {
	sec, _, _ := strings.Cut(ts, ".")
	n, err := strconv.ParseInt(sec, 10, 64)
	if err != nil {
		return time.Now()
	}
	return time.Unix(n, 0)
}
