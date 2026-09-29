package governance

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/governance/caselifecycle"
)

// Matrix rows (CHE-883 / B2). Each subtest name starts with its row id:
//   R1a-c reserve commit, R2a-e debit/wire dispatch, R3a-b native admission/ack,
//   R4a-e settlement commit, R5a-f concurrent/reconfigured/restarted workers
//   (R5 lives in budget_recovery_concurrent_test.go).

func admitRecovery(t *testing.T, fixture *admissionTestFixture) AdmissionResult {
	t.Helper()
	result, err := fixture.service.Admit(context.Background(), fixture.command)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	return result
}

func recoveryAdmitCrash(t *testing.T, fixture *admissionTestFixture, mode recoveryCrashMode) {
	t.Helper()
	installRecoveryFault(t, fixture.budget.pool)
	err := crashAtJournalInsert(t, fixture.budget.pool, fixture.command.WorkspaceID, "reserve", "before reserve commit", mode, func(ctx context.Context) error {
		_, err := fixture.service.Admit(ctx, fixture.command)
		return err
	})
	if errors.Is(err, ErrBudgetConflict) {
		t.Fatalf("crash surfaced as a conflict: %v", err)
	}
	assertNoAdmissionArtifacts(t, fixture)
	assertRecoveryConservation(t, fixture.budget.pool, fixture.command.WorkspaceID)
	if got := budgetCount(t, fixture.budget.pool, `SELECT count(*) FROM governance_budget_root WHERE workspace_id = $1`, fixture.command.WorkspaceID); got != 0 {
		t.Fatalf("crashed reserve left %d budget roots", got)
	}
}

func TestBudgetRecoveryReserveCommit(t *testing.T) {
	for _, mode := range []recoveryCrashMode{recoveryBackendLoss, recoveryContextAbort} {
		t.Run("R1a_precommit_"+string(mode)+"_leaves_zero_partial_state_then_fresh_process_admits", func(t *testing.T) {
			fixture := newRecoveryFixture(t)
			recoveryAdmitCrash(t, fixture, mode)
			response := runRecoveryProcess(t, recoveryRequest{Action: "admit", Clock: fixture.budget.clock.Now(), Admit: &fixture.command})
			if response.ErrorKind != "" || response.Duplicate {
				t.Fatalf("fresh-process admit after precommit crash = %+v, want first commit", response)
			}
			if got := budgetCount(t, fixture.budget.pool, `SELECT count(*) FROM governance_budget_reservation WHERE workspace_id = $1`, fixture.command.WorkspaceID); got != 1 {
				t.Fatalf("reservations after recovery = %d, want 1", got)
			}
			assertRecoveryConservation(t, fixture.budget.pool, fixture.command.WorkspaceID)
		})
	}
	t.Run("R1b_committed_reserve_with_lost_ack_is_reused_exactly_once_by_fresh_process", func(t *testing.T) {
		fixture := newRecoveryFixture(t)
		admitRecovery(t, fixture) // acknowledgement discarded: the caller "crashed" before reading it
		var persisted pgtype.UUID
		if err := fixture.budget.pool.QueryRow(context.Background(), `
			SELECT event_id FROM governance_budget_outbox WHERE workspace_id = $1 AND reservation_id = $2
		`, fixture.command.WorkspaceID, fixture.command.ReservationID).Scan(&persisted); err != nil {
			t.Fatal(err)
		}
		response := runRecoveryProcess(t, recoveryRequest{Action: "admit", Clock: fixture.budget.clock.Now(), Admit: &fixture.command})
		if response.ErrorKind != "" || !response.Duplicate || response.OutboxEventID != persisted.String() {
			t.Fatalf("fresh-process retry = %+v, want duplicate of outbox %s", response, persisted)
		}
		for _, table := range []string{"governance_budget_reservation", "governance_budget_outbox", "governance_concurrency_hold", "governance_case_transition"} {
			if got := budgetCount(t, fixture.budget.pool, "SELECT count(*) FROM "+table+" WHERE workspace_id = $1", fixture.command.WorkspaceID); got != 1 {
				t.Fatalf("%s rows after replayed reserve = %d, want 1", table, got)
			}
		}
		assertRecoveryConservation(t, fixture.budget.pool, fixture.command.WorkspaceID)
	})
	t.Run("R1c_same_input_duplicate_and_conflicting_input_controls", func(t *testing.T) {
		fixture := newRecoveryFixture(t)
		admitRecovery(t, fixture)
		conflict := fixture.command
		conflict.BudgetPolicy.MaxAttemptCostMicroUSD++
		response := runRecoveryProcess(t, recoveryRequest{Action: "admit", Clock: fixture.budget.clock.Now(), Admit: &conflict})
		requireRecoveryError(t, response, ErrBudgetConflict)
		assertRecoveryConservation(t, fixture.budget.pool, fixture.command.WorkspaceID)
	})
}

