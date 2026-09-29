package caselifecycle

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type AttemptAdmissionCommand struct {
	WorkspaceID          pgtype.UUID
	CaseID               pgtype.UUID
	ControlEpoch         int64
	ExpectedState        CaseState
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
}

type AttemptAdmissionResult struct {
	Case       db.GovernanceCase
	Attempt    db.GovernanceAttempt
	Transition db.GovernanceCaseTransition
	Duplicate  bool
}

func (service *Service) AdmitAttemptInTransaction(ctx context.Context, tx pgx.Tx, command AttemptAdmissionCommand) (AttemptAdmissionResult, error) {
	if err := validateAttemptAdmissionCommand(command); err != nil {
		return AttemptAdmissionResult{}, err
	}
	if err := lockGovernanceControl(ctx, tx, command.WorkspaceID, command.ControlEpoch); err != nil {
		return AttemptAdmissionResult{}, err
	}
	queries := db.New(tx)
	caseRow, err := queries.LockGovernanceCaseForUpdate(ctx, db.LockGovernanceCaseForUpdateParams{
		WorkspaceID: command.WorkspaceID,
		ID:          command.CaseID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return AttemptAdmissionResult{}, ErrStaleCase
	}
	if err != nil {
		return AttemptAdmissionResult{}, err
	}
	if caseRow.ControlEpoch != command.ControlEpoch {
		return AttemptAdmissionResult{}, ErrStaleControlEpoch
	}
	transitionCommand := attemptAdmissionTransition(command)
	prior, err := queries.FindGovernanceCaseTransitionByCause(ctx, db.FindGovernanceCaseTransitionByCauseParams{
		WorkspaceID:   command.WorkspaceID,
		CaseID:        command.CaseID,
		CauseEventKey: command.CauseEventKey,
	})
	if err == nil {
		if !sameTransitionRequest(prior, transitionCommand) ||
			CaseState(caseRow.State) != CaseAgentAttempt ||
			caseRow.StateRevision != command.ExpectedRevision+1 ||
			caseRow.CurrentAttemptID != command.AttemptID {
			return AttemptAdmissionResult{}, ErrInputConflict
		}
		attempt, err := queries.LockGovernanceAttemptForUpdate(ctx, db.LockGovernanceAttemptForUpdateParams{
			WorkspaceID: command.WorkspaceID,
			CaseID:      command.CaseID,
			ID:          command.AttemptID,
		})
		if err != nil {
			return AttemptAdmissionResult{}, ErrInputConflict
		}
		if !sameAttemptAdmission(attempt, command) {
			return AttemptAdmissionResult{}, ErrInputConflict
		}
		return AttemptAdmissionResult{Case: caseRow, Attempt: attempt, Transition: prior, Duplicate: true}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return AttemptAdmissionResult{}, err
	}
	if CaseState(caseRow.State) != command.ExpectedState || caseRow.StateRevision != command.ExpectedRevision {
		return AttemptAdmissionResult{}, ErrStaleCase
	}
	now := service.clock.Now().UTC()
	if !caseRow.LeaseToken.Valid || caseRow.LeaseToken != command.LeaseToken ||
		!caseRow.LeaseExpiresAt.Valid || !caseRow.LeaseExpiresAt.Time.After(now) ||
		!caseRow.CurrentAttemptID.Valid || caseRow.CurrentAttemptID != command.ExpectedAttemptID {
		return AttemptAdmissionResult{}, ErrStaleFence
	}
	currentAttempt, err := queries.LockGovernanceAttemptForUpdate(ctx, db.LockGovernanceAttemptForUpdateParams{
		WorkspaceID: command.WorkspaceID,
		CaseID:      command.CaseID,
		ID:          command.ExpectedAttemptID,
	})
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (currentAttempt.AttemptFence != command.ExpectedAttemptFence || currentAttempt.RedactedAt.Valid) {
		return AttemptAdmissionResult{}, ErrStaleFence
	}
	if err != nil {
		return AttemptAdmissionResult{}, err
	}
	if !command.DeadlineAt.After(now) || caseRow.AbsoluteDeadline.Valid && command.DeadlineAt.After(caseRow.AbsoluteDeadline.Time) {
		return AttemptAdmissionResult{}, ErrInvalidTransition
	}
	if command.ExpectedRevision == maxCaseRevision || currentAttempt.Ordinal == math.MaxInt32 {
		return AttemptAdmissionResult{}, ErrInvalidTransition
	}
	transition, err := service.transitionLocked(ctx, queries, caseRow, transitionCommand)
	if err != nil {
		return AttemptAdmissionResult{}, err
	}
	attempt, err := queries.InsertGovernanceAttemptWithID(ctx, db.InsertGovernanceAttemptWithIDParams{
		ID:           command.AttemptID,
		WorkspaceID:  command.WorkspaceID,
		CaseID:       command.CaseID,
		Ordinal:      currentAttempt.Ordinal + 1,
		Kind:         "agent",
		CandidateID:  command.CandidateID,
		ObligationID: command.ObligationID,
		InputDigest:  command.InputDigest,
		AttemptFence: command.AttemptFence,
		DeadlineAt:   pgtype.Timestamptz{Time: command.DeadlineAt.UTC(), Valid: true},
		Confidence:   []byte("{}"),
		Result:       []byte("{}"),
		Usage:        []byte("{}"),
	})
	if err != nil {
		return AttemptAdmissionResult{}, err
	}
	caseRow, err = queries.AssociateGovernanceAttemptWithCase(ctx, db.AssociateGovernanceAttemptWithCaseParams{
		WorkspaceID:       command.WorkspaceID,
		CaseID:            command.CaseID,
		StateRevision:     command.ExpectedRevision + 1,
		ExpectedAttemptID: command.ExpectedAttemptID,
		AttemptID:         command.AttemptID,
		ObligationID:      command.ObligationID,
		UpdatedAt:         pgtype.Timestamptz{Time: now, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return AttemptAdmissionResult{}, ErrStaleCase
	}
	if err != nil {
		return AttemptAdmissionResult{}, err
	}
	return AttemptAdmissionResult{Case: caseRow, Attempt: attempt, Transition: transition.Transition}, nil
}

func validateAttemptAdmissionCommand(command AttemptAdmissionCommand) error {
	reason := attemptAdmissionReason(command.ExpectedState)
	if !validUUID(command.WorkspaceID) || !validUUID(command.CaseID) || command.ControlEpoch < 1 ||
		!validCaseState(command.ExpectedState) || command.ExpectedRevision < 0 ||
		!validUUID(command.LeaseToken) || !validUUID(command.ExpectedAttemptID) || !validUUID(command.ExpectedAttemptFence) ||
		!validEventKey(command.CauseEventKey) || !validUUID(command.AttemptID) || !validUUID(command.AttemptFence) ||
		!validUUID(command.CandidateID) || !validUUID(command.ObligationID) ||
		command.AttemptID == command.ExpectedAttemptID || command.AttemptFence == command.ExpectedAttemptFence ||
		strings.TrimSpace(command.InputDigest) == "" || len(command.InputDigest) > 256 || command.DeadlineAt.IsZero() ||
		!CanTransitionReason(command.ExpectedState, CaseAgentAttempt, reason) {
		return ErrInvalidTransition
	}
	return nil
}

func attemptAdmissionTransition(command AttemptAdmissionCommand) TransitionCommand {
	return TransitionCommand{
		WorkspaceID:      command.WorkspaceID,
		CaseID:           command.CaseID,
		ControlEpoch:     command.ControlEpoch,
		ExpectedState:    command.ExpectedState,
		ExpectedRevision: command.ExpectedRevision,
		NextState:        CaseAgentAttempt,
		CauseEventKey:    command.CauseEventKey,
		Actor:            ActorSystem,
		Reason:           attemptAdmissionReason(command.ExpectedState),
	}
}

func attemptAdmissionReason(state CaseState) CaseReason {
	if state == CaseNextAttempt {
		return ReasonRemainingEligibleSlot
	}
	return ReasonEligibleCandidate
}

func sameAttemptAdmission(attempt db.GovernanceAttempt, command AttemptAdmissionCommand) bool {
	return attempt.ID == command.AttemptID && attempt.WorkspaceID == command.WorkspaceID &&
		attempt.CaseID == command.CaseID && attempt.Kind == "agent" && attempt.CandidateID == command.CandidateID &&
		!attempt.TaskID.Valid && attempt.ObligationID == command.ObligationID &&
		attempt.InputDigest == command.InputDigest && attempt.AttemptFence == command.AttemptFence &&
		attempt.DeadlineAt.Valid && attempt.DeadlineAt.Time.Equal(command.DeadlineAt.UTC()) &&
		string(attempt.Confidence) == "{}" && string(attempt.Result) == "{}" && string(attempt.Usage) == "{}"
}
