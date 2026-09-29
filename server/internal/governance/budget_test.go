package governance

import (
	"context"
	"errors"
	"math"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type budgetTestFixture struct {
	pool        *pgxpool.Pool
	workspaceID pgtype.UUID
	clock       *budgetTestClock
	service     *BudgetService
}

type budgetTestClock struct {
	mu      sync.RWMutex
	current time.Time
}

func (clock *budgetTestClock) Now() time.Time {
	clock.mu.RLock()
	defer clock.mu.RUnlock()
	return clock.current
}

func (clock *budgetTestClock) Set(current time.Time) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.current = current
}

func newBudgetTestFixture(t *testing.T) *budgetTestFixture {
	t.Helper()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is not set")
	}
	pool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Fatal(err)
	}
	workspaceID := budgetTestUUID()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO governance_workspace_config (workspace_id, control_epoch, settings)
		VALUES ($1, 1, '{"jev_governance_enabled":true,"rule_mode":"shadow"}')
	`, workspaceID); err != nil {
		pool.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
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
			if _, err := pool.Exec(context.Background(), "DELETE FROM "+table+" WHERE workspace_id = $1", workspaceID); err != nil {
				t.Errorf("cleanup %s: %v", table, err)
			}
		}
		pool.Close()
	})
	clock := &budgetTestClock{current: budgetTestTime(12, 5)}
	service, err := NewBudgetService(pool, clock)
	if err != nil {
		t.Fatal(err)
	}
	return &budgetTestFixture{pool: pool, workspaceID: workspaceID, clock: clock, service: service}
}

func (fixture *budgetTestFixture) reserveCommand(resource string, windowStart, windowEnd time.Time, windowCap, rootCap, cost int64, epoch int64) BudgetReserveCommand {
	reservationID := budgetTestUUID()
	return BudgetReserveCommand{
		WorkspaceID:            fixture.workspaceID,
		ReservationID:          reservationID,
		BudgetRootID:           budgetTestUUID(),
		CaseID:                 budgetTestUUID(),
		AttemptID:              budgetTestUUID(),
		ObligationID:           budgetTestUUID(),
		Resource:               resource,
		ControlEpoch:           epoch,
		WindowStart:            windowStart,
		WindowEnd:              windowEnd,
		RootCapMicroUSD:        rootCap,
		WindowCapMicroUSD:      windowCap,
		MaxAttemptCostMicroUSD: cost,
		RetryPolicyBounded:     true,
		SlotLimit:              4,
		EventKey:               "reserve-" + reservationID.String(),
	}
}

func (fixture *budgetTestFixture) setEpoch(t *testing.T, epoch int64) {
	t.Helper()
	if _, err := fixture.pool.Exec(context.Background(), `
		UPDATE governance_workspace_config SET control_epoch = $2 WHERE workspace_id = $1
	`, fixture.workspaceID, epoch); err != nil {
		t.Fatal(err)
	}
}

func TestReviewBudgetResizeThenRolloverRetainsActiveWindowCap(t *testing.T) {
	fixture := newBudgetTestFixture(t)
	ctx := context.Background()
	first := fixture.reserveCommand("resource-a", budgetTestTime(12, 0), budgetTestTime(13, 0), 100, 100, 50, 1)
	if _, err := fixture.service.Reserve(ctx, first); err != nil {
		t.Fatalf("first reservation: %v", err)
	}
	duplicate, err := fixture.service.Reserve(ctx, first)
	if err != nil || !duplicate.Duplicate {
		t.Fatalf("duplicate reservation = %+v, %v; want duplicate", duplicate, err)
	}
	if held := budgetCount(t, fixture.pool, `SELECT count(*) FROM governance_concurrency_hold WHERE workspace_id = $1 AND state = 'held'`, fixture.workspaceID); held != 1 {
		t.Fatalf("held slots after duplicate reservation = %d, want 1", held)
	}

	fixture.setEpoch(t, 2)
	fixture.clock.Set(budgetTestTime(12, 20))
	second := fixture.reserveCommand("resource-b", budgetTestTime(12, 15), budgetTestTime(12, 30), 100, 100, 50, 2)
	if _, err := fixture.service.Reserve(ctx, second); err != nil {
		t.Fatalf("second reservation: %v", err)
	}

	fixture.setEpoch(t, 3)
	fixture.clock.Set(budgetTestTime(12, 35))
	third := fixture.reserveCommand("resource-c", budgetTestTime(12, 30), budgetTestTime(12, 45), 100, 100, 50, 3)
	if _, err := fixture.service.Reserve(ctx, third); !errors.Is(err, ErrBudgetLimit) {
		t.Fatalf("third reservation error = %v, want ErrBudgetLimit", err)
	}
	if count := budgetCount(t, fixture.pool, `SELECT count(*) FROM governance_budget_reservation WHERE workspace_id = $1`, fixture.workspaceID); count != 2 {
		t.Fatalf("reservations = %d, want 2", count)
	}
	if exposure := budgetCount(t, fixture.pool, `SELECT COALESCE(sum(reserved_micro_usd + spent_micro_usd), 0) FROM governance_budget_window WHERE workspace_id = $1`, fixture.workspaceID); exposure != 100 {
		t.Fatalf("window exposure = %d, want 100", exposure)
	}
	if holds := budgetCount(t, fixture.pool, `SELECT count(*) FROM governance_concurrency_hold WHERE workspace_id = $1 AND state = 'held'`, fixture.workspaceID); holds != 2 {
		t.Fatalf("held slots = %d, want 2", holds)
	}
	assertBudgetNoArtifacts(t, fixture, third.ReservationID)
	if count := budgetCount(t, fixture.pool, `SELECT count(*) FROM governance_budget_root WHERE workspace_id = $1 AND budget_root_id = $2`, fixture.workspaceID, third.BudgetRootID); count != 0 {
		t.Fatalf("rejected reservation left %d budget roots, want 0", count)
	}
}

func TestBudgetReserveDoesNotWaitOnTerminalHistory(t *testing.T) {
	fixture := newBudgetTestFixture(t)
	ctx := context.Background()
	oldWindowStart := budgetTestTime(10, 0)
	oldWindowEnd := budgetTestTime(11, 0)
	fixture.clock.Set(budgetTestTime(10, 5))

	oldCommand := fixture.reserveCommand("resource-old", oldWindowStart, oldWindowEnd, 100, 100, 20, 1)
	oldReservation, err := fixture.service.Reserve(ctx, oldCommand)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.Settle(ctx, BudgetSettleCommand{
		WorkspaceID:      fixture.workspaceID,
		ReservationID:    oldReservation.Reservation.ReservationID,
		ExpectedRevision: oldReservation.Reservation.Revision,
		EventKey:         "settle-old-terminal",
		ReceiptID:        "old-terminal",
		UsageKnown:       true,
		TerminationKnown: true,
		UsageMicroUSD:    1,
	}); err != nil {
		t.Fatal(err)
	}
	fixture.clock.Set(budgetTestTime(12, 5))

	lockTx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lockTx.Rollback(ctx)
	var lockedID pgtype.UUID
	if err := lockTx.QueryRow(ctx, `
		SELECT reservation_id FROM governance_budget_reservation
		WHERE workspace_id = $1 AND reservation_id = $2
		FOR UPDATE
	`, fixture.workspaceID, oldReservation.Reservation.ReservationID).Scan(&lockedID); err != nil {
		t.Fatal(err)
	}
	if _, err := lockTx.Exec(ctx, `
		SELECT window_start FROM governance_budget_window
		WHERE workspace_id = $1 AND window_start = $2
		FOR UPDATE
	`, fixture.workspaceID, oldWindowStart); err != nil {
		t.Fatal(err)
	}

	command := fixture.reserveCommand("resource-current", budgetTestTime(12, 0), budgetTestTime(13, 0), 100, 100, 10, 1)
	command.BudgetRootID = oldCommand.BudgetRootID
	reserveCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if _, err := fixture.service.Reserve(reserveCtx, command); err != nil {
		t.Fatalf("reserve waited on terminal history: %v", err)
	}
}

func TestBudgetReserveRejectsAfterWorkspaceLockWaitPastWindowEnd(t *testing.T) {
	fixture := newBudgetTestFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	windowStart := budgetTestTime(12, 0)
	windowEnd := budgetTestTime(13, 0)
	fixture.clock.Set(budgetTestTime(12, 59))
	command := fixture.reserveCommand("resource-lock-wait", windowStart, windowEnd, 100, 100, 25, 1)
	lockTx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lockTx.Rollback(context.Background())
	var controlEpoch int64
	if err := lockTx.QueryRow(ctx, `
		SELECT control_epoch FROM governance_workspace_config
		WHERE workspace_id = $1 FOR UPDATE
	`, fixture.workspaceID).Scan(&controlEpoch); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := fixture.service.Reserve(ctx, command)
		result <- err
	}()
	lockerPID := int32(lockTx.Conn().PgConn().PID())
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err := fixture.pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE wait_event_type = 'Lock'
					AND $1::integer = ANY(pg_blocking_pids(pid))
					AND query LIKE '%FROM governance_workspace_config%'
			)
		`, lockerPID).Scan(&blocked); err != nil {
			t.Fatalf("check reservation lock wait: %v", err)
		}
		if blocked {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("reservation did not wait on workspace lock: %v", ctx.Err())
		case <-ticker.C:
		}
	}
	fixture.clock.Set(windowEnd)
	if err := lockTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, ErrBudgetWindow) {
		t.Fatalf("reservation after lock wait error = %v, want ErrBudgetWindow", err)
	}
	assertBudgetNoArtifacts(t, fixture, command.ReservationID)
}