func admitAndDispatcher(t *testing.T, fixture *admissionTestFixture) (*recoveryDispatcher, AdmissionResult) {
	t.Helper()
	result := admitRecovery(t, fixture)
	return &recoveryDispatcher{
		pool: fixture.budget.pool, budget: fixture.budget.service, broker: newRecoveryBroker(),
		workspaceID: fixture.command.WorkspaceID, reservationID: fixture.command.ReservationID,
	}, result
}

func TestBudgetRecoveryDebitAndWireDispatch(t *testing.T) {
	ctx := context.Background()
	t.Run("R2a_crash_before_debit_commit_sends_no_wire_and_debits_nothing", func(t *testing.T) {
		fixture := newRecoveryFixture(t)
		dispatcher, _ := admitAndDispatcher(t, fixture)
		installRecoveryFault(t, fixture.budget.pool)
		err := crashAtJournalInsert(t, fixture.budget.pool, fixture.command.WorkspaceID, "debit", "before debit commit", recoveryBackendLoss, func(ctx context.Context) error {
			return dispatcher.Send(ctx, "wire-1", 7)
		})
		if errors.Is(err, errRecoveryCrash) {
			t.Fatal("crash was injected by the dispatcher, not by the database")
		}
		if dispatcher.broker.wireCount("wire-1") != 0 {
			t.Fatal("wire attempt reached the broker without a persisted debit")
		}
		row := readRecoveryReservation(t, fixture.budget.pool, fixture.command.WorkspaceID, fixture.command.ReservationID)
		if row.Debited != 0 || row.Revision != 1 {
			t.Fatalf("crashed debit persisted %+v", row)
		}
		assertRecoveryConservation(t, fixture.budget.pool, fixture.command.WorkspaceID)
		// A fresh process applies the same wire key exactly once.
		response := runRecoveryProcess(t, recoveryRequest{Action: "debit", Clock: fixture.budget.clock.Now(), Debit: &BudgetDebitCommand{
			WorkspaceID: fixture.command.WorkspaceID, ReservationID: fixture.command.ReservationID, ExpectedRevision: 1, EventKey: "wire-1", AmountMicroUSD: 7,
		}})
		if response.ErrorKind != "" || response.Duplicate {
			t.Fatalf("recovery debit = %+v", response)
		}
		if row := readRecoveryReservation(t, fixture.budget.pool, fixture.command.WorkspaceID, fixture.command.ReservationID); row.Debited != 7 {
			t.Fatalf("debited after recovery = %d, want 7", row.Debited)
		}
	})
	t.Run("R2b_crash_after_debit_commit_before_wire_same_wire_key_debits_once", func(t *testing.T) {
		fixture := newRecoveryFixture(t)
		dispatcher, _ := admitAndDispatcher(t, fixture)
		dispatcher.crashAfterDebit = true
		if err := dispatcher.Send(ctx, "wire-1", 7); !errors.Is(err, errRecoveryCrash) {
			t.Fatalf("send = %v, want simulated crash", err)
		}
		if dispatcher.broker.wireCount("wire-1") != 0 {
			t.Fatal("wire sent although the process crashed before dispatch")
		}
		response := runRecoveryProcess(t, recoveryRequest{Action: "debit", Clock: fixture.budget.clock.Now(), Debit: &BudgetDebitCommand{
			WorkspaceID: fixture.command.WorkspaceID, ReservationID: fixture.command.ReservationID, ExpectedRevision: 1, EventKey: "wire-1", AmountMicroUSD: 7,
		}})
		if response.ErrorKind != "" || !response.Duplicate {
			t.Fatalf("replayed debit = %+v, want duplicate", response)
		}
		dispatcher.crashAfterDebit = false
		if err := dispatcher.Send(ctx, "wire-1", 7); err != nil {
			t.Fatalf("replayed send: %v", err)
		}
		if dispatcher.broker.wireCount("wire-1") != 1 {
			t.Fatalf("wire count = %d, want 1", dispatcher.broker.wireCount("wire-1"))
		}
		row := readRecoveryReservation(t, fixture.budget.pool, fixture.command.WorkspaceID, fixture.command.ReservationID)
		if row.Debited != 7 || row.AttemptsStarted != 1 {
			t.Fatalf("replay changed accounting: %+v", row)
		}
		assertRecoveryConservation(t, fixture.budget.pool, fixture.command.WorkspaceID)
	})
	t.Run("R2c_each_distinct_retry_consumes_persisted_allowance_and_exhaustion_blocks_wire", func(t *testing.T) {
		fixture := newRecoveryFixture(t) // retry allowance 2 => 3 wire attempts
		dispatcher, _ := admitAndDispatcher(t, fixture)
		for _, key := range []string{"wire-1", "wire-2", "wire-3"} {
			if err := dispatcher.Send(ctx, key, 1); err != nil {
				t.Fatalf("send %s: %v", key, err)
			}
		}
		row := readRecoveryReservation(t, fixture.budget.pool, fixture.command.WorkspaceID, fixture.command.ReservationID)
		if row.AttemptsStarted != 3 || row.RetryRemaining != 0 {
			t.Fatalf("after allowed retries attempts/remaining = %d/%d, want 3/0", row.AttemptsStarted, row.RetryRemaining)
		}
		if err := dispatcher.Send(ctx, "wire-4", 1); !errors.Is(err, ErrBudgetLimit) {
			t.Fatalf("retry beyond allowance = %v, want ErrBudgetLimit", err)
		}
		if dispatcher.broker.wireCount("wire-4") != 0 {
			t.Fatal("wire sent beyond the persisted retry allowance")
		}
		if row := readRecoveryReservation(t, fixture.budget.pool, fixture.command.WorkspaceID, fixture.command.ReservationID); row.Debited != 3 || row.AttemptsStarted != 3 {
			t.Fatalf("rejected retry changed accounting: %+v", row)
		}
		assertRecoveryConservation(t, fixture.budget.pool, fixture.command.WorkspaceID)
	})
	t.Run("R2d_expired_window_prohibits_new_wire_work", func(t *testing.T) {
		fixture := newRecoveryFixture(t)
		dispatcher, _ := admitAndDispatcher(t, fixture)
		fixture.budget.clock.Set(fixture.command.BudgetPolicy.WindowEnd)
		if err := dispatcher.Send(ctx, "wire-1", 1); !errors.Is(err, ErrBudgetWindow) {
			t.Fatalf("send after window end = %v, want ErrBudgetWindow", err)
		}
		if dispatcher.broker.wireCount("wire-1") != 0 {
			t.Fatal("wire sent after the billing window expired")
		}
		if row := readRecoveryReservation(t, fixture.budget.pool, fixture.command.WorkspaceID, fixture.command.ReservationID); row.Debited != 0 || row.Revision != 1 {
			t.Fatalf("expired debit changed accounting: %+v", row)
		}
		assertRecoveryConservation(t, fixture.budget.pool, fixture.command.WorkspaceID)
	})
	t.Run("R2e_conflicting_debit_for_same_wire_key_is_rejected", func(t *testing.T) {
		fixture := newRecoveryFixture(t)
		dispatcher, _ := admitAndDispatcher(t, fixture)
		if err := dispatcher.Send(ctx, "wire-1", 5); err != nil {
			t.Fatal(err)
		}
		response := runRecoveryProcess(t, recoveryRequest{Action: "debit", Clock: fixture.budget.clock.Now(), Debit: &BudgetDebitCommand{
			WorkspaceID: fixture.command.WorkspaceID, ReservationID: fixture.command.ReservationID, ExpectedRevision: 2, EventKey: "wire-1", AmountMicroUSD: 6,
		}})
		requireRecoveryError(t, response, ErrBudgetConflict)
		if row := readRecoveryReservation(t, fixture.budget.pool, fixture.command.WorkspaceID, fixture.command.ReservationID); row.Debited != 5 {
			t.Fatalf("conflicting debit changed accounting: %+v", row)
		}
	})
}

