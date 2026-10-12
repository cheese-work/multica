package migrations

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	dbfx "github.com/multica-ai/multica/server/internal/testutil"
)

func TestChannelOriginMigration(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "channel_origin_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, "DROP SCHEMA "+quotedSchema+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	fixture := dbfx.New(pool, "", "")
	fixture.Exec(t, `
		CREATE TABLE chat_session (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), status text);
		CREATE TABLE channel_chat_session_binding (
			id uuid PRIMARY KEY DEFAULT gen_random_uuid(), chat_session_id uuid, created_at timestamptz
		);
		CREATE TABLE chat_message (
			id uuid PRIMARY KEY DEFAULT gen_random_uuid(), chat_session_id uuid,
			channel_ingested boolean, created_at timestamptz
		);
	`)
	early := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	late := early.Add(time.Hour)
	bound := fixture.Insert(t, "chat_session", dbfx.Cols{"status": "active"})
	unbound := fixture.Insert(t, "chat_session", dbfx.Cols{"status": "archived"})
	both := fixture.Insert(t, "chat_session", dbfx.Cols{"status": "active"})
	web := fixture.Insert(t, "chat_session", dbfx.Cols{"status": "active"})
	fixture.Insert(t, "channel_chat_session_binding", dbfx.Cols{"chat_session_id": bound, "created_at": early})
	fixture.Insert(t, "channel_chat_session_binding", dbfx.Cols{"chat_session_id": both, "created_at": late})
	for _, sessionID := range []string{unbound, both} {
		fixture.Insert(t, "chat_message", dbfx.Cols{
			"chat_session_id": sessionID, "channel_ingested": true, "created_at": late,
		})
		fixture.Insert(t, "chat_message", dbfx.Cols{
			"chat_session_id": sessionID, "channel_ingested": true, "created_at": early,
		})
	}
	fixture.Insert(t, "chat_message", dbfx.Cols{
		"chat_session_id": web, "channel_ingested": false, "created_at": early,
	})
	fixture.Exec(t, readMigrationFile(t, "595_chat_session_channel_origin.up.sql"))
	for _, sessionID := range []string{bound, unbound, both} {
		var origin time.Time
		fixture.QueryRow(t, `SELECT channel_origin_at FROM chat_session WHERE id = $1`, sessionID).Scan(&origin)
		if !origin.Equal(early) {
			t.Fatalf("session %s: origin=%v, want earliest evidence=%v", sessionID, origin, early)
		}
	}
	var webOrigin pgtype.Timestamptz
	fixture.QueryRow(t, `SELECT channel_origin_at FROM chat_session WHERE id = $1`, web).Scan(&webOrigin)
	if webOrigin.Valid {
		t.Fatal("web-only messages must not establish channel origin")
	}
	for _, replacement := range []any{nil, late} {
		_, err := pool.Exec(ctx, `UPDATE chat_session SET channel_origin_at = $2 WHERE id = $1`, bound, replacement)
		var pgError *pgconn.PgError
		if !errors.As(err, &pgError) || pgError.Code != "23514" {
			t.Fatalf("changing established origin to %v must be rejected: %v", replacement, err)
		}
	}
	fixture.Exec(t, `UPDATE chat_session SET channel_origin_at = channel_origin_at, status = 'archived' WHERE id = $1`, bound)
	fixture.Exec(t, `UPDATE chat_session SET channel_origin_at = $2 WHERE id = $1`, web, early)
	fixture.Exec(t, readMigrationFile(t, "595_chat_session_channel_origin.down.sql"))
	if count := fixture.Count(t, `SELECT count(*) FROM chat_session`); count != 4 {
		t.Fatalf("rollback lost sessions: count=%d", count)
	}
	fixture.Exec(t, readMigrationFile(t, "595_chat_session_channel_origin.up.sql"))
}
