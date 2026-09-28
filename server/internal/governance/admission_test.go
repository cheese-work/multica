package governance

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/governance/caselifecycle"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type admissionTestFixture struct {
	budget  *budgetTestFixture
	service *AdmissionService
	command AdmissionCommand
}

func newAdmissionTestFixture(t *testing.T) *admissionTestFixture {
	t.Helper()
	budgetFixture := newBudgetTestFixture(t)
	queries := db.New(budgetFixture.pool)
	ctx := context.Background()
	caseID := budgetTestUUID()
	rootID := budgetTestUUID()
	caseRow, err := queries.InsertNextGovernanceCase(ctx, db.InsertNextGovernanceCaseParams{
		WorkspaceID:         budgetFixture.workspaceID,
		SubjectType:         "issue",
		SubjectID:           budgetTestUUID(),
		SubjectRevision:     1,
		RuleID:              budgetTestUUID(),
		MaterialFingerprint: "admission-" + caseID.String(),
		State:               string(caselifecycle.CaseAgentEscalation),
		AuthorityLineage:    []byte("[]"),
		TriggerAliases:      []byte("[]"),
		EvidenceDigest:      "evidence-digest",
		RuleRevision:        "rule-revision",
		ActivationRevision:  "activation-revision",
		ConfigRevision:      "config-revision",
		BudgetRootID:        rootID,
		FrozenStrategy:      []byte("[]"),
		AbsoluteDeadline:    pgtype.Timestamptz{Time: budgetFixture.clock.Now().Add(time.Hour), Valid: true},
		EvidenceEpoch:       1,
		ControlEpoch:        1,
	})
	if err != nil {
		t.Fatalf("insert admission case: %v", err)
	}
	currentFence := budgetTestUUID()
	currentAttempt, err := queries.InsertGovernanceAttempt(ctx, db.InsertGovernanceAttemptParams{
		WorkspaceID:  budgetFixture.workspaceID,
		CaseID:       caseRow.ID,
		Ordinal:      0,
		Kind:         "jev",
		InputDigest:  "current-input",
		AttemptFence: currentFence,
		DeadlineAt:   pgtype.Timestamptz{Time: budgetFixture.clock.Now().Add(20 * time.Minute), Valid: true},
		Confidence:   []byte("{}"),
		Result:       []byte("{}"),
		Usage:        []byte("{}"),
	})
	if err != nil {
		t.Fatalf("insert current admission attempt: %v", err)
	}
	if _, err := budgetFixture.pool.Exec(ctx, `
		UPDATE governance_case SET current_attempt_id = $3
		WHERE workspace_id = $1 AND id = $2
	`, budgetFixture.workspaceID, caseRow.ID, currentAttempt.ID); err != nil {
		t.Fatalf("bind current admission attempt: %v", err)
	}
	leaseToken := budgetTestUUID()
	lifecycle, err := caselifecycle.NewService(budgetFixture.pool, budgetFixture.clock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.ClaimLease(ctx, caselifecycle.LeaseCommand{
		WorkspaceID:      budgetFixture.workspaceID,
		CaseID:           caseRow.ID,
		ControlEpoch:     1,
		ExpectedState:    caselifecycle.CaseAgentEscalation,
		ExpectedRevision: caseRow.StateRevision,
		Token:            leaseToken,
		Duration:         time.Minute,
	}); err != nil {
		t.Fatalf("claim case admission lease: %v", err)
	}
	t.Cleanup(func() {
		for _, table := range []string{"governance_case_transition", "governance_attempt", "governance_case"} {
			if _, err := budgetFixture.pool.Exec(context.Background(), "DELETE FROM "+table+" WHERE workspace_id = $1", budgetFixture.workspaceID); err != nil {
				t.Errorf("cleanup %s: %v", table, err)
			}
		}
	})
	service, err := NewAdmissionService(budgetFixture.pool, budgetFixture.clock)
	if err != nil {
		t.Fatal(err)
	}
	command := AdmissionCommand{
		WorkspaceID:          budgetFixture.workspaceID,
		CaseID:               caseRow.ID,
		ControlEpoch:         1,
		ExpectedState:        caselifecycle.CaseAgentEscalation,
		ExpectedRevision:     caseRow.StateRevision,
		LeaseToken:           leaseToken,
		ExpectedAttemptID:    currentAttempt.ID,
		ExpectedAttemptFence: currentAttempt.AttemptFence,
		CauseEventKey:        "admit-" + caseID.String(),
		AttemptID:            budgetTestUUID(),
		AttemptFence:         budgetTestUUID(),
		CandidateID:          budgetTestUUID(),
		ObligationID:         budgetTestUUID(),
		InputDigest:          "candidate-input-digest",
		DeadlineAt:           budgetFixture.clock.Now().Add(5 * time.Minute),
		ReservationID:        budgetTestUUID(),
		BudgetPolicy: AdmissionBudgetPolicy{
			Resource:               "agent:fixed",
			WindowStart:            budgetTestTime(12, 0),
			WindowEnd:              budgetTestTime(13, 0),
			RootCapMicroUSD:        100,
			WindowCapMicroUSD:      100,
			MaxAttemptCostMicroUSD: 20,
			RetryPolicyBounded:     true,
			SlotLimit:              4,
		},
	}
	return &admissionTestFixture{budget: budgetFixture, service: service, command: command}
}

func TestBudgetCompositionCommitsCaseAttemptHoldBudgetJournalAndOutbox(t *testing.T) {
	fixture := newAdmissionTestFixture(t)
	result, err := fixture.service.Admit(context.Background(), fixture.command)
	if err != nil {
		t.Fatalf("admit attempt: %v", err)
	}
	if result.Duplicate {
		t.Fatal("first admission was marked duplicate")
	}
	if result.Case.State != string(caselifecycle.CaseAgentAttempt) || result.Case.StateRevision != fixture.command.ExpectedRevision+1 {
		t.Fatalf("admitted case = state %s revision %d", result.Case.State, result.Case.StateRevision)
	}
	if result.Case.CurrentAttemptID != result.Attempt.ID || result.Attempt.ID != fixture.command.AttemptID {
		t.Fatalf("case/attempt association = %s/%s, want %s", result.Case.CurrentAttemptID, result.Attempt.ID, fixture.command.AttemptID)
	}
	if result.Attempt.ObligationID != fixture.command.ObligationID {
		t.Fatalf("attempt obligation = %s, want %s", result.Attempt.ObligationID, fixture.command.ObligationID)
	}
	if result.Reservation.CaseID != fixture.command.CaseID || result.Reservation.AttemptID != result.Attempt.ID ||
		result.Reservation.ObligationID != fixture.command.ObligationID || result.Reservation.State != "reserved" {
		t.Fatalf("budget reservation is not bound to the admitted attempt: %+v", result.Reservation)
	}
	if !result.OutboxEventID.Valid {
		t.Fatal("committed admission has no persisted outbox event identity")
	}
	if got := budgetCount(t, fixture.budget.pool, `SELECT count(*) FROM governance_case_transition WHERE workspace_id = $1 AND case_id = $2 AND cause_event_key = $3`, fixture.command.WorkspaceID, fixture.command.CaseID, fixture.command.CauseEventKey); got != 1 {
		t.Fatalf("admission transition count = %d, want 1", got)
	}
	if got := budgetCount(t, fixture.budget.pool, `SELECT count(*) FROM governance_budget_reservation WHERE workspace_id = $1 AND reservation_id = $2`, fixture.command.WorkspaceID, fixture.command.ReservationID); got != 1 {
		t.Fatalf("budget reservation count = %d, want 1", got)
	}
	if got := budgetCount(t, fixture.budget.pool, `SELECT count(*) FROM governance_budget_journal WHERE workspace_id = $1 AND reservation_id = $2 AND event_key = $3`, fixture.command.WorkspaceID, fixture.command.ReservationID, fixture.command.CauseEventKey); got != 1 {
		t.Fatalf("budget journal count = %d, want 1", got)
	}
	if got := budgetCount(t, fixture.budget.pool, `SELECT count(*) FROM governance_budget_outbox WHERE workspace_id = $1 AND event_id = $2 AND state = 'pending'`, fixture.command.WorkspaceID, result.OutboxEventID); got != 1 {
		t.Fatalf("pending committed admission intent count = %d, want 1", got)
	}
	if got := budgetCount(t, fixture.budget.pool, `SELECT count(*) FROM governance_concurrency_hold WHERE workspace_id = $1 AND reservation_id = $2 AND state = 'held'`, fixture.command.WorkspaceID, fixture.command.ReservationID); got != 1 {
		t.Fatalf("stable concurrency hold count = %d, want 1", got)
	}
	if got := budgetCount(t, fixture.budget.pool, `SELECT held_slots FROM governance_concurrency_guard WHERE workspace_id = $1 AND resource = $2`, fixture.command.WorkspaceID, fixture.command.BudgetPolicy.Resource); got != 1 {
		t.Fatalf("held slot count = %d, want 1", got)
	}
	if got := budgetCount(t, fixture.budget.pool, `SELECT reserved_micro_usd FROM governance_budget_root WHERE workspace_id = $1 AND budget_root_id = $2`, fixture.command.WorkspaceID, result.Case.BudgetRootID); got != result.Reservation.TotalCapMicroUsd {
		t.Fatalf("root reserved amount = %d, want %d", got, result.Reservation.TotalCapMicroUsd)
	}
}

func TestBudgetCompositionIdempotentRetryAndConflictingPayload(t *testing.T) {
	fixture := newAdmissionTestFixture(t)
	first, err := fixture.service.Admit(context.Background(), fixture.command)
	if err != nil {
		t.Fatalf("first admission: %v", err)
	}
	duplicate, err := fixture.service.Admit(context.Background(), fixture.command)
	if err != nil || !duplicate.Duplicate || duplicate.Attempt.ID != first.Attempt.ID || duplicate.OutboxEventID != first.OutboxEventID {
		t.Fatalf("same admission retry = %+v, %v; want same committed result", duplicate, err)
	}
	conflict := fixture.command
	conflict.BudgetPolicy.WindowCapMicroUSD--
	if _, err := fixture.service.Admit(context.Background(), conflict); !errors.Is(err, ErrBudgetConflict) {
		t.Fatalf("conflicting idempotency payload error = %v, want ErrBudgetConflict", err)
	}
	if got := budgetCount(t, fixture.budget.pool, `SELECT count(*) FROM governance_attempt WHERE workspace_id = $1 AND case_id = $2 AND id = $3`, fixture.command.WorkspaceID, fixture.command.CaseID, fixture.command.AttemptID); got != 1 {
		t.Fatalf("conflicting retry changed attempt count to %d", got)
	}
}

func TestBudgetCompositionRejectsStaleFenceRevisionAndMissingPolicy(t *testing.T) {
	tests := []struct {
		name string
		edit func(*AdmissionCommand)
		want error
	}{
		{name: "stale control epoch", edit: func(command *AdmissionCommand) { command.ControlEpoch++ }, want: caselifecycle.ErrStaleControlEpoch},
		{name: "stale attempt fence", edit: func(command *AdmissionCommand) { command.ExpectedAttemptFence = budgetTestUUID() }, want: caselifecycle.ErrStaleFence},
		{name: "stale case revision", edit: func(command *AdmissionCommand) { command.ExpectedRevision++ }, want: caselifecycle.ErrStaleCase},
		{name: "missing budget policy", edit: func(command *AdmissionCommand) { command.BudgetPolicy.MaxAttemptCostMicroUSD = 0 }, want: ErrBudgetInput},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAdmissionTestFixture(t)
			command := fixture.command
			test.edit(&command)
			if _, err := fixture.service.Admit(context.Background(), command); !errors.Is(err, test.want) {
				t.Fatalf("admission error = %v, want %v", err, test.want)
			}
			assertNoAdmissionArtifacts(t, fixture)
		})
	}
}

