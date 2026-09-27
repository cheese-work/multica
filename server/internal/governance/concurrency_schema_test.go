package governance

import (
	"context"
	"strings"
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestConcurrencyWorkspaceLeadingIdentityIndexes(t *testing.T) {
	fixture := newConcurrencyFixture(t)
	ctx := context.Background()
	for _, expected := range []struct {
		index   string
		columns string
	}{
		{"governance_concurrency_guard_identity_uidx", "workspace_id, resource"},
		{"governance_concurrency_hold_identity_uidx", "workspace_id, resource, reservation_id"},
	} {
		var unique, valid bool
		var definition string
		if err := fixture.pool.QueryRow(ctx, `
			SELECT indisunique, indisvalid, pg_get_indexdef(indexrelid)
			FROM pg_index WHERE indexrelid = $1::regclass
		`, expected.index).Scan(&unique, &valid, &definition); err != nil {
			t.Fatal(err)
		}
		if !unique || !valid || !strings.HasSuffix(definition, "("+expected.columns+")") {
			t.Fatalf("invalid identity/workspace-leading index: %s (unique=%v, valid=%v)", definition, unique, valid)
		}
	}
	var foreignKeys int
	if err := fixture.pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_constraint WHERE contype = 'f'
		AND conrelid IN ('governance_concurrency_guard'::regclass, 'governance_concurrency_hold'::regclass)
	`).Scan(&foreignKeys); err != nil || foreignKeys != 0 {
		t.Fatalf("new concurrency tables contain foreign keys: %d/%v", foreignKeys, err)
	}
}

func TestConcurrencyRepresentativeExplain(t *testing.T) {
	fixture := newConcurrencyFixture(t)
	ctx := context.Background()
	tx, guards := fixture.lock(t, "jev")
	reservationID := concurrencyUUID()
	if _, err := guards.Reserve(ctx, "jev", reservationID, 1, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO governance_concurrency_guard (workspace_id, resource)
		SELECT $1, 'resource-' || ordinal FROM generate_series(1, 5000) AS ordinal
	`, fixture.workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO governance_concurrency_hold (workspace_id, resource, reservation_id)
		SELECT $1, 'jev', gen_random_uuid() FROM generate_series(1, 5000)
	`, fixture.workspaceID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"governance_concurrency_guard", "governance_concurrency_hold"} {
		if _, err := tx.Exec(ctx, "ANALYZE "+table); err != nil {
			t.Fatal(err)
		}
	}
	for _, query := range []struct {
		sql       string
		index     string
		arguments []any
	}{
		{"SELECT held_slots FROM governance_concurrency_guard WHERE workspace_id = $1 AND resource = $2 FOR UPDATE", "governance_concurrency_guard_identity_uidx", []any{fixture.workspaceID, "jev"}},
		{"SELECT state FROM governance_concurrency_hold WHERE workspace_id = $1 AND resource = $2 AND reservation_id = $3", "governance_concurrency_hold_identity_uidx", []any{fixture.workspaceID, "jev", reservationID}},
	} {
		rows, err := tx.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS) "+query.sql, query.arguments...)
		if err != nil {
			t.Fatal(err)
		}
		var lines []string
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			lines = append(lines, line)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		plan := strings.Join(lines, "\n")
		t.Logf("%s:\n%s", query.sql, plan)
		if !strings.Contains(plan, query.index) {
			t.Fatalf("representative query did not use workspace-leading identity index:\n%s", plan)
		}
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrencyWorkspaceTeardownCommitsAndRollsBack(t *testing.T) {
	fixture := newConcurrencyFixture(t)
	other := newConcurrencyFixture(t)
	ctx := context.Background()
	for _, workspace := range []concurrencyFixture{fixture, other} {
		tx, guards := workspace.lock(t, "jev")
		if _, err := guards.Reserve(ctx, "jev", concurrencyUUID(), 1, 1); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, commit := range []bool{false, true} {
		tx, err := fixture.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
		if err := db.New(tx).DeleteWorkspaceLeafData(ctx, fixture.workspaceID); err != nil {
			t.Fatal(err)
		}
		for _, table := range []string{"governance_concurrency_guard", "governance_concurrency_hold"} {
			var count int
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE workspace_id = $1", fixture.workspaceID).Scan(&count); err != nil || count != 0 {
				t.Fatalf("workspace teardown did not remove %s: %d/%v", table, count, err)
			}
		}
		if commit {
			err = tx.Commit(ctx)
		} else {
			err = tx.Rollback(ctx)
		}
		if err != nil {
			t.Fatal(err)
		}
		if !commit {
			fixture.counts(t, "jev", 1, 1)
		}
		other.counts(t, "jev", 1, 1)
	}
}
