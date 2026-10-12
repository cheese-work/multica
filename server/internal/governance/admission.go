package governance

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/governance/caselifecycle"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type AdmissionService struct {
	database  *pgxpool.Pool
	budget    *BudgetService
	lifecycle *caselifecycle.Service
}

type AdmissionBudgetPolicy struct {
	Resource               string
	WindowStart            time.Time
	WindowEnd              time.Time
	RootCapMicroUSD        int64
	WindowCapMicroUSD      int64
	MaxAttemptCostMicroUSD int64
	RetryAllowance         int64
	RetryPolicyBounded     bool
	SlotLimit              int64
}

type AdmissionCommand struct {
	WorkspaceID          pgtype.UUID
	CaseID               pgtype.UUID
	ControlEpoch         int64
	ExpectedState        caselifecycle.CaseState
	ExpectedRevision     int64
	LeaseToken           pgtype.UUID
	ExpectedAttemptID    pgtype.UUID
	ExpectedAttemptFence pgtype.UUID
	CauseEventKey        string
	AttemptID            pgtype.UUID
	AttemptFence         pgtype.UUID
	CandidateID          pgtype.UUID
	ObligationID         pgtype.UUID
	InputDigest          string
	DeadlineAt           time.Time
	ReservationID        pgtype.UUID
	BudgetPolicy         AdmissionBudgetPolicy
}

type AdmissionResult struct {
	Case          db.GovernanceCase
	Attempt       db.GovernanceAttempt
	Reservation   db.GovernanceBudgetReservation
	OutboxEventID pgtype.UUID
	Duplicate     bool
}

func NewAdmissionService(database *pgxpool.Pool, clock BudgetClock) (*AdmissionService, error) {
	if database == nil || clock == nil {
		return nil, errors.New("governance admission service requires a database and clock")
	}
	budget, err := NewBudgetService(database, clock)
	if err != nil {
		return nil, err
	}
	lifecycle, err := caselifecycle.NewService(database, clock)
	if err != nil {
		return nil, err
	}
	return &AdmissionService{database: database, budget: budget, lifecycle: lifecycle}, nil
}

func (service *AdmissionService) Admit(ctx context.Context, command AdmissionCommand) (AdmissionResult, error) {
	if err := validateAdmissionCommand(command); err != nil {
		return AdmissionResult{}, err
	}
	tx, err := service.database.Begin(ctx)
	if err != nil {
		return AdmissionResult{}, fmt.Errorf("begin governance case and budget admission: %w", err)
	}
	defer tx.Rollback(ctx)
	guards, err := LockConcurrencyResources(ctx, tx, command.WorkspaceID, command.BudgetPolicy.Resource)
	if err != nil {
		return AdmissionResult{}, err
	}
	caseAdmission, err := service.lifecycle.AdmitAttemptInTransaction(ctx, tx, caselifecycle.AttemptAdmissionCommand{
		WorkspaceID:          command.WorkspaceID,
		CaseID:               command.CaseID,
		ControlEpoch:         command.ControlEpoch,
		ExpectedState:        command.ExpectedState,
		ExpectedRevision:     command.ExpectedRevision,
		LeaseToken:           command.LeaseToken,
		ExpectedAttemptID:    command.ExpectedAttemptID,
		ExpectedAttemptFence: command.ExpectedAttemptFence,
		CauseEventKey:        command.CauseEventKey,
		AttemptID:            command.AttemptID,
		AttemptFence:         command.AttemptFence,
		CandidateID:          command.CandidateID,
		ObligationID:         command.ObligationID,
		InputDigest:          command.InputDigest,
		DeadlineAt:           command.DeadlineAt,
	})
	if err != nil {
		return AdmissionResult{}, err
	}
	policy := command.BudgetPolicy
	reservation, err := service.budget.reserveInTransaction(ctx, tx, guards, BudgetReserveCommand{
		WorkspaceID:            command.WorkspaceID,
		ReservationID:          command.ReservationID,
		BudgetRootID:           caseAdmission.Case.BudgetRootID,
		CaseID:                 command.CaseID,
		AttemptID:              caseAdmission.Attempt.ID,
		ObligationID:           caseAdmission.Attempt.ObligationID,
		Resource:               policy.Resource,
		ControlEpoch:           command.ControlEpoch,
		WindowStart:            policy.WindowStart,
		WindowEnd:              policy.WindowEnd,
		RootCapMicroUSD:        policy.RootCapMicroUSD,
		WindowCapMicroUSD:      policy.WindowCapMicroUSD,
		MaxAttemptCostMicroUSD: policy.MaxAttemptCostMicroUSD,
		RetryAllowance:         policy.RetryAllowance,
		RetryPolicyBounded:     policy.RetryPolicyBounded,
		SlotLimit:              policy.SlotLimit,
		EventKey:               command.CauseEventKey,
	}, admissionRequestDigest(command))
	if err != nil {
		return AdmissionResult{}, err
	}
	if caseAdmission.Duplicate != reservation.Duplicate || !reservation.OutboxEventID.Valid {
		return AdmissionResult{}, ErrBudgetConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return AdmissionResult{}, fmt.Errorf("commit governance case and budget admission: %w", err)
	}
	return AdmissionResult{
		Case:          caseAdmission.Case,
		Attempt:       caseAdmission.Attempt,
		Reservation:   reservation.Reservation,
		OutboxEventID: reservation.OutboxEventID,
		Duplicate:     caseAdmission.Duplicate,
	}, nil
}

