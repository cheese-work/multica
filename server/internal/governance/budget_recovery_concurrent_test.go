package governance

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

// R5 rows: concurrent, reconfigured and restarted workers. Names contain
// "Concurrent" so the plan's -count=20 command selects them.

type recoveryOutcome struct {
	err  error
	kind string
}

// runConcurrently releases every worker from one barrier and joins them all.
func runConcurrently(workers ...func() recoveryOutcome) []recoveryOutcome {
	start := make(chan struct{})
	results := make([]recoveryOutcome, len(workers))
	var wait sync.WaitGroup
	for index, worker := range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			results[index] = worker()
		}()
	}
	close(start)
	wait.Wait()
	return results
}

func countOutcomes(results []recoveryOutcome, want error) (ok, rejected int) {
	for _, result := range results {
		switch {
		case result.err == nil && result.kind == "":
			ok++
		case errors.Is(result.err, want) || (result.kind != "" && errors.Is(recoveryErrors[result.kind], want)):
			rejected++
		}
	}
	return ok, rejected
}

func TestBudgetRecoveryConcurrentOneRemainingCreditAdmitsOnce(t *testing.T) {
	t.Run("R5a_goroutines_race_one_remaining_root_credit", func(t *testing.T) {
		fixture := newRecoveryFixture(t)
		fixture.command.BudgetPolicy.RootCapMicroUSD = 60 // total cap per attempt = 20 * (1 + 2 retries) = 60
		second := nextRecoveryCommand(t, fixture, fixture.command.BudgetPolicy.Resource)
		second.BudgetPolicy = fixture.command.BudgetPolicy
		results := runConcurrently(
			func() recoveryOutcome {
				_, err := fixture.service.Admit(context.Background(), fixture.command)
				return recoveryOutcome{err: err}
			},
			func() recoveryOutcome {
				_, err := fixture.service.Admit(context.Background(), second)
				return recoveryOutcome{err: err}
			},
		)
		if ok, rejected := countOutcomes(results, ErrBudgetLimit); ok != 1 || rejected != 1 {
			t.Fatalf("outcomes = %+v, want exactly one admission and one ErrBudgetLimit", results)
		}
		assertRecoveryConservation(t, fixture.budget.pool, fixture.command.WorkspaceID)
	})
	t.Run("R5b_independent_restarted_processes_race_one_remaining_slot", func(t *testing.T) {
		fixture := newRecoveryFixture(t)
		fixture.command.BudgetPolicy.SlotLimit = 1
		commands := []AdmissionCommand{fixture.command}
		for range 2 {
			next := nextRecoveryCommand(t, fixture, fixture.command.BudgetPolicy.Resource)
			next.BudgetPolicy = fixture.command.BudgetPolicy
			commands = append(commands, next)
		}
		workers := make([]func() recoveryOutcome, len(commands))
		for index := range commands {
			command := commands[index]
			workers[index] = func() recoveryOutcome {
				response := runRecoveryProcess(t, recoveryRequest{Action: "admit", Clock: fixture.budget.clock.Now(), Admit: &command})
				return recoveryOutcome{kind: response.ErrorKind}
			}
		}
		results := runConcurrently(workers...)
		if ok, rejected := countOutcomes(results, ErrConcurrencyLimit); ok != 1 || rejected != 2 {
			t.Fatalf("subprocess outcomes = %+v, want one admission and two ErrConcurrencyLimit", results)
		}
		if got := budgetCount(t, fixture.budget.pool, `SELECT count(*) FROM governance_concurrency_hold WHERE workspace_id = $1 AND state = 'held'`, fixture.command.WorkspaceID); got != 1 {
			t.Fatalf("held slots = %d, want 1", got)
		}
		assertRecoveryConservation(t, fixture.budget.pool, fixture.command.WorkspaceID)
	})
}