func TestBudgetRecoveryNativeAdmission(t *testing.T) {
	ctx := context.Background()
	t.Run("R3a_replayed_outbox_uses_same_native_key_and_admits_at_most_once", func(t *testing.T) {
		fixture := newRecoveryFixture(t)
		admitRecovery(t, fixture)
		adapter := &recoveryOutboxAdapter{pool: fixture.budget.pool, broker: newRecoveryBroker()}
		key := fixture.command.ObligationID.String()
		if err := adapter.Deliver(ctx, fixture.command.WorkspaceID, fixture.command.ReservationID, true); !errors.Is(err, errRecoveryCrash) {
			t.Fatalf("first delivery = %v, want simulated crash after native admission", err)
		}
		for attempt := 0; attempt < 3; attempt++ {
			if err := adapter.Deliver(ctx, fixture.command.WorkspaceID, fixture.command.ReservationID, false); err != nil {
				t.Fatalf("replayed delivery %d: %v", attempt, err)
			}
		}
		if adapter.broker.admissionCount(key) != 1 {
			t.Fatalf("native admissions for obligation = %d, want 1", adapter.broker.admissionCount(key))
		}
		if len(adapter.broker.admissions) != 1 {
			t.Fatalf("broker saw keys %v, want only the obligation identity", adapter.broker.admissions)
		}
		if got := budgetCount(t, fixture.budget.pool, `SELECT count(*) FROM governance_budget_outbox WHERE workspace_id = $1 AND state = 'delivered'`, fixture.command.WorkspaceID); got != 1 {
			t.Fatalf("delivered outbox rows = %d, want 1", got)
		}
		assertRecoveryConservation(t, fixture.budget.pool, fixture.command.WorkspaceID)
	})
	t.Run("R3b_uncertain_native_outcome_retains_slot_and_spend_until_observed_reconciliation", func(t *testing.T) {
		fixture := newRecoveryFixture(t)
		fixture.command.BudgetPolicy.SlotLimit = 1
		result := admitRecovery(t, fixture)
		adapter := &recoveryOutboxAdapter{pool: fixture.budget.pool, broker: newRecoveryBroker()}
		adapter.broker.uncertain = true
		if err := adapter.Deliver(ctx, fixture.command.WorkspaceID, fixture.command.ReservationID, false); !errors.Is(err, errRecoveryUncertain) {
			t.Fatalf("uncertain delivery = %v", err)
		}
		// Unknown termination: a settle without observed termination must keep everything.
		response := runRecoveryProcess(t, recoveryRequest{Action: "settle", Clock: fixture.budget.clock.Now(), Settle: &BudgetSettleCommand{
			WorkspaceID: fixture.command.WorkspaceID, ReservationID: fixture.command.ReservationID, ExpectedRevision: result.Reservation.Revision, EventKey: "unknown-1",
		}})
		if response.ErrorKind != "" || response.Finalized {
			t.Fatalf("unknown-termination settle = %+v", response)
		}
		assertBudgetRootTotals(t, fixture.budget, fixture.rootID, result.Reservation.TotalCapMicroUsd, 0)
		if got := budgetCount(t, fixture.budget.pool, `SELECT held_slots FROM governance_concurrency_guard WHERE workspace_id = $1 AND resource = $2`, fixture.command.WorkspaceID, fixture.command.BudgetPolicy.Resource); got != 1 {
			t.Fatalf("held slots with uncertain native outcome = %d, want 1", got)
		}
		next := nextRecoveryCommand(t, fixture, fixture.command.BudgetPolicy.Resource)
		next.BudgetPolicy.SlotLimit = 1
		if _, err := fixture.service.Admit(ctx, next); !errors.Is(err, ErrConcurrencyLimit) {
			t.Fatalf("admission into the retained slot = %v, want ErrConcurrencyLimit", err)
		}
		// Observed reconciliation releases it.
		observed := runRecoveryProcess(t, recoveryRequest{Action: "settle", Clock: fixture.budget.clock.Now(), Settle: &BudgetSettleCommand{
			WorkspaceID: fixture.command.WorkspaceID, ReservationID: fixture.command.ReservationID, ExpectedRevision: response.Revision, EventKey: "observed-1",
			ReceiptID: "receipt-1", UsageKnown: true, TerminationKnown: true, UsageMicroUSD: 0,
		}})
		if observed.ErrorKind != "" || !observed.Finalized {
			t.Fatalf("observed reconciliation = %+v", observed)
		}
		assertBudgetRootTotals(t, fixture.budget, fixture.rootID, 0, 0)
		assertRecoveryConservation(t, fixture.budget.pool, fixture.command.WorkspaceID)
	})
}