func TestBudgetLiabilityShrinkExpandKeepsEveryOpenBoundary(t *testing.T) {
	fixture := newBudgetTestFixture(t)
	ctx := context.Background()
	first := fixture.reserveCommand("resource-a", budgetTestTime(12, 0), budgetTestTime(13, 0), 100, 100, 50, 1)
	if _, err := fixture.service.Reserve(ctx, first); err != nil {
		t.Fatal(err)
	}
	fixture.setEpoch(t, 2)
	fixture.clock.Set(budgetTestTime(12, 20))
	second := fixture.reserveCommand("resource-b", budgetTestTime(12, 15), budgetTestTime(12, 30), 100, 100, 25, 2)
	if _, err := fixture.service.Reserve(ctx, second); err != nil {
		t.Fatalf("shrunk window reservation: %v", err)
	}
	fixture.setEpoch(t, 3)
	fixture.clock.Set(budgetTestTime(12, 35))
	third := fixture.reserveCommand("resource-c", budgetTestTime(12, 30), budgetTestTime(13, 0), 200, 100, 25, 3)
	if _, err := fixture.service.Reserve(ctx, third); err != nil {
		t.Fatalf("expanded window reservation: %v", err)
	}
	fixture.setEpoch(t, 4)
	fourth := fixture.reserveCommand("resource-d", budgetTestTime(12, 30), budgetTestTime(13, 0), 200, 100, 1, 4)
	if _, err := fixture.service.Reserve(ctx, fourth); !errors.Is(err, ErrBudgetLimit) {
		t.Fatalf("reservation above original cap error = %v, want ErrBudgetLimit", err)
	}
	assertBudgetNoArtifacts(t, fixture, fourth.ReservationID)
}

