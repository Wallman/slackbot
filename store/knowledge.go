package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Chunk is one indexed unit of Slack content (a thread or a lone message).
type Chunk struct {
	Channel   string
	ThreadTS  string
	Text      string
	Permalink string
	CreatedAt time.Time
	Model     string
	Embedding []float32
}

// SearchResult is a chunk returned by vector search.
type SearchResult struct {
	Channel   string
	Text      string
	Permalink string
	CreatedAt time.Time
	Distance  float64
}

// MigrateKnowledge enables pgvector and creates the knowledge table. The
// embedding column has no fixed dimension so the model can be swapped; add an
// HNSW index (which needs a fixed dimension) once the corpus grows.
func (s *Store) MigrateKnowledge(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		CREATE EXTENSION IF NOT EXISTS vector;

		CREATE TABLE IF NOT EXISTS knowledge_chunks (
			channel     TEXT NOT NULL,
			thread_ts   TEXT NOT NULL,
			text        TEXT NOT NULL,
			permalink   TEXT NOT NULL DEFAULT '',
			created_at  TIMESTAMPTZ NOT NULL,
			embed_model TEXT NOT NULL,
			embedding   vector NOT NULL,
			PRIMARY KEY (channel, thread_ts)
		);
	`)
	if err != nil {
		return fmt.Errorf("migrate knowledge schema: %w", err)
	}
	return nil
}

func vectorLiteral(v []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

// UpsertChunk inserts or replaces the chunk for (channel, thread_ts).
func (s *Store) UpsertChunk(ctx context.Context, c Chunk) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO knowledge_chunks (channel, thread_ts, text, permalink, created_at, embed_model, embedding)
		VALUES ($1, $2, $3, $4, $5, $6, $7::vector)
		ON CONFLICT (channel, thread_ts) DO UPDATE SET
			text = EXCLUDED.text,
			permalink = EXCLUDED.permalink,
			created_at = EXCLUDED.created_at,
			embed_model = EXCLUDED.embed_model,
			embedding = EXCLUDED.embedding
	`, c.Channel, c.ThreadTS, c.Text, c.Permalink, c.CreatedAt, c.Model, vectorLiteral(c.Embedding))
	if err != nil {
		return fmt.Errorf("upsert chunk: %w", err)
	}
	return nil
}

// SearchChunks returns the limit nearest chunks by cosine distance, only
// considering chunks embedded with the given model.
func (s *Store) SearchChunks(ctx context.Context, model string, query []float32, limit int) ([]SearchResult, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT channel, text, permalink, created_at, embedding <=> $1::vector AS dist
		FROM knowledge_chunks
		WHERE embed_model = $2
		ORDER BY dist
		LIMIT $3
	`, vectorLiteral(query), model, limit)
	if err != nil {
		return nil, fmt.Errorf("search chunks: %w", err)
	}
	defer rows.Close()

	var out []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(&r.Channel, &r.Text, &r.Permalink, &r.CreatedAt, &r.Distance); err != nil {
			return nil, fmt.Errorf("scan chunk: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// HasChunks reports whether the knowledge table contains any rows.
func (s *Store) HasChunks(ctx context.Context) (bool, error) {
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM knowledge_chunks)`).Scan(&exists); err != nil {
		return false, fmt.Errorf("check knowledge chunks: %w", err)
	}
	return exists, nil
}
