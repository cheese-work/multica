package caselifecycle

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var (
	ErrStaleCase          = errors.New("governance case revision is stale")
	ErrInvalidTransition  = errors.New("governance case transition is invalid")
	ErrLeaseHeld          = errors.New("governance case lease is held")
	ErrStaleFence         = errors.New("governance case attempt fence is stale")
	ErrStaleControlEpoch  = errors.New("governance workspace control epoch is stale or disabled")
	ErrInputConflict      = errors.New("governance case input key conflicts with prior transition")
	ErrSuccessorConflict  = errors.New("governance case successor conflicts with prior generation")
	ErrStaleEvidenceEpoch = errors.New("governance evidence epoch is stale")
)

type Database interface {
	db.DBTX
	Begin(context.Context) (pgx.Tx, error)
}

type Clock interface {
	Now() time.Time
}

type ClockFunc func() time.Time

func (clock ClockFunc) Now() time.Time {
	return clock()
}

type ActorType string

const (
	ActorMember ActorType = "member"
	ActorAgent  ActorType = "agent"
	ActorSystem ActorType = "system"
)

type Service struct {
	database Database
	clock    Clock
}

func NewService(database Database, clock Clock) (*Service, error) {
	if database == nil || clock == nil {
		return nil, errors.New("governance case service requires a database and clock")
	}
	return &Service{database: database, clock: clock}, nil
}

type TransitionCommand struct {
	WorkspaceID      pgtype.UUID
	CaseID           pgtype.UUID
	ControlEpoch     int64
	ExpectedState    CaseState
	ExpectedRevision int64
	NextState        CaseState
	CauseEventKey    string
	Actor            ActorType
	ActorID          pgtype.UUID
	Reason           CaseReason
	LeaseToken       pgtype.UUID
	AttemptID        pgtype.UUID
	AttemptFence     pgtype.UUID
	// EvidenceEpoch is the epoch the actor reviewed; human approval requires it.
	EvidenceEpoch *int32
}

type TransitionResult struct {
	Case       db.GovernanceCase
	Transition db.GovernanceCaseTransition
	Duplicate  bool
}

type LeaseCommand struct {
	WorkspaceID      pgtype.UUID
	CaseID           pgtype.UUID
	ControlEpoch     int64
	ExpectedState    CaseState
	ExpectedRevision int64
	Token            pgtype.UUID
	Duration         time.Duration
}

type SuccessorCommand struct {
	WorkspaceID      pgtype.UUID
	PredecessorID    pgtype.UUID
	ExpectedState    CaseState
	ExpectedRevision int64
	CauseEventKey    string
	Actor            ActorType
	ActorID          pgtype.UUID
	Successor        db.InsertNextGovernanceCaseParams
}

type SuccessorResult struct {
	Case            db.GovernanceCase
	Invalidation    db.GovernanceCaseTransition
	HasInvalidation bool
	Duplicate       bool
}

func (service *Service) Transition(ctx context.Context, command TransitionCommand) (TransitionResult, error) {
	if err := validateTransitionCommand(command); err != nil {
		return TransitionResult{}, err
	}
	tx, err := service.database.Begin(ctx)
	if err != nil {
		return TransitionResult{}, fmt.Errorf("begin governance case transition: %w", err)
	}
	defer tx.Rollback(ctx)

	if err := lockGovernanceControl(ctx, tx, command.WorkspaceID, command.ControlEpoch); err != nil {
		return TransitionResult{}, err
	}
	queries := db.New(tx)
	caseRow, err := queries.LockGovernanceCaseForUpdate(ctx, db.LockGovernanceCaseForUpdateParams{
		WorkspaceID: command.WorkspaceID,
		ID:          command.CaseID,
	})
	if err != nil {
		return TransitionResult{}, fmt.Errorf("lock governance case: %w", err)
	}
	if caseRow.ControlEpoch != command.ControlEpoch {
		return TransitionResult{}, ErrStaleControlEpoch
	}
	if command.Reason == ReasonHumanApproval && CaseState(caseRow.State) == command.ExpectedState &&
		caseRow.StateRevision == command.ExpectedRevision {
		// Epoch equality is not freshness: aged evidence must refresh first.
		_, capturedAt, err := currentEvidenceCapture(ctx, tx, caseRow)
		if err != nil {
			return TransitionResult{}, err
		}
		if service.clock.Now().UTC().Sub(capturedAt) > MaxEvidenceFreshness {
			return TransitionResult{}, ErrEvidenceExpired
		}
	}
	result, err := service.transitionLocked(ctx, queries, caseRow, command)
	if err != nil {
		return TransitionResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return TransitionResult{}, fmt.Errorf("commit governance case transition: %w", err)
	}
	return result, nil
}

