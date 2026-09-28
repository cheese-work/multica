package governance

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestBudgetWorkspaceLeadingIndexesAndNoForeignKeys(t *testing.T) {
	fixture := newBudgetFixture(t)
	ctx := context.Background()
	for _, expected := range []struct {
		index   string
		columns string
	}{
		{"governance_budget_window_pkey", "workspace_id, window_start"},
		{"governance_budget_root_pkey", "workspace_id, budget_root_id"},
		{"governance_budget_reservation_pkey", "workspace_id, reservation_id"},
		{"governance_budget_reservation_root_window_idx", "workspace_id, budget_root_id, window_start, reservation_id"},
		{"governance_budget_journal_pkey", "workspace_id, reservation_id, event_key"},
		{"governance_budget_outbox_pkey", "workspace_id, event_id"},
		{"governance_budget_outbox_due_idx", "workspace_id, state, created_at, event_id"},
	} {
		var valid bool
		var definition string
		if err := fixture.pool.QueryRow(ctx, `
			SELECT indisvalid, pg_get_indexdef(indexrelid)
			FROM pg_index WHERE indexrelid = $1::regclass
		`, expected.index).Scan(&valid, &definition); err != nil {
			t.Fatal(err)
		}
		if !valid || !strings.HasSuffix(definition, "("+expected.columns+")") {
			t.Fatalf("invalid workspace-leading index: %s valid=%v", definition, valid)
		}
	}
	var foreignKeys int
	if err := fixture.pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_constraint
		WHERE contype = 'f' AND conrelid IN (
		    'governance_budget_window'::regclass,
		    'governance_budget_root'::regclass,
		    'governance_budget_reservation'::regclass,
		    'governance_budget_journal'::regclass,
		    'governance_budget_outbox'::regclass
		)
	`).Scan(&foreignKeys); err != nil || foreignKeys != 0 {
		t.Fatalf("budget tables contain foreign keys: %d/%v", foreignKeys, err)
	}
}

func TestBudgetRepresentativeExplainUsesWorkspaceIndexes(t *testing.T) {
	fixture := newBudgetFixture(t)
	ctx := context.Background()
	rootID := concurrencyUUID()
	reservationID := concurrencyUUID()
	windowStart := time.Now().UTC().Truncate(time.Hour)
	windowEnd := windowStart.Add(time.Hour)
	for _, insert := range []struct {
		sql       string
		arguments []any
	}{
		{`INSERT INTO governance_budget_root (workspace_id, budget_root_id, spend_cap_micro_usd) VALUES ($1, $2, 1000000)`, []any{fixture.workspaceID, rootID}},
		{`INSERT INTO governance_budget_root (workspace_id, budget_root_id, spend_cap_micro_usd) SELECT $1, gen_random_uuid(), 1000000 FROM generate_series(1, 5000)`, []any{fixture.workspaceID}},
		{`INSERT INTO governance_budget_window (workspace_id, window_start, window_end, spend_cap_micro_usd) VALUES ($1, $2, $3, 1000000)`, []any{fixture.workspaceID, windowStart, windowEnd}},
		{`INSERT INTO governance_budget_window (workspace_id, window_start, window_end, spend_cap_micro_usd) SELECT $1, $2::timestamptz + ordinal * interval '1 second', $3::timestamptz + ordinal * interval '1 second', 1000000 FROM generate_series(1, 5000) AS ordinal`, []any{fixture.workspaceID, windowStart, windowEnd}},
	} {
		if _, err := fixture.pool.Exec(ctx, insert.sql, insert.arguments...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fixture.pool.Exec(ctx, `
		INSERT INTO governance_budget_reservation (
		    workspace_id, reservation_id, budget_root_id, case_id, attempt_id, obligation_id,
		    resource, control_epoch, window_start, window_end, root_cap_micro_usd,
		    window_cap_micro_usd, max_attempt_cost_micro_usd, retry_allowance,
		    retry_policy_bounded, retry_allowance_remaining, total_cap_micro_usd,
		    remaining_micro_usd, request_digest
		) VALUES ($1,$2,$3,$4,$5,$6,'jev',1,$7,$8,1000000,1000000,100,0,true,0,100,100,repeat('a',64));
	`, fixture.workspaceID, reservationID, rootID, concurrencyUUID(), concurrencyUUID(), concurrencyUUID(), windowStart, windowEnd); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(ctx, `
		INSERT INTO governance_budget_reservation (
		    workspace_id, reservation_id, budget_root_id, case_id, attempt_id, obligation_id,
		    resource, control_epoch, window_start, window_end, root_cap_micro_usd,
		    window_cap_micro_usd, max_attempt_cost_micro_usd, retry_allowance,
		    retry_policy_bounded, retry_allowance_remaining, total_cap_micro_usd,
		    remaining_micro_usd, request_digest
		)
		SELECT $1, gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), gen_random_uuid(),
		    'jev',1,$2,$3,1000000,1000000,100,0,true,0,100,100,repeat('b',64)
		FROM generate_series(1,5000);
	`, fixture.workspaceID, windowStart, windowEnd); err != nil {
		t.Fatal(err)
	}
	for _, insert := range []struct {
		sql       string
		arguments []any
	}{
		{`INSERT INTO governance_budget_journal (workspace_id, reservation_id, event_key, event_type, event_digest, expected_revision, resulting_revision) VALUES ($1,$2,'explain-sample','reserve',repeat('c',64),0,1)`, []any{fixture.workspaceID, reservationID}},
		{`INSERT INTO governance_budget_journal (workspace_id, reservation_id, event_key, event_type, event_digest, expected_revision, resulting_revision) SELECT $1, gen_random_uuid(), 'event-' || ordinal, 'reserve', repeat('d',64), 0, 1 FROM generate_series(1,5000) AS ordinal`, []any{fixture.workspaceID}},
		{`INSERT INTO governance_budget_outbox (workspace_id, reservation_id, event_key, event_type, case_id, attempt_id, obligation_id, payload) VALUES ($1,$2,'explain-sample','admit',$3,$4,$5,'{}'::jsonb)`, []any{fixture.workspaceID, reservationID, concurrencyUUID(), concurrencyUUID(), concurrencyUUID()}},
		{`INSERT INTO governance_budget_outbox (workspace_id, reservation_id, event_key, event_type, case_id, attempt_id, obligation_id, payload) SELECT $1, gen_random_uuid(), 'event-' || ordinal, 'admit', gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), '{}'::jsonb FROM generate_series(1,5000) AS ordinal`, []any{fixture.workspaceID}},
	} {
		if _, err := fixture.pool.Exec(ctx, insert.sql, insert.arguments...); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range []string{
		"governance_budget_root",
		"governance_budget_window",
		"governance_budget_reservation",
		"governance_budget_journal",
		"governance_budget_outbox",
	} {
		if _, err := fixture.pool.Exec(ctx, "ANALYZE "+table); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	queries := []struct {
		sql       string
		index     string
		arguments []any
	}{
		{"SELECT spend_cap_micro_usd FROM governance_budget_root WHERE workspace_id = $1 AND budget_root_id = $2 FOR UPDATE", "governance_budget_root_pkey", []any{fixture.workspaceID, rootID}},
		{"SELECT spend_cap_micro_usd FROM governance_budget_window WHERE workspace_id = $1 AND window_start = $2 FOR UPDATE", "governance_budget_window_pkey", []any{fixture.workspaceID, windowStart}},
		{"SELECT revision FROM governance_budget_reservation WHERE workspace_id = $1 AND reservation_id = $2", "governance_budget_reservation_pkey", []any{fixture.workspaceID, reservationID}},
		{"SELECT event_digest FROM governance_budget_journal WHERE workspace_id = $1 AND reservation_id = $2 AND event_key = 'explain-sample'", "governance_budget_journal_pkey", []any{fixture.workspaceID, reservationID}},
		{"SELECT event_id FROM governance_budget_outbox WHERE workspace_id = $1 AND state = 'pending' ORDER BY created_at, event_id LIMIT 50", "governance_budget_outbox_due_idx", []any{fixture.workspaceID}},
	}
	for _, query := range queries {
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
			t.Fatalf("representative query did not use %s:\n%s", query.index, plan)
		}
	}
}

func TestBudgetWorkspaceTeardownIsTransactionalAndWorkspaceScoped(t *testing.T) {
	fixture := newBudgetFixture(t)
	other := newBudgetFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	request := fixture.request(now)
	otherRequest := other.request(now)
	_, _ = fixture.reserve(t, request, now)
	_, _ = other.reserve(t, otherRequest, now)
	tables := []string{
		"governance_budget_outbox",
		"governance_budget_journal",
		"governance_budget_reservation",
		"governance_budget_root",
		"governance_budget_window",
		"governance_concurrency_hold",
		"governance_concurrency_guard",
	}
	deleteWorkspace := func(tx pgx.Tx) {
		t.Helper()
		if err := db.New(tx).DeleteWorkspaceLeafData(context.Background(), fixture.workspaceID); err != nil {
			t.Fatal(err)
		}
		for _, table := range tables {
			var count int
			if err := tx.QueryRow(context.Background(), "SELECT count(*) FROM "+table+" WHERE workspace_id = $1", fixture.workspaceID).Scan(&count); err != nil || count != 0 {
				t.Fatalf("teardown left %s rows: %d/%v", table, count, err)
			}
		}
	}
	rollbackTx, err := fixture.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	deleteWorkspace(rollbackTx)
	if err := rollbackTx.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	fixture.counts(t, request.Resource, 1, 1)
	commitTx, err := fixture.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	deleteWorkspace(commitTx)
	if err := commitTx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	other.counts(t, otherRequest.Resource, 1, 1)
	var retained int
	if err := other.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM governance_budget_reservation
		WHERE workspace_id = $1 AND reservation_id = $2
	`, other.workspaceID, otherRequest.ReservationID).Scan(&retained); err != nil || retained != 1 {
		t.Fatalf("neighbor workspace reservation changed: %d/%v", retained, err)
	}
}
