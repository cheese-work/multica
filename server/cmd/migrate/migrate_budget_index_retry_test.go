package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type budgetIndexRetryCase struct {
	version      string
	indexName    string
	tableName    string
	duplicateKey string
}

var budgetIndexRetryCases = []budgetIndexRetryCase{
	{
		version:      "555_governance_budget_reservation_receipt_idx",
		indexName:    "governance_budget_reservation_receipt_uidx",
		tableName:    "governance_budget_reservation",
		duplicateKey: "receipt",
	},
	{
		version:   "556_governance_budget_reservation_root_window_idx",
		indexName: "governance_budget_reservation_root_window_idx",
		tableName: "governance_budget_reservation",
	},
	{
		version:   "557_governance_budget_outbox_due_idx",
		indexName: "governance_budget_outbox_due_idx",
		tableName: "governance_budget_outbox",
	},
	{
		version:      "558_governance_budget_reservation_attempt_obligation_uidx",
		indexName:    "governance_budget_reservation_attempt_obligation_uidx",
		tableName:    "governance_budget_reservation",
		duplicateKey: "attempt-obligation",
	},
	{
		version:   "572_governance_budget_window_overlap_idx",
		indexName: "governance_budget_window_overlap_idx",
		tableName: "governance_budget_window",
	},
	{
		version:   "573_governance_budget_reservation_open_window_idx",
		indexName: "governance_budget_reservation_open_window_idx",
		tableName: "governance_budget_reservation",
	},
}

func TestBudgetIndexRetry(t *testing.T) {
	for _, testCase := range budgetIndexRetryCases {
		testCase := testCase
		t.Run(testCase.version, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			schema, pool := newBudgetIndexRetryFixture(t, ctx)
			if testCase.duplicateKey != "" {
				if err := insertBudgetReservations(ctx, pool, "00000000-0000-0000-0000-000000000021", testCase.duplicateKey, 2); err != nil {
					t.Fatalf("seed conflicting reservation rows: %v", err)
				}
			} else if testCase.tableName == "governance_budget_reservation" {
				if err := insertBudgetReservations(ctx, pool, "00000000-0000-0000-0000-000000000021", "", 1); err != nil {
					t.Fatalf("seed reservation row: %v", err)
				}
			} else if testCase.tableName == "governance_budget_window" {
				if err := insertBudgetWindowRow(ctx, pool, "00000000-0000-0000-0000-000000000021"); err != nil {
					t.Fatalf("seed budget window row: %v", err)
				}
			} else if err := insertBudgetOutboxRow(ctx, pool, "00000000-0000-0000-0000-000000000021", "seed"); err != nil {
				t.Fatalf("seed outbox row: %v", err)
			}

			if testCase.duplicateKey != "" {
				assertBudgetIndexBuildFails(t, ctx, pool, schema, testCase)
				if err := runBudgetMigration(t, ctx, pool, schema, testCase.version, nil); err == nil {
					t.Fatal("retry without cleanup hook accepted the invalid index")
				}
				assertIndexValidity(t, pool, schema, testCase.indexName, false)
				assertMigrationVersionRecorded(t, ctx, pool, schema, testCase.version, false)

				if err := runBudgetMigration(t, ctx, pool, schema, testCase.version, hooksForDirection("up")); err == nil {
					t.Fatal("retry with persistent duplicates unexpectedly succeeded")
				}
				assertIndexValidity(t, pool, schema, testCase.indexName, false)
				assertMigrationVersionRecorded(t, ctx, pool, schema, testCase.version, false)
				if _, err := pool.Exec(ctx, `
					DELETE FROM governance_budget_reservation
					WHERE ctid = (
						SELECT ctid FROM governance_budget_reservation
						WHERE workspace_id = $1 LIMIT 1
					)
				`, "00000000-0000-0000-0000-000000000021"); err != nil {
					t.Fatalf("remove one conflicting fixture row: %v", err)
				}
			} else {
				createInvalidBudgetIndex(t, ctx, pool, testCase, "00000000-0000-0000-0000-000000000021")
				assertIndexValidity(t, pool, schema, testCase.indexName, false)

				if err := runBudgetMigration(t, ctx, pool, schema, testCase.version, nil); err == nil {
					t.Fatal("retry without cleanup hook accepted the invalid index")
				}
				assertIndexValidity(t, pool, schema, testCase.indexName, false)
				assertMigrationVersionRecorded(t, ctx, pool, schema, testCase.version, false)
			}

			if err := runBudgetMigration(t, ctx, pool, schema, testCase.version, hooksForDirection("up")); err != nil {
				t.Fatalf("retry with production cleanup hook: %v", err)
			}
			assertIndexValidity(t, pool, schema, testCase.indexName, true)
			assertMigrationVersionRecorded(t, ctx, pool, schema, testCase.version, true)
			assertBudgetLedgerCount(t, ctx, pool, schema, testCase.version, 1)

			var indexOID uint32
			if err := pool.QueryRow(ctx, `
				SELECT c.oid FROM pg_class AS c
				JOIN pg_namespace AS n ON n.oid = c.relnamespace
				WHERE n.nspname = $1 AND c.relname = $2 AND c.relkind = 'i'
			`, schema, testCase.indexName).Scan(&indexOID); err != nil {
				t.Fatalf("read built index OID: %v", err)
			}
			var rowCount int
			if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{schema, testCase.tableName}.Sanitize()).Scan(&rowCount); err != nil {
				t.Fatalf("count preserved fixture rows: %v", err)
			}
			if err := runBudgetMigration(t, ctx, pool, schema, testCase.version, hooksForDirection("up")); err != nil {
				t.Fatalf("duplicate migration execution: %v", err)
			}
			assertBudgetLedgerCount(t, ctx, pool, schema, testCase.version, 1)
			var duplicateIndexOID uint32
			if err := pool.QueryRow(ctx, `
				SELECT c.oid FROM pg_class AS c
				JOIN pg_namespace AS n ON n.oid = c.relnamespace
				WHERE n.nspname = $1 AND c.relname = $2 AND c.relkind = 'i'
			`, schema, testCase.indexName).Scan(&duplicateIndexOID); err != nil {
				t.Fatalf("read index OID after duplicate execution: %v", err)
			}
			if duplicateIndexOID != indexOID {
				t.Fatalf("duplicate execution replaced index OID %d with %d", indexOID, duplicateIndexOID)
			}
			var duplicateRowCount int
			if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{schema, testCase.tableName}.Sanitize()).Scan(&duplicateRowCount); err != nil {
				t.Fatalf("count rows after duplicate execution: %v", err)
			}
			if duplicateRowCount != rowCount {
				t.Fatalf("duplicate execution changed fixture rows from %d to %d", rowCount, duplicateRowCount)
			}
		})
	}
}