func TestBudgetCompositionRejectsCrossWorkspaceCase(t *testing.T) {
	fixture := newAdmissionTestFixture(t)
	otherWorkspaceID := budgetTestUUID()
	if _, err := fixture.budget.pool.Exec(context.Background(), `
		INSERT INTO governance_workspace_config (workspace_id, control_epoch, settings)
		VALUES ($1, 1, '{"jev_governance_enabled":true,"rule_mode":"shadow"}')
	`, otherWorkspaceID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := fixture.budget.pool.Exec(context.Background(), `DELETE FROM governance_workspace_config WHERE workspace_id = $1`, otherWorkspaceID); err != nil {
			t.Errorf("cleanup cross-workspace control: %v", err)
		}
	})
	command := fixture.command
	command.WorkspaceID = otherWorkspaceID
	if _, err := fixture.service.Admit(context.Background(), command); !errors.Is(err, caselifecycle.ErrStaleCase) {
		t.Fatalf("cross-workspace case error = %v, want ErrStaleCase", err)
	}
	assertNoAdmissionArtifacts(t, fixture)
	if got := budgetCount(t, fixture.budget.pool, `SELECT count(*) FROM governance_concurrency_hold WHERE workspace_id = $1 AND reservation_id = $2`, otherWorkspaceID, command.ReservationID); got != 0 {
		t.Fatalf("cross-workspace hold count = %d, want 0", got)
	}
}

