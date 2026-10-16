package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestUnpinArchivedChatSessionsRenumberKeepsTheData(t *testing.T) {
	t.Parallel()
	const version = "653_unpin_archived_chat_sessions"
	const legacyVersion = "627_unpin_archived_chat_sessions"

	for _, previouslyApplied := range []bool{false, true} {
		t.Run(fmt.Sprintf("previously_applied_%t", previouslyApplied), func(t *testing.T) {
			t.Parallel()
			admin := openTestPool(t)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			schema := fmt.Sprintf("migrate_unpin_%d_%d", time.Now().UnixNano(), rand.Uint32())
			ident := pgx.Identifier{schema}.Sanitize()
			if _, err := admin.Exec(ctx, "CREATE SCHEMA "+ident); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if _, err := admin.Exec(cleanupCtx, "DROP SCHEMA "+ident+" CASCADE"); err != nil {
					t.Errorf("cleanup schema: %v", err)
				}
			})
			pool := openTestPoolWithSearchPath(t, schema)
			for _, stmt := range []string{
				`CREATE TABLE schema_migrations (version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ DEFAULT now())`,
				`CREATE TABLE chat_session (id TEXT PRIMARY KEY, status TEXT NOT NULL, pinned_at TIMESTAMPTZ)`,
				`INSERT INTO chat_session VALUES ('active-pinned', 'active', now()), ('archived-pinned', 'archived', now()), ('archived-clear', 'archived', NULL)`,
			} {
				if _, err := pool.Exec(ctx, stmt); err != nil {
					t.Fatal(err)
				}
			}
			files := realMigrationFiles(t, []string{version}, "up")
			if previouslyApplied {
				sql, err := os.ReadFile(files[0])
				if err != nil {
					t.Fatal(err)
				}
				if _, err := pool.Exec(ctx, string(sql)); err != nil {
					t.Fatal(err)
				}
				if _, err := pool.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", legacyVersion); err != nil {
					t.Fatal(err)
				}
			}
			options := runOptions{
				Direction: "up", Files: files,
				SchemaMigrationsTable: schema + ".schema_migrations",
				AdvisoryLockKey:       int64(rand.Uint64()&0x7fffffffffffffff) | 1,
				Hooks:                 hooksForDirection("up"),
			}
			for range 2 {
				if err := runMigrations(ctx, pool, options); err != nil {
					t.Fatal(err)
				}
			}
			var recorded bool
			if err := pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)", version).Scan(&recorded); err != nil || !recorded {
				t.Fatalf("new version recorded = %t, error = %v", recorded, err)
			}
			var activePins, archivedPins int
			if err := pool.QueryRow(ctx, `SELECT
				count(*) FILTER (WHERE status = 'active' AND pinned_at IS NOT NULL),
				count(*) FILTER (WHERE status = 'archived' AND pinned_at IS NOT NULL)
				FROM chat_session`).Scan(&activePins, &archivedPins); err != nil || activePins != 1 || archivedPins != 0 {
				t.Fatalf("active pins = %d (want 1), archived pins = %d (want 0), error = %v", activePins, archivedPins, err)
			}
			if previouslyApplied {
				if err := pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)", legacyVersion).Scan(&recorded); err != nil || !recorded {
					t.Fatalf("legacy version preserved = %t, error = %v", recorded, err)
				}
			}
		})
	}
}