func TestBudgetIndexRecordedVersionRequiresUsableIndex(t *testing.T) {
	for _, scenario := range []string{"missing", "invalid", "wrong_definition"} {
		scenario := scenario
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			schema, pool := newBudgetIndexRetryFixture(t, ctx)
			const version = "555_governance_budget_reservation_receipt_idx"
			if err := runBudgetMigration(t, ctx, pool, schema, version, hooksForDirection("up")); err != nil {
				t.Fatalf("apply receipt index migration: %v", err)
			}
			indexIdent := pgx.Identifier{schema, "governance_budget_reservation_receipt_uidx"}.Sanitize()
			if _, err := pool.Exec(ctx, "DROP INDEX CONCURRENTLY "+indexIdent); err != nil {
				t.Fatalf("drop receipt index for %s case: %v", scenario, err)
			}

			switch scenario {
			case "invalid":
				if err := insertBudgetReservations(ctx, pool, "00000000-0000-0000-0000-000000000031", "receipt", 2); err != nil {
					t.Fatalf("seed duplicate receipt rows: %v", err)
				}
				path := realMigrationFiles(t, []string{version}, "up")[0]
				migrationSQL, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read receipt index migration: %v", err)
				}
				if _, err := pool.Exec(ctx, string(migrationSQL)); err == nil {
					t.Fatal("duplicate receipt index build unexpectedly succeeded")
				}
				assertIndexValidity(t, pool, schema, "governance_budget_reservation_receipt_uidx", false)
			case "wrong_definition":
				if _, err := pool.Exec(ctx, "CREATE UNIQUE INDEX CONCURRENTLY "+pgx.Identifier{"governance_budget_reservation_receipt_uidx"}.Sanitize()+" ON governance_budget_reservation (reservation_id)"); err != nil {
					t.Fatalf("create wrong-shape valid index: %v", err)
				}
			}

			if err := runBudgetMigration(t, ctx, pool, schema, version, hooksForDirection("up")); err == nil {
				t.Fatalf("recorded migration accepted %s index", scenario)
			}
			assertMigrationVersionRecorded(t, ctx, pool, schema, version, true)
		})
	}
}