func TestBudgetCompositionSerializesConcurrentSameRequest(t *testing.T) {
	fixture := newAdmissionTestFixture(t)
	const workers = 2
	start := make(chan struct{})
	type outcome struct {
		result AdmissionResult
		err    error
	}
	results := make(chan outcome, workers)
	var wait sync.WaitGroup
	wait.Add(workers)
	for worker := 0; worker < workers; worker++ {
		go func() {
			defer wait.Done()
			<-start
			result, err := fixture.service.Admit(context.Background(), fixture.command)
			results <- outcome{result: result, err: err}
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	committed, duplicates := 0, 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent admission: %v", result.err)
		}
		if result.result.Duplicate {
			duplicates++
		} else {
			committed++
		}
	}
	if committed != 1 || duplicates != 1 {
		t.Fatalf("concurrent outcomes = committed:%d duplicates:%d, want one each", committed, duplicates)
	}
	if got := budgetCount(t, fixture.budget.pool, `SELECT count(*) FROM governance_budget_outbox WHERE workspace_id = $1 AND reservation_id = $2`, fixture.command.WorkspaceID, fixture.command.ReservationID); got != 1 {
		t.Fatalf("concurrent outbox count = %d, want 1", got)
	}
}

func assertNoAdmissionArtifacts(t *testing.T, fixture *admissionTestFixture) {
	t.Helper()
	for _, check := range []struct {
		name  string
		query string
		args  []any
	}{
		{"attempt", `SELECT count(*) FROM governance_attempt WHERE workspace_id = $1 AND case_id = $2 AND id = $3`, []any{fixture.command.WorkspaceID, fixture.command.CaseID, fixture.command.AttemptID}},
		{"transition", `SELECT count(*) FROM governance_case_transition WHERE workspace_id = $1 AND case_id = $2 AND cause_event_key = $3`, []any{fixture.command.WorkspaceID, fixture.command.CaseID, fixture.command.CauseEventKey}},
		{"reservation", `SELECT count(*) FROM governance_budget_reservation WHERE workspace_id = $1 AND reservation_id = $2`, []any{fixture.command.WorkspaceID, fixture.command.ReservationID}},
		{"journal", `SELECT count(*) FROM governance_budget_journal WHERE workspace_id = $1 AND reservation_id = $2`, []any{fixture.command.WorkspaceID, fixture.command.ReservationID}},
		{"outbox", `SELECT count(*) FROM governance_budget_outbox WHERE workspace_id = $1 AND reservation_id = $2`, []any{fixture.command.WorkspaceID, fixture.command.ReservationID}},
		{"hold", `SELECT count(*) FROM governance_concurrency_hold WHERE workspace_id = $1 AND reservation_id = $2`, []any{fixture.command.WorkspaceID, fixture.command.ReservationID}},
	} {
		t.Run(check.name, func(t *testing.T) {
			if got := budgetCount(t, fixture.budget.pool, check.query, check.args...); got != 0 {
				t.Fatalf("partial %s count = %d, want 0", check.name, got)
			}
		})
	}
	var state string
	if err := fixture.budget.pool.QueryRow(context.Background(), `SELECT state FROM governance_case WHERE workspace_id = $1 AND id = $2`, fixture.command.WorkspaceID, fixture.command.CaseID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != string(caselifecycle.CaseAgentEscalation) {
		t.Fatalf("failed admission changed case state to %s", state)
	}
}
