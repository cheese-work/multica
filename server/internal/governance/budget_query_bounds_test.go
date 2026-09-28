package governance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type budgetExplainDocument []struct {
	Plan budgetExplainNode `json:"Plan"`
}

type budgetExplainNode struct {
	NodeType                string              `json:"Node Type"`
	ActualRows              float64             `json:"Actual Rows"`
	ActualLoops             float64             `json:"Actual Loops"`
	RowsRemovedByFilter     float64             `json:"Rows Removed by Filter"`
	RowsRemovedByIndexCheck float64             `json:"Rows Removed by Index Recheck"`
	SharedHitBlocks         int                 `json:"Shared Hit Blocks"`
	SharedReadBlocks        int                 `json:"Shared Read Blocks"`
	Plans                   []budgetExplainNode `json:"Plans"`
}

func TestBudgetQueryBounds(t *testing.T) {
	fixture := newBudgetTestFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var databaseName, postgresVersion, sharedBuffers, effectiveCacheSize string
	if err := fixture.pool.QueryRow(ctx, `SELECT current_database(), version()`).Scan(&databaseName, &postgresVersion); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pool.QueryRow(ctx, `SHOW shared_buffers`).Scan(&sharedBuffers); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pool.QueryRow(ctx, `SHOW effective_cache_size`).Scan(&effectiveCacheSize); err != nil {
		t.Fatal(err)
	}

	distractorWorkspaceID := budgetTestUUID()
	distractorRootID := budgetTestUUID()
	seedBudgetHistory(t, ctx, fixture.pool, distractorWorkspaceID, distractorRootID, 100000)
	t.Cleanup(func() { cleanupBudgetWorkspace(t, fixture.pool, distractorWorkspaceID) })

	queryDigest := sha256.Sum256([]byte(budgetWindowExposureQuery))
	t.Logf("database=%s postgres=%q os=%s arch=%s cpus=%d shared_buffers=%s effective_cache_size=%s distractor_terminal_rows=100000 query_sha256=%s cache_state=warm-after-analyze-and-one-query", databaseName, postgresVersion, runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), sharedBuffers, effectiveCacheSize, hex.EncodeToString(queryDigest[:]))

	for index, historyRows := range []int{1000, 10000, 100000} {
		workspaceID := fixture.workspaceID
		if index > 0 {
			workspaceID = budgetTestUUID()
			cleanupWorkspaceID := workspaceID
			t.Cleanup(func() { cleanupBudgetWorkspace(t, fixture.pool, cleanupWorkspaceID) })
			if _, err := fixture.pool.Exec(ctx, `
				INSERT INTO governance_workspace_config (workspace_id, control_epoch, settings)
				VALUES ($1, 1, '{"jev_governance_enabled":true,"rule_mode":"shadow"}')
			`, workspaceID); err != nil {
				t.Fatal(err)
			}
		}
		rootID := budgetTestUUID()
		seedBudgetHistory(t, ctx, fixture.pool, workspaceID, rootID, historyRows)
		if _, err := fixture.pool.Exec(ctx, `ANALYZE governance_budget_window`); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.pool.Exec(ctx, `ANALYZE governance_budget_reservation`); err != nil {
			t.Fatal(err)
		}

		service, err := NewBudgetService(fixture.pool, fixture.clock)
		if err != nil {
			t.Fatal(err)
		}
		unresolvedStart, unresolvedEnd := budgetTestTime(11, 30), budgetTestTime(12, 30)
		unresolvedCommand := fixture.reserveCommand("resource-unresolved", unresolvedStart, unresolvedEnd, 200000, 200000, 100, 1)
		unresolvedCommand.WorkspaceID = workspaceID
		unresolvedCommand.BudgetRootID = rootID
		unresolved, err := service.Reserve(ctx, unresolvedCommand)
		if err != nil {
			t.Fatalf("seed unresolved reservation at history=%d: %v", historyRows, err)
		}
		if _, err := service.Settle(ctx, BudgetSettleCommand{
			WorkspaceID:      workspaceID,
			ReservationID:    unresolved.Reservation.ReservationID,
			ExpectedRevision: unresolved.Reservation.Revision,
			EventKey:         "unknown-termination",
		}); err != nil {
			t.Fatalf("retain unresolved liability at history=%d: %v", historyRows, err)
		}

		incomingStart, incomingEnd := budgetTestTime(12, 0), budgetTestTime(13, 0)
		if _, err := fixture.pool.Exec(ctx, `
			INSERT INTO governance_budget_window (workspace_id, window_start, window_end, spend_cap_micro_usd)
			VALUES ($1, $2, $3, 200000)
			ON CONFLICT (workspace_id, window_start) DO NOTHING
		`, workspaceID, incomingStart, incomingEnd); err != nil {
			t.Fatal(err)
		}
		starts := []time.Time{unresolvedStart, incomingStart}
		if _, err := fixture.pool.Exec(ctx, budgetWindowExposureQuery, workspaceID, starts); err != nil {
			t.Fatal(err)
		}
		protectedPlan := explainBudgetQuery(t, ctx, fixture.pool, protectedBudgetWindowsQuery, workspaceID, incomingStart, fixture.clock.Now())
		protectedMetrics := budgetPlanMetrics(t, protectedPlan)
		if protectedMetrics.returnedRows != 2 {
			t.Fatalf("protected-window query returned %d rows at history=%d, want 2", protectedMetrics.returnedRows, historyRows)
		}
		if historyRows == 100000 && protectedMetrics.visitedRows > 8 {
			t.Fatalf("protected-window query visited %d scan rows at history=%d, want at most 8", protectedMetrics.visitedRows, historyRows)
		}
		exposurePlan := explainBudgetQuery(t, ctx, fixture.pool, budgetWindowExposureQuery, workspaceID, starts)
		exposureMetrics := budgetPlanMetrics(t, exposurePlan)
		if exposureMetrics.returnedRows != 2 {
			t.Fatalf("exposure query returned %d rows at history=%d, want 2", exposureMetrics.returnedRows, historyRows)
		}
		if historyRows == 100000 && exposureMetrics.visitedRows > 8 {
			t.Fatalf("exposure query visited %d scan rows at history=%d, want at most 8", exposureMetrics.visitedRows, historyRows)
		}

		tx, err := fixture.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		state, err := loadBudgetAdmissionState(ctx, tx, workspaceID, rootID, incomingStart, fixture.clock.Now())
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		if len(state.windows) != 2 || len(state.reservations) != 0 {
			_ = tx.Rollback(ctx)
			t.Fatalf("admission returned %d windows and %d history reservations at history=%d, want 2 windows and 0 reservations", len(state.windows), len(state.reservations), historyRows)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}

		for _, boundaryStart := range starts {
			want := exhaustiveBudgetWindowExposure(t, ctx, fixture.pool, workspaceID, boundaryStart)
			if got := state.windowExposure[budgetWindowKey(boundaryStart)]; got != want {
				t.Fatalf("window exposure at history=%d start=%s = %d, exhaustive oracle wants %d", historyRows, boundaryStart, got, want)
			}
		}

		command := fixture.reserveCommand("resource-current", incomingStart, incomingEnd, 200000, 200000, 10, 1)
		command.WorkspaceID = workspaceID
		command.BudgetRootID = rootID
		if _, err := service.Reserve(ctx, command); err != nil {
			t.Fatalf("reserve with terminal history=%d: %v", historyRows, err)
		}
		t.Logf("history=%d distractor=100000 protected_returned=%d protected_visited=%d protected_hits=%d protected_reads=%d protected_plan=%s exposure_returned=%d exposure_visited=%d exposure_hits=%d exposure_reads=%d exposure_plan=%s", historyRows, protectedMetrics.returnedRows, protectedMetrics.visitedRows, protectedMetrics.sharedHits, protectedMetrics.sharedReads, string(protectedPlan), exposureMetrics.returnedRows, exposureMetrics.visitedRows, exposureMetrics.sharedHits, exposureMetrics.sharedReads, string(exposurePlan))
	}
}

