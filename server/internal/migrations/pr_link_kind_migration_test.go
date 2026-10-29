package migrations

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestPRLinkKindMigration(test *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		test.Skip("integration test requires DATABASE_URL")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		test.Fatal(err)
	}
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		CREATE TEMP TABLE issue_pull_request (issue_id text, pull_request_id text);
		INSERT INTO issue_pull_request VALUES ('legacy-issue', 'legacy-pr');
	`); err != nil {
		test.Fatal(err)
	}
	for range 2 {
		applyMigrationFile(test, ctx, tx, "775_issue_pull_request_link_kind.up.sql")
	}
	var kind string
	if err := tx.QueryRow(ctx, `SELECT link_kind FROM issue_pull_request`).Scan(&kind); err != nil {
		test.Fatal(err)
	}
	if kind != "closing" {
		test.Fatalf("legacy link kind = %q, want closing", kind)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO issue_pull_request VALUES ('ref-issue', 'ref-pr', 'reference')`); err != nil {
		test.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SAVEPOINT invalid_kind"); err != nil {
		test.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE issue_pull_request SET link_kind = 'unknown'`); !isCheckViolation(err) {
		test.Fatalf("invalid link kind error = %v, want check violation", err)
	}
	if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT invalid_kind"); err != nil {
		test.Fatal(err)
	}
	for range 2 {
		applyMigrationFile(test, ctx, tx, "775_issue_pull_request_link_kind.down.sql")
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM issue_pull_request`).Scan(&count); err != nil {
		test.Fatal(err)
	}
	if count != 2 {
		test.Fatalf("links after rollback = %d, want 2", count)
	}
	applyMigrationFile(test, ctx, tx, "775_issue_pull_request_link_kind.up.sql")
}
