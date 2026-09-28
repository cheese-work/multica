package governance

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type budgetFixture struct {
	concurrencyFixture
	windowCap int64
}

func newBudgetFixture(t *testing.T) budgetFixture {
	t.Helper()
	fixture := budgetFixture{concurrencyFixture: newConcurrencyFixture(t), windowCap: 1000}
	fixture.setWindowCap(t, fixture.windowCap)
	t.Cleanup(func() {
		ctx := context.Background()
		for _, table := range []string{
			"governance_budget_outbox",
			"governance_budget_journal",
			"governance_budget_reservation",
			"governance_budget_root",
			"governance_budget_window",
		} {
			if _, err := fixture.pool.Exec(ctx, "DELETE FROM "+table+" WHERE workspace_id = $1", fixture.workspaceID); err != nil {
				t.Errorf("cleanup %s: %v", table, err)
			}
		}
	})
	return fixture
}

func (fixture budgetFixture) setWindowCap(t *testing.T, cap int64) {
	t.Helper()
	if _, err := fixture.pool.Exec(context.Background(), `
		UPDATE governance_workspace_config
		SET settings = jsonb_set(
		    settings,
		    '{limits}',
		    COALESCE(settings->'limits', '{}'::jsonb) || jsonb_build_object(
		        'workspace_spend_cap_micro_usd', $2::bigint,
		        'admission_window_seconds', 3600
		    ),
		    true
		)
		WHERE workspace_id = $1
	`, fixture.workspaceID, cap); err != nil {
		t.Fatal(err)
	}
}

func (fixture budgetFixture) request(now time.Time) BudgetRequest {
	return BudgetRequest{
		ReservationID:          concurrencyUUID(),
		BudgetRootID:           concurrencyUUID(),
		CaseID:                 concurrencyUUID(),
		AttemptID:              concurrencyUUID(),
		ObligationID:           concurrencyUUID(),
		Resource:               "jev",
		ConcurrencyLimit:       2,
		ControlEpoch:           1,
		RootCapMicroUSD:        500,
		WindowStart:            now.Truncate(time.Hour),
		WindowEnd:              now.Truncate(time.Hour).Add(time.Hour),
		MaxAttemptCostMicroUSD: 100,
		RetryAllowance:         1,
		RetryPolicyBounded:     true,
	}
}

func (fixture budgetFixture) reserve(t *testing.T, request BudgetRequest, now time.Time) (BudgetReservation, bool) {
	t.Helper()
	ctx := context.Background()
	tx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	reservation, duplicate, err := ReserveBudget(ctx, tx, fixture.workspaceID, request, now)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return reservation, duplicate
}

func (fixture budgetFixture) tryReserve(request BudgetRequest, now time.Time) error {
	ctx := context.Background()
	tx, err := fixture.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	_, _, err = ReserveBudget(ctx, tx, fixture.workspaceID, request, now)
	return err
}

