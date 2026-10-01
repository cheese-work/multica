package main

import (
	"context"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestBudgetSchema(t *testing.T) {
	adminPool := openTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	schema := createScratchSchema(t, ctx, adminPool, "migrate_budget_schema_")
	pool := openTestPoolWithSearchPath(t, schema)
	versions := []string{
		"554_governance_budget",
		"555_governance_budget_reservation_receipt_idx",
		"556_governance_budget_reservation_root_window_idx",
		"557_governance_budget_outbox_due_idx",
		"558_governance_budget_reservation_attempt_obligation_uidx",
		"572_governance_budget_window_overlap_idx",
		"573_governance_budget_reservation_open_window_idx",
	}
	options := runOptions{
		Direction:             "up",
		Files:                 realMigrationFiles(t, versions, "up"),
		SchemaMigrationsTable: schema + ".schema_migrations",
		AdvisoryLockKey:       int64(rand.Uint64()&0x7fffffffffffffff) | 1,
		Hooks:                 hooksForDirection("up"),
	}
	if err := runMigrations(ctx, pool, options); err != nil {
		t.Fatalf("apply budget schema and index migrations: %v", err)
	}

	tables := []string{
		"governance_budget_window",
		"governance_budget_root",
		"governance_budget_reservation",
		"governance_budget_journal",
		"governance_budget_outbox",
	}
	var tableCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM information_schema.tables
		WHERE table_schema = $1 AND table_name = ANY($2)
	`, schema, tables).Scan(&tableCount); err != nil {
		t.Fatalf("count budget tables: %v", err)
	}
	if tableCount != len(tables) {
		t.Fatalf("budget table count = %d, want %d", tableCount, len(tables))
	}
	var foreignKeyCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM information_schema.table_constraints
		WHERE constraint_schema = $1 AND table_name = ANY($2) AND constraint_type = 'FOREIGN KEY'
	`, schema, tables).Scan(&foreignKeyCount); err != nil {
		t.Fatalf("count budget foreign keys: %v", err)
	}
	if foreignKeyCount != 0 {
		t.Fatalf("budget foreign key count = %d, want 0", foreignKeyCount)
	}

	for _, workspaceID := range []string{
		"00000000-0000-0000-0000-000000000011",
		"00000000-0000-0000-0000-000000000012",
	} {
		insertBudgetSchemaFixture(t, ctx, pool, workspaceID)
	}

	indexes := []struct {
		version   string
		name      string
		table     string
		unique    bool
		columns   []string
		predicate string
	}{
		{
			version:   "555_governance_budget_reservation_receipt_idx",
			name:      "governance_budget_reservation_receipt_uidx",
			table:     "governance_budget_reservation",
			unique:    true,
			columns:   []string{"workspace_id", "settlement_receipt_id"},
			predicate: "settlement_receipt_id IS NOT NULL",
		},
		{
			version: "556_governance_budget_reservation_root_window_idx",
			name:    "governance_budget_reservation_root_window_idx",
			table:   "governance_budget_reservation",
			columns: []string{"workspace_id", "budget_root_id", "window_start", "reservation_id"},
		},
		{
			version: "557_governance_budget_outbox_due_idx",
			name:    "governance_budget_outbox_due_idx",
			table:   "governance_budget_outbox",
			columns: []string{"workspace_id", "state", "created_at", "event_id"},
		},
		{
			version: "558_governance_budget_reservation_attempt_obligation_uidx",
			name:    "governance_budget_reservation_attempt_obligation_uidx",
			table:   "governance_budget_reservation",
			unique:  true,
			columns: []string{"workspace_id", "attempt_id", "obligation_id"},
		},
		{
			version: "572_governance_budget_window_overlap_idx",
			name:    "governance_budget_window_overlap_idx",
			table:   "governance_budget_window",
			columns: []string{"workspace_id", "window_end", "window_start"},
		},
		{
			version:   "573_governance_budget_reservation_open_window_idx",
			name:      "governance_budget_reservation_open_window_idx",
			table:     "governance_budget_reservation",
			columns:   []string{"workspace_id", "window_start"},
			predicate: "state = 'reserved'::text",
		},
	}
	indexOIDs := make(map[string]uint32, len(indexes))
	for _, index := range indexes {
		if _, ok := requiredConcurrentIndexes[index.version]; !ok {
			t.Fatalf("production index postcondition missing for %s", index.version)
		}
		var schemaName, tableName, method, predicate string
		var unique, valid, ready bool
		var columns []string
		var oid uint32
		if err := pool.QueryRow(ctx, `
			SELECT index_schema.nspname, table_class.relname, access_method.amname,
			       index_catalog.indisunique, index_catalog.indisvalid, index_catalog.indisready,
			       ARRAY(
			           SELECT pg_get_indexdef(index_catalog.indexrelid, key_position, FALSE)
			           FROM generate_series(1, index_catalog.indnkeyatts) AS key_position
			           ORDER BY key_position
			       ),
			       COALESCE(pg_get_expr(index_catalog.indpred, index_catalog.indrelid), ''),
			       index_class.oid
			FROM pg_index AS index_catalog
			JOIN pg_class AS index_class ON index_class.oid = index_catalog.indexrelid
			JOIN pg_namespace AS index_schema ON index_schema.oid = index_class.relnamespace
			JOIN pg_class AS table_class ON table_class.oid = index_catalog.indrelid
			JOIN pg_am AS access_method ON access_method.oid = index_class.relam
			WHERE index_schema.nspname = $1 AND index_class.relname = $2
		`, schema, index.name).Scan(&schemaName, &tableName, &method, &unique, &valid, &ready, &columns, &predicate, &oid); err != nil {
			t.Fatalf("inspect budget index %s: %v", index.name, err)
		}
		if schemaName != schema || tableName != index.table || method != "btree" || unique != index.unique || !valid || !ready {
			t.Fatalf("budget index %s shape: schema=%q table=%q method=%q unique=%v valid=%v ready=%v", index.name, schemaName, tableName, method, unique, valid, ready)
		}
		if !equalIndexColumns(columns, index.columns) || normalizeIndexPredicate(predicate) != normalizeIndexPredicate(index.predicate) {
			t.Fatalf("budget index %s keys/predicate: columns=%v predicate=%q", index.name, columns, predicate)
		}
		indexOIDs[index.name] = oid
	}

	if err := runMigrations(ctx, pool, options); err != nil {
		t.Fatalf("rerun budget migrations: %v", err)
	}
	for _, index := range indexes {
		var oid uint32
		if err := pool.QueryRow(ctx, `
			SELECT c.oid
			FROM pg_class AS c
			JOIN pg_namespace AS n ON n.oid = c.relnamespace
			WHERE n.nspname = $1 AND c.relname = $2 AND c.relkind = 'i'
		`, schema, index.name).Scan(&oid); err != nil {
			t.Fatalf("re-read budget index %s after duplicate migration run: %v", index.name, err)
		}
		if oid != indexOIDs[index.name] {
			t.Fatalf("budget index %s OID changed from %d to %d on duplicate run", index.name, indexOIDs[index.name], oid)
		}
	}
}

func insertBudgetSchemaFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool, workspaceID string) {
	t.Helper()
	statements := []struct {
		query string
		args  []any
	}{
		{
			query: `INSERT INTO governance_budget_window (workspace_id, window_start, window_end, spend_cap_micro_usd)
VALUES ($1, date_trunc('hour', now()), date_trunc('hour', now()) + interval '1 hour', 1000000)`,
			args: []any{workspaceID},
		},
		{
			query: `INSERT INTO governance_budget_root (workspace_id, budget_root_id, spend_cap_micro_usd)
VALUES ($1, '00000000-0000-0000-0000-000000000002', 1000000)`,
			args: []any{workspaceID},
		},
		{
			query: `INSERT INTO governance_budget_reservation (
workspace_id, reservation_id, budget_root_id, case_id, attempt_id, obligation_id, resource,
control_epoch, window_start, window_end, root_cap_micro_usd, window_cap_micro_usd,
max_attempt_cost_micro_usd, retry_allowance, retry_policy_bounded,
retry_allowance_remaining, total_cap_micro_usd, remaining_micro_usd, request_digest
)
VALUES (
$1, '00000000-0000-0000-0000-000000000003', '00000000-0000-0000-0000-000000000002',
'00000000-0000-0000-0000-000000000004', '00000000-0000-0000-0000-000000000005',
'00000000-0000-0000-0000-000000000006', 'schema-fixture', 0,
date_trunc('hour', now()), date_trunc('hour', now()) + interval '1 hour', 1000000, 1000000,
500000, 0, TRUE, 0, 500000, 500000, repeat('a', 64)
)`,
			args: []any{workspaceID},
		},
		{
			query: `INSERT INTO governance_budget_journal (
workspace_id, reservation_id, event_key, event_type, event_digest, expected_revision, resulting_revision
)
VALUES ($1, '00000000-0000-0000-0000-000000000003', 'schema-fixture', 'reserve', repeat('b', 64), 0, 1)`,
			args: []any{workspaceID},
		},
		{
			query: `INSERT INTO governance_budget_outbox (
workspace_id, event_id, reservation_id, event_key, event_type, case_id, attempt_id, obligation_id, payload
)
VALUES (
$1, '00000000-0000-0000-0000-000000000007', '00000000-0000-0000-0000-000000000003',
'schema-fixture', 'admit', '00000000-0000-0000-0000-000000000004',
'00000000-0000-0000-0000-000000000005', '00000000-0000-0000-0000-000000000006', '{}'
)`,
			args: []any{workspaceID},
		},
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("insert schema-only budget fixture: %v", err)
		}
	}
}
