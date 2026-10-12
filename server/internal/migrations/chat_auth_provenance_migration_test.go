package migrations

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestChatMessageSentViaMigration(test *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		test.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	connection, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		test.Fatal(err)
	}
	defer connection.Close(ctx)
	schema := "chat_auth_provenance_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := connection.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		test.Fatal(err)
	}
	defer connection.Exec(ctx, "DROP SCHEMA "+quotedSchema+" CASCADE")
	if _, err := connection.Exec(ctx, `SELECT set_config('search_path', $1, false)`, schema); err != nil {
		test.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `CREATE TABLE chat_message (id int PRIMARY KEY); INSERT INTO chat_message (id) VALUES (1)`); err != nil {
		test.Fatal(err)
	}
	if _, err := connection.Exec(ctx, readMigrationFile(test, "593_chat_message_auth_provenance.up.sql")); err != nil {
		test.Fatal(err)
	}
	var legacyIsNull bool
	if err := connection.QueryRow(ctx, `SELECT sent_via IS NULL FROM chat_message WHERE id = 1`).Scan(&legacyIsNull); err != nil {
		test.Fatal(err)
	}
	if !legacyIsNull {
		test.Fatal("migration must not attest existing messages")
	}
	for index, source := range []string{"interactive_jwt", "cli_jwt", "pat", "task_token", "cloud_pat", "channel", "system"} {
		if _, err := connection.Exec(ctx, `INSERT INTO chat_message (id, sent_via) VALUES ($1, $2)`, index+2, source); err != nil {
			test.Fatalf("allowed source %q: %v", source, err)
		}
	}
	if _, err := connection.Exec(ctx, `INSERT INTO chat_message (id, sent_via) VALUES (9, 'member')`); err == nil {
		test.Fatal("unrecognized provenance must be rejected")
	}
	if _, err := connection.Exec(ctx, readMigrationFile(test, "593_chat_message_auth_provenance.down.sql")); err != nil {
		test.Fatal(err)
	}
	var count int
	if err := connection.QueryRow(ctx, `SELECT count(*) FROM chat_message`).Scan(&count); err != nil || count != 8 {
		test.Fatalf("rollback must preserve all messages: count=%d, error=%v", count, err)
	}
}