func TestBudgetRecoveryConcurrentLockWaitAcrossWindowExpiry(t *testing.T) {
	t.Run("R5c_admission_waiting_across_window_end_rejects_with_zero_artifacts", func(t *testing.T) {
		fixture := newRecoveryFixture(t)
		fixture.command.DeadlineAt = budgetTestTime(13, 3) // past the window end but inside the case deadline, so only the billing window can reject
		fixture.budget.clock.Set(budgetTestTime(12, 59))
		holderPID, release := holdWorkspaceControl(t, fixture.budget.pool, fixture.command.WorkspaceID)
		result := make(chan error, 1)
		go func() { _, err := fixture.service.Admit(context.Background(), fixture.command); result <- err }()
		recoveryWaitBlockedBy(t, fixture.budget.pool, holderPID)
		fixture.budget.clock.Set(fixture.command.BudgetPolicy.WindowEnd)
		release()
		if err := <-result; !errors.Is(err, ErrBudgetWindow) {
			t.Fatalf("admission after lock wait = %v, want ErrBudgetWindow", err)
		}
		assertNoAdmissionArtifacts(t, fixture)
		assertRecoveryConservation(t, fixture.budget.pool, fixture.command.WorkspaceID)
	})
	t.Run("R5d_debit_waiting_across_window_end_rejects_with_zero_partial_debit", func(t *testing.T) {
		fixture := newRecoveryFixture(t)
		fixture.command.DeadlineAt = budgetTestTime(13, 3) // past the window end but inside the case deadline, so only the billing window can reject
		fixture.budget.clock.Set(budgetTestTime(12, 59))
		reserved := admitRecovery(t, fixture)
		holderPID, release := holdWorkspaceControl(t, fixture.budget.pool, fixture.command.WorkspaceID)
		result := make(chan error, 1)
		go func() {
			_, err := fixture.budget.service.Debit(context.Background(), BudgetDebitCommand{
				WorkspaceID: fixture.command.WorkspaceID, ReservationID: fixture.command.ReservationID,
				ExpectedRevision: reserved.Reservation.Revision, EventKey: "wire-1", AmountMicroUSD: 5,
			})
			result <- err
		}()
		recoveryWaitBlockedBy(t, fixture.budget.pool, holderPID)
		fixture.budget.clock.Set(fixture.command.BudgetPolicy.WindowEnd)
		release()
		if err := <-result; !errors.Is(err, ErrBudgetWindow) {
			t.Fatalf("debit after lock wait = %v, want ErrBudgetWindow", err)
		}
		if row := readRecoveryReservation(t, fixture.budget.pool, fixture.command.WorkspaceID, fixture.command.ReservationID); row.Debited != 0 || row.Revision != 1 {
			t.Fatalf("expired debit persisted %+v", row)
		}
		if got := budgetCount(t, fixture.budget.pool, `SELECT count(*) FROM governance_budget_journal WHERE workspace_id = $1 AND event_key = 'wire-1'`, fixture.command.WorkspaceID); got != 0 {
			t.Fatalf("expired debit left %d journal rows", got)
		}
		assertRecoveryConservation(t, fixture.budget.pool, fixture.command.WorkspaceID)
	})
}