func TestBudgetLiabilityReductionAndLaterIncreaseStayWithinOriginalCap(t *testing.T) {
	fixture := newBudgetTestFixture(t)
	ctx := context.Background()
	windowStart := budgetTestTime(12, 0)
	windowEnd := budgetTestTime(13, 0)
	first := fixture.reserveCommand("resource-a", windowStart, windowEnd, 100, 100, 40, 1)
	if _, err := fixture.service.Reserve(ctx, first); err != nil {
		t.Fatal(err)
	}
	fixture.setEpoch(t, 2)
	second := fixture.reserveCommand("resource-b", windowStart, windowEnd, 60, 100, 20, 2)
	second.BudgetRootID = first.BudgetRootID
	if _, err := fixture.service.Reserve(ctx, second); err != nil {
		t.Fatalf("reduced-cap reservation: %v", err)
	}
	fixture.setEpoch(t, 3)
	third := fixture.reserveCommand("resource-c", windowStart, windowEnd, 200, 100, 40, 3)
	third.BudgetRootID = first.BudgetRootID
	if _, err := fixture.service.Reserve(ctx, third); err != nil {
		t.Fatalf("increase within original cap: %v", err)
	}
	fixture.setEpoch(t, 4)
	fourth := fixture.reserveCommand("resource-d", windowStart, windowEnd, 200, 100, 1, 4)
	fourth.BudgetRootID = first.BudgetRootID
	if _, err := fixture.service.Reserve(ctx, fourth); !errors.Is(err, ErrBudgetLimit) {
		t.Fatalf("reservation beyond original cap error = %v, want ErrBudgetLimit", err)
	}
	assertBudgetNoArtifacts(t, fixture, fourth.ReservationID)
}

