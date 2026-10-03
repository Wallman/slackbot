// Package store persists Slack thread conversations (and which threads the
// bot is participating in) to Postgres, so history survives restarts and
// the bot can recognize thread replies even without being re-@mentioned.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver

	"slackbot/llm"
)

// Store wraps a Postgres connection pool for thread persistence.
type Store struct {
	db *sql.DB
}

// Open connects to Postgres using the given DSN (e.g.
// "postgres://user:pass@localhost:5432/slackbot?sslmode=disable") and
// verifies connectivity.
func Open(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS threads (
			channel        TEXT NOT NULL,
			thread_ts      TEXT NOT NULL,
			last_active_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			PRIMARY KEY (channel, thread_ts)
		);

		CREATE TABLE IF NOT EXISTS thread_messages (
			id           BIGSERIAL PRIMARY KEY,
			channel      TEXT NOT NULL,
			thread_ts    TEXT NOT NULL,
			role         TEXT NOT NULL,
			content      TEXT,
			tool_calls   JSONB,
			tool_call_id TEXT,
			created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
			FOREIGN KEY (channel, thread_ts) REFERENCES threads (channel, thread_ts)
		);

		CREATE INDEX IF NOT EXISTS thread_messages_channel_thread_ts_created_at_idx
			ON thread_messages (channel, thread_ts, created_at);
	`)
	if err != nil {
		return fmt.Errorf("migrate schema: %w", err)
	}
	return nil
}

// EnsureThread records that a thread exists (or bumps its last_active_at if
// it already does). Call this whenever the bot is mentioned or replies in a
// thread, so IsKnownThread can later recognize follow-up replies in it.
func (s *Store) EnsureThread(ctx context.Context, channel, threadTS string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO threads (channel, thread_ts, last_active_at)
		VALUES ($1, $2, now())
		ON CONFLICT (channel, thread_ts)
		DO UPDATE SET last_active_at = now()
	`, channel, threadTS)
	if err != nil {
		return fmt.Errorf("ensure thread: %w", err)
	}
	return nil
}

func (s *Store) IsKnownThread(ctx context.Context, channel, threadTS string) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM threads WHERE channel = $1 AND thread_ts = $2)
	`, channel, threadTS).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check known thread: %w", err)
	}
	return exists, nil
}

// AppendMessage persists a single conversation message for a thread.
func (s *Store) AppendMessage(ctx context.Context, channel, threadTS string, msg llm.Message) error {
	var toolCallsJSON []byte
	if len(msg.ToolCalls) > 0 {
		var err error
		toolCallsJSON, err = json.Marshal(msg.ToolCalls)
		if err != nil {
			return fmt.Errorf("marshal tool calls: %w", err)
		}
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO thread_messages (channel, thread_ts, role, content, tool_calls, tool_call_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, now())
	`, channel, threadTS, msg.Role, nullableString(msg.Content), toolCallsJSON, nullableString(msg.ToolCallID))
	if err != nil {
		return fmt.Errorf("append message: %w", err)
	}
	return nil
}

// LoadHistory returns the full stored conversation for a thread, in
// chronological order.
func (s *Store) LoadHistory(ctx context.Context, channel, threadTS string) ([]llm.Message, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT role, content, tool_calls, tool_call_id
		FROM thread_messages
		WHERE channel = $1 AND thread_ts = $2
		ORDER BY created_at, id
	`, channel, threadTS)
	if err != nil {
		return nil, fmt.Errorf("load history: %w", err)
	}
	defer rows.Close()

	var history []llm.Message
	for rows.Next() {
		var (
			msg           llm.Message
			content       sql.NullString
			toolCallsJSON []byte
			toolCallID    sql.NullString
		)
		if err := rows.Scan(&msg.Role, &content, &toolCallsJSON, &toolCallID); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		msg.Content = content.String
		msg.ToolCallID = toolCallID.String
		if len(toolCallsJSON) > 0 {
			if err := json.Unmarshal(toolCallsJSON, &msg.ToolCalls); err != nil {
				return nil, fmt.Errorf("unmarshal tool calls: %w", err)
			}
		}
		history = append(history, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate history: %w", err)
	}
	return history, nil
}

// nullableString converts an empty string to a SQL NULL so empty Content
// (e.g. on assistant tool-call messages) is stored as NULL rather than "".
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// PruneOlderThan deletes threads (and their messages, via FK cascade rules
// enforced at the application level here) that have been inactive longer
// than the given duration. Call periodically to bound storage growth.
func (s *Store) PruneOlderThan(ctx context.Context, age time.Duration) error {
	cutoff := time.Now().Add(-age)
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM thread_messages WHERE (channel, thread_ts) IN (
			SELECT channel, thread_ts FROM threads WHERE last_active_at < $1
		)
	`, cutoff)
	if err != nil {
		return fmt.Errorf("prune old messages: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM threads WHERE last_active_at < $1`, cutoff)
	if err != nil {
		return fmt.Errorf("prune old threads: %w", err)
	}
	return nil
}