func TestBudgetRecoveryConcurrentDebitAndSettleConserveBudget(t *testing.T) {
	t.Run("R5e_debit_versus_settle_preserves_conservation", func(t *testing.T) {
		fixture := newRecoveryFixture(t)
		reserved := admitRecovery(t, fixture)
		revision := reserved.Reservation.Revision
		results := runConcurrently(
			func() recoveryOutcome {
				_, err := fixture.budget.service.Debit(context.Background(), BudgetDebitCommand{
					WorkspaceID: fixture.command.WorkspaceID, ReservationID: fixture.command.ReservationID,
					ExpectedRevision: revision, EventKey: "wire-1", AmountMicroUSD: 5,
				})
				return recoveryOutcome{err: err}
			},
			func() recoveryOutcome {
				_, err := fixture.budget.service.Settle(context.Background(), *settleRecovery(fixture, revision, "settle-1", "receipt-1", true, true, 5))
				return recoveryOutcome{err: err}
			},
		)
		// Whichever wins the workspace lock, the loser observes a changed
		// revision or terminal state and must not double count.
		for _, result := range results {
			if result.err != nil && !errors.Is(result.err, ErrBudgetRevision) && !errors.Is(result.err, ErrBudgetLimit) {
				t.Fatalf("unexpected loser error %v", result.err)
			}
		}
		row := readRecoveryReservation(t, fixture.budget.pool, fixture.command.WorkspaceID, fixture.command.ReservationID)
		if row.Debited > 5 {
			t.Fatalf("debited %d exceeds the single 5 micro-USD wire", row.Debited)
		}
		assertRecoveryConservation(t, fixture.budget.pool, fixture.command.WorkspaceID)
	})
	t.Run("R5f_settle_versus_settle_finalizes_once", func(t *testing.T) {
		fixture := newRecoveryFixture(t)
		reserved := admitRecovery(t, fixture)
		revision := reserved.Reservation.Revision
		same := func() recoveryOutcome {
			response, err := fixture.budget.service.Settle(context.Background(), *settleRecovery(fixture, revision, "settle-1", "receipt-1", true, true, 8))
			if err == nil && response.Duplicate {
				return recoveryOutcome{kind: "duplicate"}
			}
			return recoveryOutcome{err: err}
		}
		results := runConcurrently(same, same)
		finalized, duplicates := 0, 0
		for _, result := range results {
			switch {
			case result.err != nil:
				t.Fatalf("same-input settle failed: %v", result.err)
			case result.kind == "duplicate":
				duplicates++
			default:
				finalized++
			}
		}
		if finalized != 1 || duplicates != 1 {
			t.Fatalf("outcomes finalized/duplicate = %d/%d, want 1/1", finalized, duplicates)
		}
		conflicting := runConcurrently(
			func() recoveryOutcome {
				_, err := fixture.budget.service.Settle(context.Background(), *settleRecovery(fixture, revision, "settle-2", "receipt-2", true, true, 9))
				return recoveryOutcome{err: err}
			},
		)
		if !errors.Is(conflicting[0].err, ErrBudgetConflict) {
			t.Fatalf("conflicting receipt = %v, want ErrBudgetConflict", conflicting[0].err)
		}
		assertBudgetRootTotals(t, fixture.budget, fixture.rootID, 0, 8)
		assertRecoveryConservation(t, fixture.budget.pool, fixture.command.WorkspaceID)
	})
}

func TestBudgetRecoveryReconfiguredRolloverKeepsRootLiabilityAndUnknownSlot(t *testing.T) {
	t.Run("R5g_rollover_shrink_and_cap_reduction_after_restart_cannot_free_unknown_prior_slot", func(t *testing.T) {
		fixture := newRecoveryFixture(t)
		fixture.command.BudgetPolicy.SlotLimit = 1
		first := admitRecovery(t, fixture) // termination stays unknown
		total := first.Reservation.TotalCapMicroUsd
		fixture.budget.clock.Set(budgetTestTime(13, 10))
		next := nextRecoveryCommand(t, fixture, fixture.command.BudgetPolicy.Resource)
		next.BudgetPolicy.WindowStart, next.BudgetPolicy.WindowEnd = budgetTestTime(13, 0), budgetTestTime(13, 30) // rollover + shrink
		next.BudgetPolicy.SlotLimit = 1
		response := runRecoveryProcess(t, recoveryRequest{Action: "admit", Clock: fixture.budget.clock.Now(), Admit: &next})
		requireRecoveryError(t, response, ErrConcurrencyLimit)
		// Free the slot only in the test's accounting view by raising the limit,
		// then prove the root liability alone still blocks a reduced-cap retry.
		next.BudgetPolicy.SlotLimit = 4
		next.BudgetPolicy.RootCapMicroUSD = total + next.BudgetPolicy.MaxAttemptCostMicroUSD*3 - 1
		response = runRecoveryProcess(t, recoveryRequest{Action: "admit", Clock: fixture.budget.clock.Now(), Admit: &next})
		requireRecoveryError(t, response, ErrBudgetLimit)
		assertBudgetRootTotals(t, fixture.budget, fixture.rootID, total, 0)
		assertNoAdmissionArtifactsFor(t, fixture, next)
		assertRecoveryConservation(t, fixture.budget.pool, fixture.command.WorkspaceID)
	})
}

func assertNoAdmissionArtifactsFor(t *testing.T, fixture *admissionTestFixture, command AdmissionCommand) {
	t.Helper()
	for _, table := range []string{"governance_budget_reservation", "governance_budget_journal", "governance_budget_outbox", "governance_concurrency_hold"} {
		if got := budgetCount(t, fixture.budget.pool, "SELECT count(*) FROM "+table+" WHERE workspace_id = $1 AND reservation_id = $2", command.WorkspaceID, command.ReservationID); got != 0 {
			t.Fatalf("rejected admission left %d rows in %s", got, table)
		}
	}
}

var _ pgtype.UUID