func TestBudgetLiabilityRejectsReductionBelowExistingExposure(t *testing.T) {
	fixture := newBudgetTestFixture(t)
	ctx := context.Background()
	windowStart := budgetTestTime(12, 0)
	windowEnd := budgetTestTime(13, 0)
	first := fixture.reserveCommand("resource-a", windowStart, windowEnd, 100, 100, 80, 1)
	if _, err := fixture.service.Reserve(ctx, first); err != nil {
		t.Fatal(err)
	}
	fixture.setEpoch(t, 2)
	fixture.clock.Set(budgetTestTime(12, 20))
	reduced := fixture.reserveCommand("resource-b", windowStart, windowEnd, 60, 100, 1, 2)
	reduced.BudgetRootID = first.BudgetRootID
	if _, err := fixture.service.Reserve(ctx, reduced); !errors.Is(err, ErrBudgetLimit) {
		t.Fatalf("reservation above reduced cap error = %v, want ErrBudgetLimit", err)
	}
	assertBudgetNoArtifacts(t, fixture, reduced.ReservationID)
	fixture.setEpoch(t, 3)
	increased := fixture.reserveCommand("resource-c", windowStart, windowEnd, 200, 100, 20, 3)
	increased.BudgetRootID = first.BudgetRootID
	if _, err := fixture.service.Reserve(ctx, increased); err != nil {
		t.Fatalf("reservation within original cap after increase: %v", err)
	}
	fixture.setEpoch(t, 4)
	over := fixture.reserveCommand("resource-d", windowStart, windowEnd, 200, 100, 1, 4)
	over.BudgetRootID = first.BudgetRootID
	if _, err := fixture.service.Reserve(ctx, over); !errors.Is(err, ErrBudgetLimit) {
		t.Fatalf("reservation beyond original cap error = %v, want ErrBudgetLimit", err)
	}
	assertBudgetNoArtifacts(t, fixture, over.ReservationID)
}

