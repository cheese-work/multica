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

func TestSquadLeaderFailureMarkerRenumberPreservesFunction(t *testing.T) {
	t.Parallel()
	const version = "614_squad_leader_failure_marker"
	const legacyVersion = "593_squad_leader_failure_marker"

	for _, previouslyApplied := range []bool{false, true} {
		t.Run(fmt.Sprintf("previously_applied_%t", previouslyApplied), func(t *testing.T) {
			t.Parallel()
			admin := openTestPool(t)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			schema := fmt.Sprintf("migrate_leader_marker_%d_%d", time.Now().UnixNano(), rand.Uint32())
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
			if _, err := pool.Exec(ctx, `CREATE TABLE schema_migrations (
				version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ DEFAULT now()
			)`); err != nil {
				t.Fatal(err)
			}
			files := realMigrationFiles(t, []string{version}, "up")
			var originalOID uint32
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
				if err := pool.QueryRow(ctx, "SELECT 'capture_issue_collaboration_wakeup()'::regprocedure::oid").Scan(&originalOID); err != nil {
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
			if previouslyApplied {
				var currentOID uint32
				if err := pool.QueryRow(ctx, "SELECT 'capture_issue_collaboration_wakeup()'::regprocedure::oid").Scan(&currentOID); err != nil {
					t.Fatal(err)
				}
				if currentOID != originalOID {
					t.Fatalf("function identity changed: before=%d after=%d", originalOID, currentOID)
				}
				if err := pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)", legacyVersion).Scan(&recorded); err != nil || !recorded {
					t.Fatalf("legacy version preserved = %t, error = %v", recorded, err)
				}
			}
		})
	}
}