func validateAdmissionCommand(command AdmissionCommand) error {
	policy := command.BudgetPolicy
	if !validConcurrencyUUID(command.WorkspaceID) || !validConcurrencyUUID(command.CaseID) || command.ControlEpoch < 1 ||
		command.ExpectedRevision < 0 || !validConcurrencyUUID(command.LeaseToken) ||
		!validConcurrencyUUID(command.ExpectedAttemptID) || !validConcurrencyUUID(command.ExpectedAttemptFence) ||
		!validBudgetKey(command.CauseEventKey) || !validConcurrencyUUID(command.AttemptID) ||
		!validConcurrencyUUID(command.AttemptFence) || !validConcurrencyUUID(command.CandidateID) ||
		!validConcurrencyUUID(command.ObligationID) || !validConcurrencyUUID(command.ReservationID) ||
		strings.TrimSpace(command.InputDigest) == "" || len(command.InputDigest) > 256 ||
		policy.Resource == "" || strings.TrimSpace(policy.Resource) != policy.Resource || len(policy.Resource) > 128 ||
		policy.RootCapMicroUSD <= 0 || policy.WindowCapMicroUSD <= 0 || policy.MaxAttemptCostMicroUSD <= 0 ||
		policy.RetryAllowance < 0 || !policy.RetryPolicyBounded || policy.SlotLimit < 0 ||
		!policy.WindowStart.Before(policy.WindowEnd) || command.DeadlineAt.IsZero() {
		if !command.BudgetPolicy.RetryPolicyBounded {
			return ErrBudgetRetryUnbounded
		}
		return ErrBudgetInput
	}
	if !validCaseAdmissionState(command.ExpectedState) {
		return caselifecycle.ErrInvalidTransition
	}
	return nil
}

func validCaseAdmissionState(state caselifecycle.CaseState) bool {
	return state == caselifecycle.CaseAgentEscalation || state == caselifecycle.CaseNextAttempt
}

func admissionRequestDigest(command AdmissionCommand) string {
	policy := command.BudgetPolicy
	return hashBudgetValue(struct {
		WorkspaceID          string                  `json:"workspace_id"`
		CaseID               string                  `json:"case_id"`
		ControlEpoch         int64                   `json:"control_epoch"`
		ExpectedState        caselifecycle.CaseState `json:"expected_state"`
		ExpectedRevision     int64                   `json:"expected_revision"`
		LeaseToken           string                  `json:"lease_token"`
		ExpectedAttemptID    string                  `json:"expected_attempt_id"`
		ExpectedAttemptFence string                  `json:"expected_attempt_fence"`
		CauseEventKey        string                  `json:"cause_event_key"`
		AttemptID            string                  `json:"attempt_id"`
		AttemptFence         string                  `json:"attempt_fence"`
		CandidateID          string                  `json:"candidate_id"`
		ObligationID         string                  `json:"obligation_id"`
		InputDigest          string                  `json:"input_digest"`
		DeadlineAt           time.Time               `json:"deadline_at"`
		ReservationID        string                  `json:"reservation_id"`
		Resource             string                  `json:"resource"`
		WindowStart          time.Time               `json:"window_start"`
		WindowEnd            time.Time               `json:"window_end"`
		RootCapMicroUSD      int64                   `json:"root_cap_micro_usd"`
		WindowCapMicroUSD    int64                   `json:"window_cap_micro_usd"`
		AttemptCostMicroUSD  int64                   `json:"attempt_cost_micro_usd"`
		RetryAllowance       int64                   `json:"retry_allowance"`
		RetryBounded         bool                    `json:"retry_bounded"`
		SlotLimit            int64                   `json:"slot_limit"`
	}{
		WorkspaceID:          budgetUUIDString(command.WorkspaceID),
		CaseID:               budgetUUIDString(command.CaseID),
		ControlEpoch:         command.ControlEpoch,
		ExpectedState:        command.ExpectedState,
		ExpectedRevision:     command.ExpectedRevision,
		LeaseToken:           budgetUUIDString(command.LeaseToken),
		ExpectedAttemptID:    budgetUUIDString(command.ExpectedAttemptID),
		ExpectedAttemptFence: budgetUUIDString(command.ExpectedAttemptFence),
		CauseEventKey:        command.CauseEventKey,
		AttemptID:            budgetUUIDString(command.AttemptID),
		AttemptFence:         budgetUUIDString(command.AttemptFence),
		CandidateID:          budgetUUIDString(command.CandidateID),
		ObligationID:         budgetUUIDString(command.ObligationID),
		InputDigest:          command.InputDigest,
		DeadlineAt:           command.DeadlineAt.UTC(),
		ReservationID:        budgetUUIDString(command.ReservationID),
		Resource:             policy.Resource,
		WindowStart:          policy.WindowStart.UTC(),
		WindowEnd:            policy.WindowEnd.UTC(),
		RootCapMicroUSD:      policy.RootCapMicroUSD,
		WindowCapMicroUSD:    policy.WindowCapMicroUSD,
		AttemptCostMicroUSD:  policy.MaxAttemptCostMicroUSD,
		RetryAllowance:       policy.RetryAllowance,
		RetryBounded:         policy.RetryPolicyBounded,
		SlotLimit:            policy.SlotLimit,
	})
}