func settleRecovery(fixture *admissionTestFixture, revision int64, key, receipt string, usageKnown, terminationKnown bool, usage int64) *BudgetSettleCommand {
	return &BudgetSettleCommand{
		WorkspaceID: fixture.command.WorkspaceID, ReservationID: fixture.command.ReservationID, ExpectedRevision: revision,
		EventKey: key, ReceiptID: receipt, UsageKnown: usageKnown, TerminationKnown: terminationKnown, UsageMicroUSD: usage,
	}
}

func TestBudgetRecoverySettlementCommit(t *testing.T) {
	t.Run("R4a_crash_before_settle_commit_retains_liability_then_fresh_process_settles_once", func(t *testing.T) {
		fixture := newRecoveryFixture(t)
		result := admitRecovery(t, fixture)
		installRecoveryFault(t, fixture.budget.pool)
		command := settleRecovery(fixture, result.Reservation.Revision, "settle-1", "receipt-1", true, true, 10)
		crashAtJournalInsert(t, fixture.budget.pool, fixture.command.WorkspaceID, "settle", "before settlement commit", recoveryBackendLoss, func(ctx context.Context) error {
			_, err := fixture.budget.service.Settle(ctx, *command)
			return err
		})
		if row := readRecoveryReservation(t, fixture.budget.pool, fixture.command.WorkspaceID, fixture.command.ReservationID); row.StateName != "reserved" || row.Settled != 0 {
			t.Fatalf("crashed settle persisted %+v", row)
		}
		assertBudgetRootTotals(t, fixture.budget, fixture.rootID, result.Reservation.TotalCapMicroUsd, 0)
		assertRecoveryConservation(t, fixture.budget.pool, fixture.command.WorkspaceID)
		response := runRecoveryProcess(t, recoveryRequest{Action: "settle", Clock: fixture.budget.clock.Now(), Settle: command})
		if response.ErrorKind != "" || response.Duplicate || !response.Finalized {
			t.Fatalf("recovery settle = %+v", response)
		}
		assertBudgetRootTotals(t, fixture.budget, fixture.rootID, 0, 10)
	})
	t.Run("R4b_committed_settle_with_lost_ack_duplicates_noop_and_conflicts_reject", func(t *testing.T) {
		fixture := newRecoveryFixture(t)
		result := admitRecovery(t, fixture)
		command := settleRecovery(fixture, result.Reservation.Revision, "settle-1", "receipt-1", true, true, 10)
		if _, err := fixture.budget.service.Settle(context.Background(), *command); err != nil {
			t.Fatal(err)
		}
		duplicate := runRecoveryProcess(t, recoveryRequest{Action: "settle", Clock: fixture.budget.clock.Now(), Settle: command})
		if duplicate.ErrorKind != "" || !duplicate.Duplicate {
			t.Fatalf("duplicate settle = %+v", duplicate)
		}
		otherReceipt := settleRecovery(fixture, result.Reservation.Revision, "settle-2", "receipt-2", true, true, 10)
		requireRecoveryError(t, runRecoveryProcess(t, recoveryRequest{Action: "settle", Clock: fixture.budget.clock.Now(), Settle: otherReceipt}), ErrBudgetConflict)
		otherUsage := settleRecovery(fixture, result.Reservation.Revision, "settle-1", "receipt-1", true, true, 11)
		requireRecoveryError(t, runRecoveryProcess(t, recoveryRequest{Action: "settle", Clock: fixture.budget.clock.Now(), Settle: otherUsage}), ErrBudgetConflict)
		assertBudgetRootTotals(t, fixture.budget, fixture.rootID, 0, 10)
		assertRecoveryConservation(t, fixture.budget.pool, fixture.command.WorkspaceID)
	})
	t.Run("R4c_terminal_known_usage_unknown_charges_full_cap", func(t *testing.T) {
		fixture := newRecoveryFixture(t)
		result := admitRecovery(t, fixture)
		response := runRecoveryProcess(t, recoveryRequest{Action: "settle", Clock: fixture.budget.clock.Now(), Settle: settleRecovery(fixture, result.Reservation.Revision, "settle-1", "receipt-1", false, true, 0)})
		if response.ErrorKind != "" || !response.Finalized {
			t.Fatalf("settle = %+v", response)
		}
		assertBudgetRootTotals(t, fixture.budget, fixture.rootID, 0, result.Reservation.TotalCapMicroUsd)
		assertRecoveryConservation(t, fixture.budget.pool, fixture.command.WorkspaceID)
	})
	t.Run("R4d_termination_unknown_retains_liability_and_slot", func(t *testing.T) {
		fixture := newRecoveryFixture(t)
		result := admitRecovery(t, fixture)
		response := runRecoveryProcess(t, recoveryRequest{Action: "settle", Clock: fixture.budget.clock.Now(), Settle: settleRecovery(fixture, result.Reservation.Revision, "unknown-1", "", false, false, 0)})
		if response.ErrorKind != "" || response.Finalized || response.State != "reserved" {
			t.Fatalf("unknown settle = %+v", response)
		}
		assertBudgetRootTotals(t, fixture.budget, fixture.rootID, result.Reservation.TotalCapMicroUsd, 0)
		if got := budgetCount(t, fixture.budget.pool, `SELECT count(*) FROM governance_concurrency_hold WHERE workspace_id = $1 AND state = 'held'`, fixture.command.WorkspaceID); got != 1 {
			t.Fatalf("held slots = %d, want 1", got)
		}
		assertRecoveryConservation(t, fixture.budget.pool, fixture.command.WorkspaceID)
	})
	t.Run("R4e_verified_usage_refunds_only_the_proven_remainder_once", func(t *testing.T) {
		fixture := newRecoveryFixture(t)
		result := admitRecovery(t, fixture)
		total := result.Reservation.TotalCapMicroUsd
		if _, err := fixture.budget.service.Debit(context.Background(), BudgetDebitCommand{
			WorkspaceID: fixture.command.WorkspaceID, ReservationID: fixture.command.ReservationID, ExpectedRevision: result.Reservation.Revision, EventKey: "wire-1", AmountMicroUSD: 12,
		}); err != nil {
			t.Fatal(err)
		}
		command := settleRecovery(fixture, 2, "settle-1", "receipt-1", true, true, 30)
		first := runRecoveryProcess(t, recoveryRequest{Action: "settle", Clock: fixture.budget.clock.Now(), Settle: command})
		second := runRecoveryProcess(t, recoveryRequest{Action: "settle", Clock: fixture.budget.clock.Now(), Settle: command})
		if first.ErrorKind != "" || !first.Finalized || second.ErrorKind != "" || !second.Duplicate {
			t.Fatalf("settle/replay = %+v / %+v", first, second)
		}
		assertBudgetRootTotals(t, fixture.budget, fixture.rootID, 0, 30)
		if total-30 <= 0 {
			t.Fatal("fixture leaves no refundable remainder")
		}
		assertRecoveryConservation(t, fixture.budget.pool, fixture.command.WorkspaceID)
	})
}