func (fixture budgetFixture) debit(t *testing.T, request BudgetDebit, now time.Time) (BudgetReservation, bool, error) {
	t.Helper()
	ctx := context.Background()
	tx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	reservation, duplicate, err := DebitBudget(ctx, tx, fixture.workspaceID, request, now)
	if err != nil {
		_ = tx.Rollback(ctx)
		return reservation, duplicate, err
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return reservation, duplicate, nil
}

func (fixture budgetFixture) settle(t *testing.T, settlement BudgetSettlement, now time.Time) (BudgetReservation, bool, error) {
	t.Helper()
	ctx := context.Background()
	tx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	reservation, duplicate, err := SettleBudget(ctx, tx, fixture.workspaceID, settlement, now)
	if err != nil {
		_ = tx.Rollback(ctx)
		return reservation, duplicate, err
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return reservation, duplicate, nil
}

func (fixture budgetFixture) totals(t *testing.T, rootID pgtype.UUID, windowStart time.Time) (int64, int64, int64, int64) {
	t.Helper()
	ctx := context.Background()
	var rootReserved, rootSpent, windowReserved, windowSpent int64
	if err := fixture.pool.QueryRow(ctx, `
		SELECT reserved_micro_usd, spent_micro_usd FROM governance_budget_root
		WHERE workspace_id = $1 AND budget_root_id = $2
	`, fixture.workspaceID, rootID).Scan(&rootReserved, &rootSpent); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pool.QueryRow(ctx, `
		SELECT reserved_micro_usd, spent_micro_usd FROM governance_budget_window
		WHERE workspace_id = $1 AND window_start = $2
	`, fixture.workspaceID, windowStart).Scan(&windowReserved, &windowSpent); err != nil {
		t.Fatal(err)
	}
	return rootReserved, rootSpent, windowReserved, windowSpent
}

func TestBudgetConcurrentAdmissionsRespectLastSlot(t *testing.T) {
	fixture := newBudgetFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	requests := []BudgetRequest{fixture.request(now), fixture.request(now)}
	for index := range requests {
		requests[index].ConcurrencyLimit = 1
	}
	results := make(chan error, len(requests))
	start := make(chan struct{})
	var workers sync.WaitGroup
	transactions := make([]pgx.Tx, len(requests))
	connections := make(map[uint32]bool)
	for index := range requests {
		tx, err := fixture.pool.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		transactions[index] = tx
		connections[tx.Conn().PgConn().PID()] = true
	}
	if len(connections) != 2 {
		t.Fatal("concurrency test did not use independent PostgreSQL connections")
	}
	workers.Add(len(requests))
	for index, tx := range transactions {
		go func(index int, tx pgx.Tx) {
			defer workers.Done()
			<-start
			_, _, err := ReserveBudget(context.Background(), tx, fixture.workspaceID, requests[index], now)
			if err == nil {
				err = tx.Commit(context.Background())
			} else {
				_ = tx.Rollback(context.Background())
			}
			results <- err
		}(index, tx)
	}
	close(start)
	workers.Wait()
	close(results)
	admitted, refused := 0, 0
	for err := range results {
		switch {
		case err == nil:
			admitted++
		case errors.Is(err, ErrConcurrencyLimit):
			refused++
		default:
			t.Fatalf("unexpected admission result: %v", err)
		}
	}
	if admitted != 1 || refused != 1 {
		t.Fatalf("admitted/refused = %d/%d, want 1/1", admitted, refused)
	}
	fixture.counts(t, "jev", 1, 1)
}

func TestBudgetConcurrentAdmissionsRespectLastCredit(t *testing.T) {
	fixture := newBudgetFixture(t)
	fixture.setWindowCap(t, 100)
	now := time.Now().UTC().Truncate(time.Microsecond)
	requests := []BudgetRequest{fixture.request(now), fixture.request(now)}
	for index := range requests {
		requests[index].ConcurrencyLimit = 2
		requests[index].RetryAllowance = 0
		requests[index].BudgetRootID = concurrencyUUID()
		requests[index].MaxAttemptCostMicroUSD = 100
	}
	results := make(chan error, len(requests))
	start := make(chan struct{})
	var workers sync.WaitGroup
	transactions := make([]pgx.Tx, len(requests))
	connections := make(map[uint32]bool)
	for index := range requests {
		tx, err := fixture.pool.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		transactions[index] = tx
		connections[tx.Conn().PgConn().PID()] = true
	}
	if len(connections) != 2 {
		t.Fatal("spend test did not use independent PostgreSQL connections")
	}
	workers.Add(len(requests))
	for index, tx := range transactions {
		go func(index int, tx pgx.Tx) {
			defer workers.Done()
			<-start
			_, _, err := ReserveBudget(context.Background(), tx, fixture.workspaceID, requests[index], now)
			if err == nil {
				err = tx.Commit(context.Background())
			} else {
				_ = tx.Rollback(context.Background())
			}
			results <- err
		}(index, tx)
	}
	close(start)
	workers.Wait()
	close(results)
	admitted, refused := 0, 0
	for err := range results {
		switch {
		case err == nil:
			admitted++
		case errors.Is(err, ErrBudgetLimit):
			refused++
		default:
			t.Fatalf("unexpected spend result: %v", err)
		}
	}
	if admitted != 1 || refused != 1 {
		t.Fatalf("admitted/refused = %d/%d, want 1/1", admitted, refused)
	}
	var reserved int64
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT reserved_micro_usd FROM governance_budget_window
		WHERE workspace_id = $1 AND window_start = $2
	`, fixture.workspaceID, requests[0].WindowStart).Scan(&reserved); err != nil || reserved != 100 {
		t.Fatalf("window reservation = %d, want 100: %v", reserved, err)
	}
}

func TestBudgetReservationIsIdempotentAndRollbackSafe(t *testing.T) {
	fixture := newBudgetFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	request := fixture.request(now)
	connection, err := fixture.pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	tx, err := connection.Begin(context.Background())
	if err != nil {
		connection.Release()
		t.Fatal(err)
	}
	connectionReleased := false
	t.Cleanup(func() {
		_ = tx.Rollback(context.Background())
		if !connectionReleased {
			connection.Release()
		}
	})
	if _, _, err := ReserveBudget(context.Background(), tx, fixture.workspaceID, request, now); err != nil {
		t.Fatal(err)
	}
	if err := connection.Conn().Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	connection.Release()
	connectionReleased = true
	reservation, duplicate := fixture.reserve(t, request, now)
	if duplicate || reservation.Revision != 1 || reservation.RemainingMicroUSD != 200 {
		t.Fatalf("restarted reservation = %+v duplicate=%v", reservation, duplicate)
	}
	reservation, duplicate = fixture.reserve(t, request, now)
	if !duplicate || reservation.Revision != 1 {
		t.Fatalf("duplicate reservation = %+v duplicate=%v", reservation, duplicate)
	}
	changed := request
	changed.MaxAttemptCostMicroUSD++
	tx, err = fixture.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = ReserveBudget(context.Background(), tx, fixture.workspaceID, changed, now)
	_ = tx.Rollback(context.Background())
	if !errors.Is(err, ErrBudgetIdempotencyConflict) {
		t.Fatalf("conflicting reservation identity returned %v", err)
	}
	var outbox, journal int64
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM governance_budget_outbox WHERE workspace_id = $1 AND reservation_id = $2
	`, fixture.workspaceID, request.ReservationID).Scan(&outbox); err != nil || outbox != 1 {
		t.Fatalf("outbox count = %d: %v", outbox, err)
	}
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM governance_budget_journal WHERE workspace_id = $1 AND reservation_id = $2
	`, fixture.workspaceID, request.ReservationID).Scan(&journal); err != nil || journal != 1 {
		t.Fatalf("journal count = %d: %v", journal, err)
	}
	rootReserved, rootSpent, windowReserved, windowSpent := fixture.totals(t, request.BudgetRootID, request.WindowStart)
	if rootReserved != 200 || rootSpent != 0 || windowReserved != 200 || windowSpent != 0 {
		t.Fatalf("reserve totals = %d/%d %d/%d", rootReserved, rootSpent, windowReserved, windowSpent)
	}
}

func TestBudgetRejectsUnboundedRetriesAndExpiredWindows(t *testing.T) {
	fixture := newBudgetFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	request := fixture.request(now)
	request.RetryPolicyBounded = false
	tx, err := fixture.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = ReserveBudget(context.Background(), tx, fixture.workspaceID, request, now)
	_ = tx.Rollback(context.Background())
	if !errors.Is(err, ErrBudgetUnboundedRetries) {
		t.Fatalf("unbounded retry policy returned %v", err)
	}
	request = fixture.request(now)
	_, _ = fixture.reserve(t, request, now)
	_, _, err = fixture.debit(t, BudgetDebit{
		ReservationID:    request.ReservationID,
		ExpectedRevision: 1,
		EventKey:         "initial-wire",
	}, request.WindowEnd)
	if !errors.Is(err, ErrBudgetWindowClosed) {
		t.Fatalf("expired window allowed a new wire attempt: %v", err)
	}
	fixture.counts(t, request.Resource, 1, 1)
}

func TestBudgetRetryAllowanceIsPersistedAndDebited(t *testing.T) {
	fixture := newBudgetFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	request := fixture.request(now)
	reserved, duplicate := fixture.reserve(t, request, now)
	if duplicate || reserved.RetryAllowanceRemaining != 1 {
		t.Fatalf("initial retry allowance = %+v", reserved)
	}
	first, _, err := fixture.debit(t, BudgetDebit{
		ReservationID:    request.ReservationID,
		ExpectedRevision: reserved.Revision,
		EventKey:         "wire-1",
	}, now)
	if err != nil || first.AttemptsStarted != 1 || first.DebitedMicroUSD != 100 || first.RemainingMicroUSD != 100 {
		t.Fatalf("initial debit = %+v: %v", first, err)
	}
	retry, duplicate, err := fixture.debit(t, BudgetDebit{
		ReservationID:    request.ReservationID,
		ExpectedRevision: first.Revision,
		EventKey:         "wire-2",
	}, now)
	if err != nil || duplicate || retry.RetryAllowanceRemaining != 0 || retry.AttemptsStarted != 2 || retry.RemainingMicroUSD != 0 {
		t.Fatalf("bounded retry debit = %+v duplicate=%v: %v", retry, duplicate, err)
	}
	if replay, duplicate, err := fixture.debit(t, BudgetDebit{
		ReservationID:    request.ReservationID,
		ExpectedRevision: first.Revision,
		EventKey:         "wire-2",
	}, now); err != nil || !duplicate || replay.Revision != retry.Revision {
		t.Fatalf("replayed retry = %+v duplicate=%v: %v", replay, duplicate, err)
	}
	_, _, err = fixture.debit(t, BudgetDebit{
		ReservationID:    request.ReservationID,
		ExpectedRevision: retry.Revision,
		EventKey:         "wire-3",
	}, now)
	if !errors.Is(err, ErrBudgetRetryAllowance) {
		t.Fatalf("retry beyond persisted allowance returned %v", err)
	}
	fixture.counts(t, request.Resource, 1, 1)
}

func TestBudgetRejectsMisalignedWindow(t *testing.T) {
	fixture := newBudgetFixture(t)
	now := time.Date(2026, 9, 28, 12, 30, 0, 0, time.UTC)
	request := fixture.request(now)
	request.WindowStart = request.WindowStart.Add(5 * time.Minute)
	request.WindowEnd = request.WindowEnd.Add(5 * time.Minute)

	if err := fixture.tryReserve(request, now); !errors.Is(err, ErrBudgetInput) {
		t.Fatalf("misaligned window error = %v, want %v", err, ErrBudgetInput)
	}
}

func TestBudgetUnknownTerminationRetainsSlotAndUnknownUsageSettlesFullCap(t *testing.T) {
	fixture := newBudgetFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	request := fixture.request(now)
	reserved, _ := fixture.reserve(t, request, now)
	unknown := BudgetSettlement{
		ReservationID:    request.ReservationID,
		ExpectedRevision: reserved.Revision,
		EventKey:         "receipt-unknown-termination",
		ReceiptID:        "billing-receipt-unknown-termination",
		UsageKnown:       false,
		TerminationKnown: false,
	}
	if _, _, err := fixture.settle(t, unknown, now); !errors.Is(err, ErrBudgetLiabilityUnknown) {
		t.Fatalf("unknown termination settled: %v", err)
	}
	rootReserved, rootSpent, windowReserved, windowSpent := fixture.totals(t, request.BudgetRootID, request.WindowStart)
	if rootReserved != 200 || rootSpent != 0 || windowReserved != 200 || windowSpent != 0 {
		t.Fatalf("unknown liability changed totals: %d/%d %d/%d", rootReserved, rootSpent, windowReserved, windowSpent)
	}
	fixture.counts(t, request.Resource, 1, 1)
	unknown.TerminationKnown = true
	unknown.EventKey = "receipt-terminal-usage-unknown"
	unknown.ReceiptID = "billing-receipt-terminal-usage-unknown"
	unknown.ExpectedRevision = reserved.Revision
	settled, duplicate, err := fixture.settle(t, unknown, now)
	if err != nil || duplicate || settled.State != "settled" || settled.SettledMicroUSD != 200 {
		t.Fatalf("unknown usage settlement = %+v duplicate=%v: %v", settled, duplicate, err)
	}
	fixture.counts(t, request.Resource, 0, 1)
	rootReserved, rootSpent, windowReserved, windowSpent = fixture.totals(t, request.BudgetRootID, request.WindowStart)
	if rootReserved != 0 || rootSpent != 200 || windowReserved != 0 || windowSpent != 200 {
		t.Fatalf("unknown usage was not charged full cap: %d/%d %d/%d", rootReserved, rootSpent, windowReserved, windowSpent)
	}
}

func TestBudgetSettlementRefundsProvenRemainderOnce(t *testing.T) {
	fixture := newBudgetFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	request := fixture.request(now)
	reserved, _ := fixture.reserve(t, request, now)
	debited, _, err := fixture.debit(t, BudgetDebit{
		ReservationID:    request.ReservationID,
		ExpectedRevision: reserved.Revision,
		EventKey:         "wire-1",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	settlement := BudgetSettlement{
		ReservationID:    request.ReservationID,
		ExpectedRevision: debited.Revision,
		EventKey:         "receipt-42",
		ReceiptID:        "billing-receipt-42",
		UsageKnown:       true,
		UsageMicroUSD:    35,
		TerminationKnown: true,
	}
	settled, duplicate, err := fixture.settle(t, settlement, now)
	if err != nil || duplicate || settled.SettledMicroUSD != 35 || settled.RemainingMicroUSD != 0 {
		t.Fatalf("settlement = %+v duplicate=%v: %v", settled, duplicate, err)
	}
	if replay, duplicate, err := fixture.settle(t, settlement, now); err != nil || !duplicate || replay.Revision != settled.Revision {
		t.Fatalf("duplicate settlement = %+v duplicate=%v: %v", replay, duplicate, err)
	}
	conflicting := settlement
	conflicting.UsageMicroUSD++
	if _, _, err := fixture.settle(t, conflicting, now); !errors.Is(err, ErrBudgetIdempotencyConflict) {
		t.Fatalf("conflicting receipt replay returned %v", err)
	}
	rootReserved, rootSpent, windowReserved, windowSpent := fixture.totals(t, request.BudgetRootID, request.WindowStart)
	if rootReserved != 0 || rootSpent != 35 || windowReserved != 0 || windowSpent != 35 {
		t.Fatalf("settlement/refund applied more than once: %d/%d %d/%d", rootReserved, rootSpent, windowReserved, windowSpent)
	}
	fixture.counts(t, request.Resource, 0, 1)
}

func TestBudgetCASAndBillingReceiptIdentity(t *testing.T) {
	fixture := newBudgetFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	firstRequest := fixture.request(now)
	firstRequest.ConcurrencyLimit = 2
	firstReservation, _ := fixture.reserve(t, firstRequest, now)
	_, _, err := fixture.debit(t, BudgetDebit{
		ReservationID:    firstRequest.ReservationID,
		ExpectedRevision: firstReservation.Revision + 1,
		EventKey:         "stale-wire",
	}, now)
	if !errors.Is(err, ErrBudgetCAS) {
		t.Fatalf("stale revision returned %v", err)
	}
	debited, _, err := fixture.debit(t, BudgetDebit{
		ReservationID:    firstRequest.ReservationID,
		ExpectedRevision: firstReservation.Revision,
		EventKey:         "wire-one",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	tooLarge := BudgetSettlement{
		ReservationID:    firstRequest.ReservationID,
		ExpectedRevision: debited.Revision,
		EventKey:         "receipt-too-large",
		ReceiptID:        "receipt-too-large",
		UsageKnown:       true,
		UsageMicroUSD:    firstReservation.TotalCapMicroUSD + 1,
		TerminationKnown: true,
	}
	if _, _, err := fixture.settle(t, tooLarge, now); !errors.Is(err, ErrBudgetUsageExceedsCap) {
		t.Fatalf("over-cap receipt returned %v", err)
	}
	firstSettlement := BudgetSettlement{
		ReservationID:    firstRequest.ReservationID,
		ExpectedRevision: debited.Revision,
		EventKey:         "receipt-shared-one",
		ReceiptID:        "receipt-shared",
		UsageKnown:       true,
		UsageMicroUSD:    35,
		TerminationKnown: true,
	}
	if _, _, err := fixture.settle(t, firstSettlement, now); err != nil {
		t.Fatal(err)
	}
	secondRequest := fixture.request(now)
	secondRequest.ConcurrencyLimit = 2
	secondReservation, _ := fixture.reserve(t, secondRequest, now)
	secondSettlement := BudgetSettlement{
		ReservationID:    secondRequest.ReservationID,
		ExpectedRevision: secondReservation.Revision,
		EventKey:         "receipt-shared-two",
		ReceiptID:        firstSettlement.ReceiptID,
		UsageKnown:       true,
		UsageMicroUSD:    20,
		TerminationKnown: true,
	}
	if _, _, err := fixture.settle(t, secondSettlement, now); !errors.Is(err, ErrBudgetIdempotencyConflict) {
		t.Fatalf("receipt reuse across reservations returned %v", err)
	}
	fixture.counts(t, secondRequest.Resource, 1, 2)
}

func TestBudgetRootLiabilitySurvivesRolloverAndLimitReduction(t *testing.T) {
	fixture := newBudgetFixture(t)
	fixture.setWindowCap(t, 500)
	now := time.Now().UTC().Truncate(time.Microsecond)
	request := fixture.request(now)
	request.RootCapMicroUSD = 150
	request.RetryAllowance = 0
	request.ConcurrencyLimit = 2
	_, _ = fixture.reserve(t, request, now)
	nextWindow := request.WindowEnd
	rolled := request
	rolled.ReservationID = concurrencyUUID()
	rolled.CaseID = concurrencyUUID()
	rolled.AttemptID = concurrencyUUID()
	rolled.ObligationID = concurrencyUUID()
	rolled.WindowStart = nextWindow
	rolled.WindowEnd = nextWindow.Add(time.Hour)
	err := fixture.tryReserve(rolled, nextWindow.Add(time.Second))
	if !errors.Is(err, ErrBudgetLimit) {
		t.Fatalf("new window ignored the root liability: %v", err)
	}
	rolled.RootCapMicroUSD = 50
	err = fixture.tryReserve(rolled, nextWindow.Add(time.Second))
	if !errors.Is(err, ErrBudgetLimit) {
		t.Fatalf("limit reduction discarded old liability: %v", err)
	}
	fixture.counts(t, request.Resource, 1, 1)
	rootReserved, rootSpent, windowReserved, windowSpent := fixture.totals(t, request.BudgetRootID, request.WindowStart)
	if rootReserved != 100 || rootSpent != 0 || windowReserved != 100 || windowSpent != 0 {
		t.Fatalf("rollover/rejected admission changed prior liability: %d/%d %d/%d", rootReserved, rootSpent, windowReserved, windowSpent)
	}
	settled, _, err := fixture.settle(t, BudgetSettlement{
		ReservationID:    request.ReservationID,
		ExpectedRevision: 1,
		EventKey:         "late-terminal-receipt",
		ReceiptID:        "late-terminal-receipt",
		UsageKnown:       false,
		TerminationKnown: true,
	}, nextWindow.Add(2*time.Hour))
	if err != nil || settled.SettledMicroUSD != 100 {
		t.Fatalf("old-window liability failed late settlement: %+v/%v", settled, err)
	}
	fixture.counts(t, request.Resource, 0, 1)
}