func seedBudgetHistory(t *testing.T, ctx context.Context, pool *pgxpool.Pool, workspaceID, rootID pgtype.UUID, rows int) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO governance_budget_root (workspace_id, budget_root_id, spend_cap_micro_usd, spent_micro_usd)
		VALUES ($1, $2, 200000, $3)
	`, workspaceID, rootID, rows); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO governance_budget_window (
			workspace_id, window_start, window_end, spend_cap_micro_usd, spent_micro_usd
		)
		SELECT $1, start_at, start_at + interval '1 second', 200000, 1
		FROM (
			SELECT timestamptz '2000-01-01 00:00:00+00' + value * interval '2 seconds' AS start_at
			FROM generate_series(1, $2::integer) AS value
		) AS history
	`, workspaceID, rows); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO governance_budget_reservation (
			workspace_id, reservation_id, budget_root_id, case_id, attempt_id, obligation_id,
			resource, control_epoch, window_start, window_end, root_cap_micro_usd,
			window_cap_micro_usd, max_attempt_cost_micro_usd, retry_allowance,
			retry_policy_bounded, retry_allowance_remaining, attempts_started,
			total_cap_micro_usd, remaining_micro_usd, debited_micro_usd, settled_micro_usd,
			state, revision, request_digest, settlement_receipt_id, usage_known,
			termination_known, settled_at
		)
		SELECT $1, gen_random_uuid(), $2, gen_random_uuid(), gen_random_uuid(), gen_random_uuid(),
			'history', 1, start_at, start_at + interval '1 second', 200000,
			200000, 1, 0, TRUE, 0, 1, 1, 0, 1, 1, 'settled', 1,
			repeat('a', 64), 'history-' || value, TRUE, TRUE, now()
		FROM (
			SELECT value,
				timestamptz '2000-01-01 00:00:00+00' + value * interval '2 seconds' AS start_at
			FROM generate_series(1, $3::integer) AS value
		) AS history
	`, workspaceID, rootID, rows); err != nil {
		t.Fatal(err)
	}
}

func cleanupBudgetWorkspace(t *testing.T, pool *pgxpool.Pool, workspaceID pgtype.UUID) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, table := range []string{
		"governance_budget_outbox",
		"governance_budget_journal",
		"governance_budget_reservation",
		"governance_budget_root",
		"governance_budget_window",
		"governance_concurrency_hold",
		"governance_concurrency_guard",
		"governance_workspace_config",
	} {
		if _, err := pool.Exec(ctx, "DELETE FROM "+table+" WHERE workspace_id = $1", workspaceID); err != nil {
			t.Errorf("cleanup %s for %s: %v", table, workspaceID.String(), err)
		}
	}
}

func exhaustiveBudgetWindowExposure(t *testing.T, ctx context.Context, pool *pgxpool.Pool, workspaceID pgtype.UUID, boundaryStart time.Time) int64 {
	t.Helper()
	var exposure int64
	err := pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(CASE
			WHEN reservation.state = 'reserved' OR NOT reservation.usage_known
				THEN reservation.total_cap_micro_usd
			ELSE reservation.settled_micro_usd
		END), 0)
		FROM governance_budget_window AS boundary
		JOIN governance_budget_reservation AS reservation
		  ON reservation.workspace_id = boundary.workspace_id
		 AND reservation.window_start < boundary.window_end
		 AND reservation.window_end > boundary.window_start
		WHERE boundary.workspace_id = $1 AND boundary.window_start = $2
	`, workspaceID, boundaryStart).Scan(&exposure)
	if err != nil {
		t.Fatal(err)
	}
	return exposure
}