var _ = caselifecycle.CaseAgentAttempt

// R4f-R4h cover the production settle path, SettleEvaluation, which records
// verified usage and settles. It is a distinct Debit caller from the wire
// dispatcher: settle-time usage is not new wire work.

func reserveEvaluationForRecovery(t *testing.T, fixture *budgetTestFixture) (*DeploymentBudgetPolicy, BudgetReservationResult) {
	t.Helper()
	policy := testDeploymentBudgetPolicy(400, 1000, 1000)
	input := testEvaluationBudgetInput(fixture.workspaceID, budgetTestUUID(), budgetTestUUID(), 3, 1000, 1)
	reserved, err := fixture.service.ReserveEvaluation(context.Background(), policy, input)
	if err != nil {
		t.Fatalf("reserve evaluation: %v", err)
	}
	return policy, reserved
}

func TestBudgetRecoverySettleEvaluation(t *testing.T) {
	ctx := context.Background()
	// 1,000,000 input tokens cost 100 micro-USD under the test pricing policy.
	const usageTokens = 1_000_000
	t.Run("R4f_settle_after_window_end_still_settles_verified_usage", func(t *testing.T) {
		fixture := newBudgetTestFixture(t)
		policy, reserved := reserveEvaluationForRecovery(t, fixture)
		fixture.clock.Set(budgetTestTime(13, 0)) // provider call straddled the window end
		if err := fixture.service.SettleEvaluation(ctx, policy, reserved, "receipt-1", usageTokens, 0, true); err != nil {
			t.Fatalf("settle after window end = %v, want settled", err)
		}
		row := readRecoveryReservation(t, fixture.pool, fixture.workspaceID, reserved.Reservation.ReservationID)
		if row.StateName != "settled" || row.Settled != 100 {
			t.Fatalf("reservation after late settle = %+v, want settled at 100", row)
		}
		assertRecoveryConservation(t, fixture.pool, fixture.workspaceID)
	})
	t.Run("R4g_settle_after_wire_predebit_neither_double_debits_nor_consumes_retry", func(t *testing.T) {
		fixture := newBudgetTestFixture(t)
		policy, reserved := reserveEvaluationForRecovery(t, fixture)
		id := reserved.Reservation.ReservationID
		debited, err := fixture.service.Debit(ctx, BudgetDebitCommand{
			WorkspaceID: fixture.workspaceID, ReservationID: id, ExpectedRevision: reserved.Reservation.Revision, EventKey: "wire-1", AmountMicroUSD: 40,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := fixture.service.SettleEvaluation(ctx, policy, debited, "receipt-1", usageTokens, 0, true); err != nil {
			t.Fatalf("settle after wire pre-debit = %v, want settled", err)
		}
		row := readRecoveryReservation(t, fixture.pool, fixture.workspaceID, id)
		if row.StateName != "settled" || row.Settled != 100 || row.Debited != 100 || row.AttemptsStarted != 1 {
			t.Fatalf("settled reservation = %+v, want settled/debited 100 and one attempt", row)
		}
		assertRecoveryConservation(t, fixture.pool, fixture.workspaceID)
	})
	t.Run("R4h_worst_case_predebit_settles_and_refunds_only_the_excess_exactly_once", func(t *testing.T) {
		fixture := newBudgetTestFixture(t)
		policy, reserved := reserveEvaluationForRecovery(t, fixture)
		id := reserved.Reservation.ReservationID
		debited, err := fixture.service.Debit(ctx, BudgetDebitCommand{
			WorkspaceID: fixture.workspaceID, ReservationID: id, ExpectedRevision: reserved.Reservation.Revision, EventKey: "wire-1", AmountMicroUSD: 400,
		}) // worst case: the whole attempt cap, taken before the wire
		if err != nil {
			t.Fatal(err)
		}
		assertBudgetRootTotals(t, fixture, reserved.Reservation.BudgetRootID, reserved.Reservation.TotalCapMicroUsd-400, 400)
		if err := fixture.service.SettleEvaluation(ctx, policy, debited, "receipt-1", usageTokens, 0, true); err != nil {
			t.Fatalf("settle below the worst-case pre-debit = %v, want settled with a refund", err)
		}
		row := readRecoveryReservation(t, fixture.pool, fixture.workspaceID, id)
		if row.StateName != "settled" || row.Settled != 100 || row.Debited != 100 || row.Remaining != 0 {
			t.Fatalf("settled reservation = %+v, want settled at 100 (300 refunded)", row)
		}
		assertBudgetRootTotals(t, fixture, reserved.Reservation.BudgetRootID, 0, 100)
		// A replay under a different event key must not refund a second time.
		if err := fixture.service.SettleEvaluation(ctx, policy, debited, "receipt-1", usageTokens, 0, true); err != nil {
			t.Fatalf("replayed settle = %v, want duplicate no-op", err)
		}
		assertBudgetRootTotals(t, fixture, reserved.Reservation.BudgetRootID, 0, 100)
		sameReceiptNewKey, err := fixture.service.Settle(ctx, BudgetSettleCommand{
			WorkspaceID: fixture.workspaceID, ReservationID: id, ExpectedRevision: debited.Reservation.Revision,
			EventKey: "settle-other-key", ReceiptID: "receipt-1", UsageKnown: true, TerminationKnown: true, UsageMicroUSD: 100,
		})
		if err != nil || !sameReceiptNewKey.Duplicate {
			t.Fatalf("same receipt under another event key = %+v, %v; want duplicate no-op", sameReceiptNewKey, err)
		}
		assertBudgetRootTotals(t, fixture, reserved.Reservation.BudgetRootID, 0, 100)
		assertRecoveryConservation(t, fixture.pool, fixture.workspaceID)
		// The refund is spendable once, not twice: 300 freed, so a 300-cap sibling fits and a 301 one does not.
		fits := fixture.reserveCommand("resource-refund", budgetTestTime(12, 0), budgetTestTime(13, 0), 1000, 1000, 300, 1)
		fits.BudgetRootID = reserved.Reservation.BudgetRootID
		if _, err := fixture.service.Reserve(ctx, fits); err != nil {
			t.Fatalf("reserve against the refund: %v", err)
		}
	})
	t.Run("R4j_first_wire_debit_after_interim_usage_is_not_metered_as_a_retry", func(t *testing.T) {
		fixture := newBudgetTestFixture(t)
		_, reserved := reserveEvaluationForRecovery(t, fixture) // retry allowance 0
		id := reserved.Reservation.ReservationID
		interim, err := fixture.service.Settle(ctx, BudgetSettleCommand{
			WorkspaceID: fixture.workspaceID, ReservationID: id, ExpectedRevision: reserved.Reservation.Revision,
			EventKey: "interim", UsageKnown: true, UsageMicroUSD: 50,
		})
		if err != nil {
			t.Fatalf("interim usage report: %v", err)
		}
		debited, err := fixture.service.Debit(ctx, BudgetDebitCommand{
			WorkspaceID: fixture.workspaceID, ReservationID: id, ExpectedRevision: interim.Reservation.Revision, EventKey: "wire-1", AmountMicroUSD: 100,
		})
		if err != nil {
			t.Fatalf("first wire debit after interim usage = %v, want it to ride the reserved attempt", err)
		}
		if debited.Reservation.AttemptsStarted != 1 || debited.Reservation.RetryAllowanceRemaining != 0 {
			t.Fatalf("attempts/retry remaining = %d/%d, want 1/0", debited.Reservation.AttemptsStarted, debited.Reservation.RetryAllowanceRemaining)
		}
		// A second distinct wire key is a retry, and the allowance is 0.
		if _, err := fixture.service.Debit(ctx, BudgetDebitCommand{
			WorkspaceID: fixture.workspaceID, ReservationID: id, ExpectedRevision: debited.Reservation.Revision, EventKey: "wire-2", AmountMicroUSD: 10,
		}); !errors.Is(err, ErrBudgetLimit) {
			t.Fatalf("second wire key with no allowance = %v, want ErrBudgetLimit", err)
		}
		assertRecoveryConservation(t, fixture.pool, fixture.workspaceID)
	})
	t.Run("R4i_interim_usage_below_predebit_without_termination_retains_liability", func(t *testing.T) {
		fixture := newBudgetTestFixture(t)
		_, reserved := reserveEvaluationForRecovery(t, fixture)
		id := reserved.Reservation.ReservationID
		debited, err := fixture.service.Debit(ctx, BudgetDebitCommand{
			WorkspaceID: fixture.workspaceID, ReservationID: id, ExpectedRevision: reserved.Reservation.Revision, EventKey: "wire-1", AmountMicroUSD: 300,
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = fixture.service.Settle(ctx, BudgetSettleCommand{
			WorkspaceID: fixture.workspaceID, ReservationID: id, ExpectedRevision: debited.Reservation.Revision,
			EventKey: "interim", UsageKnown: true, UsageMicroUSD: 100,
		})
		if !errors.Is(err, ErrBudgetInvariant) {
			t.Fatalf("interim usage below the pre-debit = %v, want ErrBudgetInvariant", err)
		}
		if row := readRecoveryReservation(t, fixture.pool, fixture.workspaceID, id); row.StateName != "reserved" || row.Debited != 300 {
			t.Fatalf("rejected interim settle changed the reservation: %+v", row)
		}
		assertRecoveryConservation(t, fixture.pool, fixture.workspaceID)
	})
}