func TestBudgetIndexRejectsRelationCollision(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	schema, pool := newBudgetIndexRetryFixture(t, ctx)
	const version = "557_governance_budget_outbox_due_idx"
	if _, err := pool.Exec(ctx, "CREATE TABLE governance_budget_outbox_due_idx (id BIGINT PRIMARY KEY)"); err != nil {
		t.Fatalf("create non-index collision: %v", err)
	}
	if err := runBudgetMigration(t, ctx, pool, schema, version, hooksForDirection("up")); err == nil {
		t.Fatal("migration accepted a non-index relation collision")
	}
	assertMigrationVersionRecorded(t, ctx, pool, schema, version, false)
	var tableExists bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = $1 AND c.relname = 'governance_budget_outbox_due_idx' AND c.relkind = 'r'
		)
	`, schema).Scan(&tableExists); err != nil {
		t.Fatalf("verify collision relation remains: %v", err)
	}
	if !tableExists {
		t.Fatal("migration removed the non-index collision relation")
	}
}

func newBudgetIndexRetryFixture(t *testing.T, ctx context.Context) (string, *pgxpool.Pool) {
	t.Helper()
	adminPool := openTestPool(t)
	schema := createScratchSchema(t, ctx, adminPool, "migrate_budget_retry_")
	pool := openBudgetIndexRetryPool(t, schema)
	if err := runBudgetMigration(t, ctx, pool, schema, "554_governance_budget", hooksForDirection("up")); err != nil {
		t.Fatalf("apply budget table migration: %v", err)
	}
	return schema, pool
}

func openBudgetIndexRetryPool(t *testing.T, schema string) *pgxpool.Pool {
	t.Helper()
	config, err := pgxpool.ParseConfig(testDatabaseURL())
	if err != nil {
		t.Fatalf("parse test database URL: %v", err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open budget retry pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping budget retry pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func runBudgetMigration(t *testing.T, ctx context.Context, pool *pgxpool.Pool, schema, version string, hooks map[string]preMigrationHook) error {
	t.Helper()
	return runMigrations(ctx, pool, runOptions{
		Direction:             "up",
		Files:                 realMigrationFiles(t, []string{version}, "up"),
		SchemaMigrationsTable: schema + ".schema_migrations",
		AdvisoryLockKey:       migrationAdvisoryLockKey,
		Hooks:                 hooks,
	})
}

func assertBudgetIndexBuildFails(t *testing.T, ctx context.Context, pool *pgxpool.Pool, schema string, testCase budgetIndexRetryCase) {
	t.Helper()
	if err := runBudgetMigration(t, ctx, pool, schema, testCase.version, hooksForDirection("up")); err == nil {
		t.Fatal("concurrent unique index build with duplicate rows unexpectedly succeeded")
	}
	assertIndexValidity(t, pool, schema, testCase.indexName, false)
	assertMigrationVersionRecorded(t, ctx, pool, schema, testCase.version, false)
}

type budgetExec interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func insertBudgetReservations(ctx context.Context, exec budgetExec, workspaceID, duplicateKey string, count int) error {
	_, err := exec.Exec(ctx, `
		INSERT INTO governance_budget_reservation (
			workspace_id, reservation_id, budget_root_id, case_id, attempt_id, obligation_id,
			resource, control_epoch, window_start, window_end, root_cap_micro_usd,
			window_cap_micro_usd, max_attempt_cost_micro_usd, retry_allowance,
			retry_policy_bounded, retry_allowance_remaining, total_cap_micro_usd,
			remaining_micro_usd, request_digest, settlement_receipt_id,
			state, termination_known, settled_at
		)
		SELECT $1, gen_random_uuid(), '00000000-0000-0000-0000-000000000041',
		       gen_random_uuid(),
		       CASE WHEN $2 = 'attempt-obligation' THEN '00000000-0000-0000-0000-000000000042'::uuid ELSE gen_random_uuid() END,
		       CASE WHEN $2 = 'attempt-obligation' THEN '00000000-0000-0000-0000-000000000043'::uuid ELSE gen_random_uuid() END,
		       'index-retry-test', 0, now(), now() + interval '1 hour',
		       1000000, 1000000, 500000, 0, TRUE, 0, 500000,
		       CASE WHEN $2 = 'receipt' THEN 0 ELSE 500000 END, repeat('c', 64),
		       CASE WHEN $2 = 'receipt' THEN 'duplicate-receipt' ELSE NULL END,
		       CASE WHEN $2 = 'receipt' THEN 'settled' ELSE 'reserved' END,
		       ($2 = 'receipt'), CASE WHEN $2 = 'receipt' THEN now() ELSE NULL END
		FROM generate_series(1, $3::integer)
	`, workspaceID, duplicateKey, count)
	return err
}

func insertBudgetOutboxRow(ctx context.Context, exec budgetExec, workspaceID, eventKey string) error {
	_, err := exec.Exec(ctx, `
		INSERT INTO governance_budget_outbox (
			workspace_id, reservation_id, event_key, event_type,
			case_id, attempt_id, obligation_id, payload
		)
		VALUES ($1, gen_random_uuid(), $2, 'admit', gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), '{}')
	`, workspaceID, eventKey)
	return err
}

func insertBudgetWindowRow(ctx context.Context, exec budgetExec, workspaceID string) error {
	_, err := exec.Exec(ctx, `
		INSERT INTO governance_budget_window (workspace_id, window_start, window_end, spend_cap_micro_usd)
		VALUES ($1, $2::timestamptz, $2::timestamptz + interval '1 hour', 1000000)
	`, workspaceID, time.Now().UTC())
	return err
}

func holdBudgetIndexBuild(t *testing.T, ctx context.Context, pool *pgxpool.Pool, testCase budgetIndexRetryCase, workspaceID string) func() {
	t.Helper()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire concurrent-build blocker: %v", err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		conn.Release()
		t.Fatalf("begin concurrent-build blocker: %v", err)
	}
	switch testCase.tableName {
	case "governance_budget_reservation":
		if err := insertBudgetReservations(ctx, tx, workspaceID, "", 1); err != nil {
			_ = tx.Rollback(ctx)
			conn.Release()
			t.Fatalf("insert concurrent-build blocker row: %v", err)
		}
	case "governance_budget_window":
		if err := insertBudgetWindowRow(ctx, tx, workspaceID); err != nil {
			_ = tx.Rollback(ctx)
			conn.Release()
			t.Fatalf("insert concurrent-build blocker window: %v", err)
		}
	case "governance_budget_outbox":
		if err := insertBudgetOutboxRow(ctx, tx, workspaceID, "build-blocker"); err != nil {
			_ = tx.Rollback(ctx)
			conn.Release()
			t.Fatalf("insert concurrent-build blocker outbox row: %v", err)
		}
	default:
		_ = tx.Rollback(ctx)
		conn.Release()
		t.Fatalf("unsupported budget index fixture table %q", testCase.tableName)
	}
	released := false
	release := func() {
		if released {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanupCtx)
		conn.Release()
		released = true
	}
	t.Cleanup(release)
	return release
}

func createInvalidBudgetIndex(t *testing.T, ctx context.Context, pool *pgxpool.Pool, testCase budgetIndexRetryCase, workspaceID string) {
	t.Helper()
	blocker := holdBudgetIndexBuild(t, ctx, pool, testCase, workspaceID)
	defer blocker()

	migrationSQL, err := os.ReadFile(realMigrationFiles(t, []string{testCase.version}, "up")[0])
	if err != nil {
		t.Fatalf("read migration %s: %v", testCase.version, err)
	}
	builder, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire migration builder: %v", err)
	}
	defer builder.Release()
	if _, err := builder.Exec(ctx, "SET statement_timeout = '2s'"); err != nil {
		t.Fatalf("set concurrent-build timeout: %v", err)
	}
	_, buildErr := builder.Exec(ctx, string(migrationSQL))
	if _, err := builder.Exec(ctx, "SET statement_timeout = DEFAULT"); err != nil {
		t.Logf("reset concurrent-build timeout: %v", err)
	}
	if buildErr == nil {
		t.Fatal("blocked concurrent index build unexpectedly succeeded")
	}
	blocker()
}

func assertBudgetLedgerCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, schema, version string, want int) {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM `+pgx.Identifier{schema, "schema_migrations"}.Sanitize()+` WHERE version = $1
	`, version).Scan(&count); err != nil {
		t.Fatalf("count ledger version %s: %v", version, err)
	}
	if count != want {
		t.Fatalf("ledger row count for %s = %d, want %d", version, count, want)
	}
}