func explainBudgetQuery(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query string, args ...any) []byte {
	t.Helper()
	var plan []byte
	if err := pool.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+query, args...).Scan(&plan); err != nil {
		t.Fatal(err)
	}
	return plan
}

type budgetPlanMeasurements struct {
	returnedRows int
	visitedRows  int
	sharedHits   int
	sharedReads  int
}

func budgetPlanMetrics(t *testing.T, plan []byte) budgetPlanMeasurements {
	t.Helper()
	var document budgetExplainDocument
	if err := json.Unmarshal(plan, &document); err != nil {
		t.Fatal(err)
	}
	if len(document) != 1 {
		t.Fatalf("EXPLAIN returned %d plan documents, want 1", len(document))
	}
	root := document[0].Plan
	measurements := budgetPlanMeasurements{
		returnedRows: int(root.ActualRows * root.ActualLoops),
		sharedHits:   root.SharedHitBlocks,
		sharedReads:  root.SharedReadBlocks,
	}
	measureBudgetScanRows(root, &measurements)
	return measurements
}

func measureBudgetScanRows(node budgetExplainNode, measurements *budgetPlanMeasurements) {
	if strings.Contains(node.NodeType, "Scan") {
		loops := node.ActualLoops
		if loops < 1 {
			loops = 1
		}
		measurements.visitedRows += int((node.ActualRows + node.RowsRemovedByFilter + node.RowsRemovedByIndexCheck) * loops)
	}
	for _, child := range node.Plans {
		measureBudgetScanRows(child, measurements)
	}
}