func (service *Service) ClaimLease(ctx context.Context, command LeaseCommand) (db.GovernanceCase, error) {
	if !validUUID(command.WorkspaceID) || !validUUID(command.CaseID) || !validUUID(command.Token) ||
		!validCaseState(command.ExpectedState) || command.ExpectedState.IsTerminal() || command.ExpectedState == CaseParked ||
		command.ControlEpoch < 1 || command.ExpectedRevision < 0 || command.Duration <= 0 {
		return db.GovernanceCase{}, errors.New("invalid governance case lease request")
	}
	now := service.clock.Now().UTC()
	expiresAt := now.Add(command.Duration)
	if !expiresAt.After(now) {
		return db.GovernanceCase{}, errors.New("governance case lease duration overflows clock")
	}
	tx, err := service.database.Begin(ctx)
	if err != nil {
		return db.GovernanceCase{}, fmt.Errorf("begin governance case lease claim: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := lockGovernanceControl(ctx, tx, command.WorkspaceID, command.ControlEpoch); err != nil {
		return db.GovernanceCase{}, err
	}
	queries := db.New(tx)
	caseRow, err := queries.LockGovernanceCaseForUpdate(ctx, db.LockGovernanceCaseForUpdateParams{
		WorkspaceID: command.WorkspaceID,
		ID:          command.CaseID,
	})
	if err != nil {
		return db.GovernanceCase{}, fmt.Errorf("lock governance case for lease: %w", err)
	}
	if caseRow.ControlEpoch != command.ControlEpoch {
		return db.GovernanceCase{}, ErrStaleControlEpoch
	}
	if CaseState(caseRow.State) != command.ExpectedState || caseRow.StateRevision != command.ExpectedRevision {
		return db.GovernanceCase{}, ErrStaleCase
	}
	if caseRow.LeaseToken.Valid && (!caseRow.LeaseExpiresAt.Valid || caseRow.LeaseExpiresAt.Time.After(now)) {
		return db.GovernanceCase{}, ErrLeaseHeld
	}
	updated, err := queries.UpdateGovernanceCaseLease(ctx, db.UpdateGovernanceCaseLeaseParams{
		WorkspaceID:    command.WorkspaceID,
		ID:             command.CaseID,
		StateRevision:  command.ExpectedRevision,
		LeaseToken:     command.Token,
		LeaseExpiresAt: pgtype.Timestamptz{Time: expiresAt, Valid: true},
		UpdatedAt:      pgtype.Timestamptz{Time: now, Valid: true},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.GovernanceCase{}, ErrStaleCase
		}
		return db.GovernanceCase{}, fmt.Errorf("claim governance case lease: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return db.GovernanceCase{}, fmt.Errorf("commit governance case lease claim: %w", err)
	}
	return updated, nil
}

func (service *Service) ReleaseLease(ctx context.Context, workspaceID, caseID, token pgtype.UUID, capturedControlEpoch int64) (bool, error) {
	if !validUUID(workspaceID) || !validUUID(caseID) || !validUUID(token) || capturedControlEpoch < 1 {
		return false, errors.New("invalid governance case lease release")
	}
	tx, err := service.database.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin governance case lease release: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := lockGovernanceControl(ctx, tx, workspaceID, capturedControlEpoch); err != nil {
		return false, err
	}
	queries := db.New(tx)
	caseRow, err := queries.LockGovernanceCaseForUpdate(ctx, db.LockGovernanceCaseForUpdateParams{
		WorkspaceID: workspaceID,
		ID:          caseID,
	})
	if err != nil {
		return false, fmt.Errorf("lock governance case for lease release: %w", err)
	}
	if caseRow.ControlEpoch != capturedControlEpoch {
		return false, ErrStaleControlEpoch
	}
	count, err := queries.ReleaseGovernanceCaseLease(ctx, db.ReleaseGovernanceCaseLeaseParams{
		WorkspaceID: workspaceID,
		ID:          caseID,
		LeaseToken:  token,
		UpdatedAt:   pgtype.Timestamptz{Time: service.clock.Now().UTC(), Valid: true},
	})
	if err != nil {
		return false, fmt.Errorf("release governance case lease: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit governance case lease release: %w", err)
	}
	return count == 1, nil
}

func (service *Service) CreateSuccessor(ctx context.Context, command SuccessorCommand) (SuccessorResult, error) {
	if err := validateSuccessorCommand(command); err != nil {
		return SuccessorResult{}, err
	}
	tx, err := service.database.Begin(ctx)
	if err != nil {
		return SuccessorResult{}, fmt.Errorf("begin governance case successor: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := lockGovernanceControl(ctx, tx, command.WorkspaceID, command.Successor.ControlEpoch); err != nil {
		return SuccessorResult{}, err
	}
	queries := db.New(tx)
	next := command.Successor
	if err := queries.LockGovernanceCaseIdentity(ctx, db.LockGovernanceCaseIdentityParams{
		WorkspaceID: command.WorkspaceID,
		SubjectType: pgtype.Text{String: next.SubjectType, Valid: true},
		SubjectID:   next.SubjectID,
		RuleID:      next.RuleID,
	}); err != nil {
		return SuccessorResult{}, fmt.Errorf("lock governance case identity: %w", err)
	}
	predecessor, err := queries.LockGovernanceCaseForUpdate(ctx, db.LockGovernanceCaseForUpdateParams{
		WorkspaceID: command.WorkspaceID,
		ID:          command.PredecessorID,
	})
	if err != nil {
		return SuccessorResult{}, fmt.Errorf("lock governance case predecessor: %w", err)
	}
	if predecessor.ControlEpoch != next.ControlEpoch {
		return SuccessorResult{}, ErrStaleControlEpoch
	}
	if predecessor.SubjectType != next.SubjectType || predecessor.SubjectID != next.SubjectID || predecessor.RuleID != next.RuleID {
		return SuccessorResult{}, ErrSuccessorConflict
	}
	existing, err := queries.FindGovernanceCaseByMaterialFingerprint(ctx, db.FindGovernanceCaseByMaterialFingerprintParams{
		WorkspaceID:         next.WorkspaceID,
		SubjectType:         next.SubjectType,
		SubjectID:           next.SubjectID,
		SubjectRevision:     next.SubjectRevision,
		RuleID:              next.RuleID,
		MaterialFingerprint: next.MaterialFingerprint,
	})
	if err == nil {
		// A repeat delivery of the facts the predecessor already covers (new
		// arrival time, duplicate PR/native/MJ event) is not a material change.
		if existing.ID == predecessor.ID {
			if err := tx.Commit(ctx); err != nil {
				return SuccessorResult{}, fmt.Errorf("commit duplicate governance case event: %w", err)
			}
			return SuccessorResult{Case: existing, Duplicate: true}, nil
		}
		if !existing.PredecessorCaseID.Valid || existing.PredecessorCaseID != predecessor.ID || existing.BudgetRootID != predecessor.BudgetRootID {
			return SuccessorResult{}, ErrSuccessorConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return SuccessorResult{}, fmt.Errorf("commit resolved governance case successor: %w", err)
		}
		return SuccessorResult{Case: existing, Duplicate: true}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return SuccessorResult{}, fmt.Errorf("find governance case successor: %w", err)
	}
	if CaseState(predecessor.State) != command.ExpectedState || predecessor.StateRevision != command.ExpectedRevision {
		return SuccessorResult{}, ErrStaleCase
	}
	result := SuccessorResult{}
	if !command.ExpectedState.IsTerminal() {
		transition, err := service.transitionLocked(ctx, queries, predecessor, TransitionCommand{
			WorkspaceID:      command.WorkspaceID,
			CaseID:           command.PredecessorID,
			ExpectedState:    command.ExpectedState,
			ExpectedRevision: command.ExpectedRevision,
			NextState:        CaseInvalidated,
			CauseEventKey:    command.CauseEventKey,
			Actor:            command.Actor,
			ActorID:          command.ActorID,
			Reason:           ReasonMaterialChanged,
		})
		if err != nil {
			return SuccessorResult{}, err
		}
		result.Invalidation = transition.Transition
		result.HasInvalidation = true
	}
	next.PredecessorCaseID = predecessor.ID
	next.BudgetRootID = predecessor.BudgetRootID
	next.AbsoluteDeadline = predecessor.AbsoluteDeadline
	next.FrozenStrategy = append([]byte(nil), predecessor.FrozenStrategy...)
	next.EvidenceEpoch = predecessor.EvidenceEpoch
	next.RefreshCount = predecessor.RefreshCount
	created, err := queries.InsertNextGovernanceCase(ctx, next)
	if err != nil {
		return SuccessorResult{}, fmt.Errorf("insert governance case successor: %w", err)
	}
	result.Case = created
	if err := tx.Commit(ctx); err != nil {
		return SuccessorResult{}, fmt.Errorf("commit governance case successor: %w", err)
	}
	return result, nil
}

func lockGovernanceControl(ctx context.Context, tx pgx.Tx, workspaceID pgtype.UUID, capturedControlEpoch int64) error {
	if capturedControlEpoch < 1 {
		return ErrStaleControlEpoch
	}
	var controlEpoch int64
	var enabled bool
	err := tx.QueryRow(ctx, `
		SELECT control_epoch, settings->>'jev_governance_enabled' = 'true'
		FROM governance_workspace_config
		WHERE workspace_id = $1
		FOR SHARE
	`, workspaceID).Scan(&controlEpoch, &enabled)
	if err != nil {
		return ErrStaleControlEpoch
	}
	if !enabled || controlEpoch != capturedControlEpoch {
		return ErrStaleControlEpoch
	}
	return nil
}

func (service *Service) transitionLocked(ctx context.Context, queries *db.Queries, caseRow db.GovernanceCase, command TransitionCommand) (TransitionResult, error) {
	prior, err := queries.FindGovernanceCaseTransitionByCause(ctx, db.FindGovernanceCaseTransitionByCauseParams{
		WorkspaceID:   command.WorkspaceID,
		CaseID:        command.CaseID,
		CauseEventKey: command.CauseEventKey,
	})
	if err == nil {
		if !sameTransitionRequest(prior, command) {
			return TransitionResult{}, ErrInputConflict
		}
		return TransitionResult{Case: caseRow, Transition: prior, Duplicate: true}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return TransitionResult{}, fmt.Errorf("read governance transition input: %w", err)
	}
	if CaseState(caseRow.State) != command.ExpectedState || caseRow.StateRevision != command.ExpectedRevision {
		return TransitionResult{}, ErrStaleCase
	}
	if command.Reason == ReasonHumanApproval && (command.EvidenceEpoch == nil || *command.EvidenceEpoch != caseRow.EvidenceEpoch) {
		return TransitionResult{}, ErrStaleEvidenceEpoch
	}
	if requiresAttemptFence(command) {
		if err := service.validateAttemptFence(ctx, queries, caseRow, command); err != nil {
			return TransitionResult{}, err
		}
	}
	if command.ExpectedRevision == maxCaseRevision {
		return TransitionResult{}, errors.New("governance case revision exhausted")
	}
	now := service.clock.Now().UTC()
	updated, err := queries.UpdateGovernanceCaseTransitionCAS(ctx, db.UpdateGovernanceCaseTransitionCASParams{
		WorkspaceID:           command.WorkspaceID,
		ID:                    command.CaseID,
		ExpectedState:         string(command.ExpectedState),
		ExpectedStateRevision: command.ExpectedRevision,
		NextState:             string(command.NextState),
		Reason:                string(command.Reason),
		UpdatedAt:             pgtype.Timestamptz{Time: now, Valid: true},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return TransitionResult{}, ErrStaleCase
		}
		return TransitionResult{}, fmt.Errorf("compare-and-set governance case: %w", err)
	}
	transition, err := queries.InsertGovernanceCaseTransition(ctx, db.InsertGovernanceCaseTransitionParams{
		WorkspaceID:            command.WorkspaceID,
		CaseID:                 command.CaseID,
		ResultingStateRevision: command.ExpectedRevision + 1,
		ExpectedStateRevision:  command.ExpectedRevision,
		FromState:              string(command.ExpectedState),
		ToState:                string(command.NextState),
		CauseEventKey:          command.CauseEventKey,
		ActorType:              string(command.Actor),
		ActorID:                command.ActorID,
		SanitizedReason:        string(command.Reason),
	})
	if err != nil {
		return TransitionResult{}, fmt.Errorf("append governance transition: %w", err)
	}
	return TransitionResult{Case: updated, Transition: transition}, nil
}

func (service *Service) validateAttemptFence(ctx context.Context, queries *db.Queries, caseRow db.GovernanceCase, command TransitionCommand) error {
	now := service.clock.Now().UTC()
	if !validUUID(command.LeaseToken) || caseRow.LeaseToken != command.LeaseToken ||
		!caseRow.LeaseExpiresAt.Valid || !caseRow.LeaseExpiresAt.Time.After(now) ||
		!validUUID(command.AttemptID) || !validUUID(command.AttemptFence) ||
		!caseRow.CurrentAttemptID.Valid || caseRow.CurrentAttemptID != command.AttemptID {
		return ErrStaleFence
	}
	attempt, err := queries.LockGovernanceAttemptForUpdate(ctx, db.LockGovernanceAttemptForUpdateParams{
		WorkspaceID: command.WorkspaceID,
		CaseID:      command.CaseID,
		ID:          command.AttemptID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrStaleFence
		}
		return fmt.Errorf("lock governance attempt fence: %w", err)
	}
	now = service.clock.Now().UTC()
	if !caseRow.LeaseExpiresAt.Time.After(now) ||
		attempt.AttemptFence != command.AttemptFence || attempt.TerminalAt.Valid ||
		(attempt.DeadlineAt.Valid && !attempt.DeadlineAt.Time.After(now)) {
		return ErrStaleFence
	}
	return nil
}

func validateTransitionCommand(command TransitionCommand) error {
	if !validUUID(command.WorkspaceID) || !validUUID(command.CaseID) ||
		command.ControlEpoch < 1 ||
		!validCaseState(command.ExpectedState) || !validCaseState(command.NextState) ||
		command.ExpectedRevision < 0 || !validEventKey(command.CauseEventKey) ||
		!validActor(command.Actor, command.ActorID) || !validCaseReason(command.Reason) ||
		!CanTransitionReason(command.ExpectedState, command.NextState, command.Reason) {
		return ErrInvalidTransition
	}
	if requiresAttemptFence(command) &&
		(!validUUID(command.LeaseToken) || !validUUID(command.AttemptID) || !validUUID(command.AttemptFence)) {
		return ErrStaleFence
	}
	return nil
}

func validateSuccessorCommand(command SuccessorCommand) error {
	next := command.Successor
	if !validUUID(command.WorkspaceID) || !validUUID(command.PredecessorID) ||
		!validCaseState(command.ExpectedState) || command.ExpectedRevision < 0 ||
		!validEventKey(command.CauseEventKey) || !validActor(command.Actor, command.ActorID) ||
		!validUUID(next.WorkspaceID) || next.WorkspaceID != command.WorkspaceID ||
		next.SubjectType == "" || !validUUID(next.SubjectID) || !validUUID(next.RuleID) ||
		next.MaterialFingerprint == "" || next.ControlEpoch < 1 || next.State != string(CaseCaptured) ||
		len(next.AuthorityLineage) == 0 || len(next.TriggerAliases) == 0 {
		return ErrSuccessorConflict
	}
	return nil
}

func sameTransitionRequest(prior db.GovernanceCaseTransition, command TransitionCommand) bool {
	return prior.ExpectedStateRevision == command.ExpectedRevision &&
		prior.FromState == string(command.ExpectedState) && prior.ToState == string(command.NextState) &&
		prior.ActorType == string(command.Actor) && prior.ActorID == command.ActorID &&
		prior.SanitizedReason == string(command.Reason)
}

func requiresAttemptFence(command TransitionCommand) bool {
	switch command.ExpectedState {
	case CaseJevEvaluating:
		return command.NextState == CaseCorrectionPending || command.NextState == CaseAgentEscalation || command.NextState == CaseAbstained
	case CaseAgentAttempt:
		return command.NextState == CaseCorrectionPending || command.NextState == CaseNextAttempt
	default:
		return false
	}
}

func validActor(actor ActorType, actorID pgtype.UUID) bool {
	switch actor {
	case ActorSystem:
		return !actorID.Valid
	case ActorMember, ActorAgent:
		return validUUID(actorID)
	default:
		return false
	}
}

func validEventKey(key string) bool {
	return key != "" && key == strings.TrimSpace(key) && len(key) <= 256
}

func validUUID(id pgtype.UUID) bool {
	return id.Valid && id.Bytes != [16]byte{}
}

const maxCaseRevision = int64(1<<63 - 1)