func TestBudgetLiabilitySameStartWindowIdentityConflict(t *testing.T) {
	fixture := newBudgetTestFixture(t)
	first := fixture.reserveCommand("resource-a", budgetTestTime(12, 0), budgetTestTime(13, 0), 100, 100, 20, 1)
	if _, err := fixture.service.Reserve(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	fixture.setEpoch(t, 2)
	fixture.clock.Set(budgetTestTime(12, 20))
	conflicting := fixture.reserveCommand("resource-b", budgetTestTime(12, 0), budgetTestTime(12, 30), 100, 100, 20, 2)
	if _, err := fixture.service.Reserve(context.Background(), conflicting); !errors.Is(err, ErrBudgetWindowConflict) {
		t.Fatalf("same-start interval error = %v, want ErrBudgetWindowConflict", err)
	}
	assertBudgetNoArtifacts(t, fixture, conflicting.ReservationID)
}

func TestBudgetLiabilityUnknownPriorWindowCarriesUntilTerminal(t *testing.T) {
	fixture := newBudgetTestFixture(t)
	ctx := context.Background()
	first := fixture.reserveCommand("resource-a", budgetTestTime(12, 0), budgetTestTime(13, 0), 100, 100, 100, 1)
	reserved, err := fixture.service.Reserve(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	fixture.clock.Set(budgetTestTime(13, 5))
	fixture.setEpoch(t, 2)
	second := fixture.reserveCommand("resource-b", budgetTestTime(13, 0), budgetTestTime(14, 0), 100, 100, 100, 2)
	if _, err := fixture.service.Reserve(ctx, second); !errors.Is(err, ErrBudgetLimit) {
		t.Fatalf("rollover reservation error = %v, want ErrBudgetLimit", err)
	}

	unknownTermination, err := fixture.service.Settle(ctx, BudgetSettleCommand{
		WorkspaceID:      fixture.workspaceID,
		ReservationID:    reserved.Reservation.ReservationID,
		ExpectedRevision: reserved.Reservation.Revision,
		EventKey:         "unknown-termination",
	})
	if err != nil || unknownTermination.Finalized {
		t.Fatalf("unknown termination settlement = %+v, %v; want unresolved", unknownTermination, err)
	}
	if held := budgetCount(t, fixture.pool, `SELECT count(*) FROM governance_concurrency_hold WHERE workspace_id = $1 AND reservation_id = $2 AND state = 'held'`, fixture.workspaceID, first.ReservationID); held != 1 {
		t.Fatalf("unknown termination held slots = %d, want 1", held)
	}
	fixture.clock.Set(budgetTestTime(14, 5))
	fixture.setEpoch(t, 3)
	third := fixture.reserveCommand("resource-c", budgetTestTime(14, 0), budgetTestTime(15, 0), 100, 100, 100, 3)
	if _, err := fixture.service.Reserve(ctx, third); !errors.Is(err, ErrBudgetLimit) {
		t.Fatalf("second rollover with unknown exposure error = %v, want ErrBudgetLimit", err)
	}

	finalCommand := BudgetSettleCommand{
		WorkspaceID:      fixture.workspaceID,
		ReservationID:    reserved.Reservation.ReservationID,
		ExpectedRevision: unknownTermination.Reservation.Revision,
		EventKey:         "unknown-usage-terminal",
		ReceiptID:        "unknown-usage-receipt",
		TerminationKnown: true,
	}
	finalized, err := fixture.service.Settle(ctx, finalCommand)
	if err != nil || !finalized.Finalized || finalized.Reservation.SettledMicroUsd != 100 {
		t.Fatalf("unknown usage finalization = %+v, %v; want full-cap charge", finalized, err)
	}
	duplicate, err := fixture.service.Settle(ctx, finalCommand)
	if err != nil || !duplicate.Duplicate {
		t.Fatalf("duplicate settlement = %+v, %v; want duplicate", duplicate, err)
	}
	assertBudgetRootTotals(t, fixture, first.BudgetRootID, 0, 100)
	if held := budgetCount(t, fixture.pool, `SELECT count(*) FROM governance_concurrency_hold WHERE workspace_id = $1 AND reservation_id = $2 AND state = 'held'`, fixture.workspaceID, first.ReservationID); held != 0 {
		t.Fatalf("terminal unknown-usage held slots = %d, want 0", held)
	}
	fixture.setEpoch(t, 4)
	fourth := fixture.reserveCommand("resource-d", budgetTestTime(14, 0), budgetTestTime(15, 0), 100, 100, 100, 4)
	if _, err := fixture.service.Reserve(ctx, fourth); err != nil {
		t.Fatalf("expired, terminal window blocked unrelated spend: %v", err)
	}
}

func TestBudgetLiabilityKnownSettlementReleasesOnlyProvenRemainder(t *testing.T) {
	fixture := newBudgetTestFixture(t)
	ctx := context.Background()
	first := fixture.reserveCommand("resource-a", budgetTestTime(12, 0), budgetTestTime(13, 0), 100, 100, 100, 1)
	reserved, err := fixture.service.Reserve(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	debited, err := fixture.service.Debit(ctx, BudgetDebitCommand{
		WorkspaceID:      fixture.workspaceID,
		ReservationID:    reserved.Reservation.ReservationID,
		ExpectedRevision: reserved.Reservation.Revision,
		EventKey:         "wire-1",
		AmountMicroUSD:   10,
	})
	if err != nil {
		t.Fatal(err)
	}
	settleCommand := BudgetSettleCommand{
		WorkspaceID:      fixture.workspaceID,
		ReservationID:    debited.Reservation.ReservationID,
		ExpectedRevision: debited.Reservation.Revision,
		EventKey:         "known-terminal",
		ReceiptID:        "known-receipt",
		UsageKnown:       true,
		TerminationKnown: true,
		UsageMicroUSD:    20,
	}
	settled, err := fixture.service.Settle(ctx, settleCommand)
	if err != nil || !settled.Finalized || settled.Reservation.SettledMicroUsd != 20 {
		t.Fatalf("known settlement = %+v, %v", settled, err)
	}
	duplicate, err := fixture.service.Settle(ctx, settleCommand)
	if err != nil || !duplicate.Duplicate {
		t.Fatalf("duplicate known settlement = %+v, %v", duplicate, err)
	}
	assertBudgetRootTotals(t, fixture, first.BudgetRootID, 0, 20)

	fixture.setEpoch(t, 2)
	next := fixture.reserveCommand("resource-b", budgetTestTime(12, 0), budgetTestTime(13, 0), 100, 100, 80, 2)
	next.BudgetRootID = first.BudgetRootID
	if _, err := fixture.service.Reserve(ctx, next); err != nil {
		t.Fatalf("reservation after proven refund: %v", err)
	}
}

func TestBudgetLiabilityExactBoundaryAndWorkspaceIsolation(t *testing.T) {
	fixture := newBudgetTestFixture(t)
	ctx := context.Background()
	first := fixture.reserveCommand("resource-a", budgetTestTime(12, 0), budgetTestTime(13, 0), 100, 100, 10, 1)
	reserved, err := fixture.service.Reserve(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.Settle(ctx, BudgetSettleCommand{
		WorkspaceID:      fixture.workspaceID,
		ReservationID:    reserved.Reservation.ReservationID,
		ExpectedRevision: reserved.Reservation.Revision,
		EventKey:         "zero-usage",
		ReceiptID:        "zero-usage-receipt",
		UsageKnown:       true,
		TerminationKnown: true,
	}); err != nil {
		t.Fatal(err)
	}
	fixture.clock.Set(budgetTestTime(13, 0))
	fixture.setEpoch(t, 2)
	duplicate, err := fixture.service.Reserve(ctx, first)
	if err != nil || !duplicate.Duplicate {
		t.Fatalf("duplicate reservation after window end = %+v, %v; want duplicate", duplicate, err)
	}
	stale := fixture.reserveCommand("resource-b", budgetTestTime(12, 0), budgetTestTime(13, 0), 100, 100, 1, 2)
	if _, err := fixture.service.Reserve(ctx, stale); !errors.Is(err, ErrBudgetWindow) {
		t.Fatalf("reservation at exact end error = %v, want ErrBudgetWindow", err)
	}
	assertBudgetNoArtifacts(t, fixture, stale.ReservationID)
	current := fixture.reserveCommand("resource-c", budgetTestTime(13, 0), budgetTestTime(14, 0), 100, 100, 100, 2)
	if _, err := fixture.service.Reserve(ctx, current); err != nil {
		t.Fatalf("new window blocked by finalized prior window: %v", err)
	}

	neighbor := newBudgetTestFixture(t)
	neighbor.clock.Set(budgetTestTime(13, 5))
	neighborRequest := neighbor.reserveCommand("resource-a", budgetTestTime(13, 0), budgetTestTime(14, 0), 100, 100, 100, 1)
	if _, err := neighbor.service.Reserve(ctx, neighborRequest); err != nil {
		t.Fatalf("neighboring workspace admission: %v", err)
	}
}

func TestBudgetLiabilityRejectsUnboundedRetryAndOverflow(t *testing.T) {
	fixture := newBudgetTestFixture(t)
	windowStart := budgetTestTime(12, 0)
	windowEnd := budgetTestTime(13, 0)
	unbounded := fixture.reserveCommand("resource-a", windowStart, windowEnd, 100, 100, 10, 1)
	unbounded.RetryPolicyBounded = false
	if _, err := fixture.service.Reserve(context.Background(), unbounded); !errors.Is(err, ErrBudgetRetryUnbounded) {
		t.Fatalf("unbounded retry error = %v, want ErrBudgetRetryUnbounded", err)
	}
	overflow := fixture.reserveCommand("resource-b", windowStart, windowEnd, math.MaxInt64, math.MaxInt64, math.MaxInt64, 1)
	overflow.RetryAllowance = 1
	if _, err := fixture.service.Reserve(context.Background(), overflow); !errors.Is(err, ErrBudgetOverflow) {
		t.Fatalf("overflow error = %v, want ErrBudgetOverflow", err)
	}
	assertBudgetNoArtifacts(t, fixture, unbounded.ReservationID)
	assertBudgetNoArtifacts(t, fixture, overflow.ReservationID)
}

func TestBudgetLiabilityConcurrentAdmissionSharesWorkspaceCap(t *testing.T) {
	fixture := newBudgetTestFixture(t)
	start := make(chan struct{})
	results := make(chan error, 2)
	commands := []BudgetReserveCommand{
		fixture.reserveCommand("resource-a", budgetTestTime(12, 0), budgetTestTime(13, 0), 100, 100, 60, 1),
		fixture.reserveCommand("resource-b", budgetTestTime(12, 0), budgetTestTime(13, 0), 100, 100, 60, 1),
	}
	for _, command := range commands {
		go func(command BudgetReserveCommand) {
			<-start
			_, err := fixture.service.Reserve(context.Background(), command)
			results <- err
		}(command)
	}
	close(start)
	var admitted, rejected int
	for range commands {
		err := <-results
		switch {
		case err == nil:
			admitted++
		case errors.Is(err, ErrBudgetLimit):
			rejected++
		default:
			t.Fatalf("concurrent reservation error = %v", err)
		}
	}
	if admitted != 1 || rejected != 1 {
		t.Fatalf("concurrent admissions/rejections = %d/%d, want 1/1", admitted, rejected)
	}
	if exposure := budgetCount(t, fixture.pool, `SELECT COALESCE(sum(reserved_micro_usd + spent_micro_usd), 0) FROM governance_budget_window WHERE workspace_id = $1`, fixture.workspaceID); exposure != 60 {
		t.Fatalf("concurrent window exposure = %d, want 60", exposure)
	}
}

func budgetTestTime(hour, minute int) time.Time {
	return time.Date(2030, time.January, 2, hour, minute, 0, 0, time.UTC)
}

func budgetTestUUID() pgtype.UUID {
	return pgtype.UUID{Bytes: uuid.New(), Valid: true}
}

func budgetCount(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int64 {
	t.Helper()
	var count int64
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func assertBudgetNoArtifacts(t *testing.T, fixture *budgetTestFixture, reservationID pgtype.UUID) {
	t.Helper()
	for _, table := range []string{"governance_budget_reservation", "governance_budget_journal", "governance_budget_outbox", "governance_concurrency_hold"} {
		query := "SELECT count(*) FROM " + table + " WHERE workspace_id = $1 AND reservation_id = $2"
		if count := budgetCount(t, fixture.pool, query, fixture.workspaceID, reservationID); count != 0 {
			t.Fatalf("rejected reservation left %d rows in %s", count, table)
		}
	}
}

func assertBudgetRootTotals(t *testing.T, fixture *budgetTestFixture, rootID pgtype.UUID, wantReserved, wantSpent int64) {
	t.Helper()
	var reserved, spent int64
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT reserved_micro_usd, spent_micro_usd FROM governance_budget_root
		WHERE workspace_id = $1 AND budget_root_id = $2
	`, fixture.workspaceID, rootID).Scan(&reserved, &spent); err != nil {
		t.Fatal(err)
	}
	if reserved != wantReserved || spent != wantSpent {
		t.Fatalf("root counters = %d/%d, want %d/%d", reserved, spent, wantReserved, wantSpent)
	}
}
