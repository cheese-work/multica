package governance

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var (
	ErrBudgetInput          = errors.New("invalid governance budget input")
	ErrBudgetRetryUnbounded = errors.New("governance budget retry policy is unbounded")
	ErrBudgetOverflow       = errors.New("governance budget arithmetic overflow")
	ErrBudgetLimit          = errors.New("governance budget limit reached")
	ErrBudgetWindow         = errors.New("governance budget window is not current")
	ErrBudgetWindowConflict = errors.New("governance budget window identity conflicts")
	ErrBudgetConflict       = errors.New("governance budget request conflicts with prior event")
	ErrBudgetNotFound       = errors.New("governance budget reservation not found")
	ErrBudgetRevision       = errors.New("governance budget reservation revision is stale")
	ErrBudgetInvariant      = errors.New("governance budget accounting is inconsistent")
)

type BudgetClock interface {
	Now() time.Time
}

type BudgetService struct {
	database *pgxpool.Pool
	clock    BudgetClock
}

type BudgetReserveCommand struct {
	WorkspaceID            pgtype.UUID
	ReservationID          pgtype.UUID
	BudgetRootID           pgtype.UUID
	CaseID                 pgtype.UUID
	AttemptID              pgtype.UUID
	ObligationID           pgtype.UUID
	Resource               string
	ControlEpoch           int64
	WindowStart            time.Time
	WindowEnd              time.Time
	RootCapMicroUSD        int64
	WindowCapMicroUSD      int64
	MaxAttemptCostMicroUSD int64
	RetryAllowance         int64
	RetryPolicyBounded     bool
	SlotLimit              int64
	EventKey               string
}

type BudgetReservationResult struct {
	Reservation db.GovernanceBudgetReservation
	Duplicate   bool
}

type BudgetDebitCommand struct {
	WorkspaceID      pgtype.UUID
	ReservationID    pgtype.UUID
	ExpectedRevision int64
	EventKey         string
	AmountMicroUSD   int64
}

type BudgetSettleCommand struct {
	WorkspaceID      pgtype.UUID
	ReservationID    pgtype.UUID
	ExpectedRevision int64
	EventKey         string
	ReceiptID        string
	UsageKnown       bool
	TerminationKnown bool
	UsageMicroUSD    int64
}

type BudgetSettlementResult struct {
	Reservation db.GovernanceBudgetReservation
	Duplicate   bool
	Finalized   bool
}

func NewBudgetService(database *pgxpool.Pool, clock BudgetClock) (*BudgetService, error) {
	if database == nil || clock == nil {
		return nil, errors.New("governance budget service requires a database and clock")
	}
	return &BudgetService{database: database, clock: clock}, nil
}

func (service *BudgetService) Reserve(ctx context.Context, command BudgetReserveCommand) (BudgetReservationResult, error) {
	windowStart := command.WindowStart.UTC()
	windowEnd := command.WindowEnd.UTC()
	totalCap, err := budgetTotalCap(command.MaxAttemptCostMicroUSD, command.RetryAllowance)
	if err != nil {
		return BudgetReservationResult{}, err
	}
	if !validBudgetReserveCommand(command, windowStart, windowEnd) {
		if !command.RetryPolicyBounded {
			return BudgetReservationResult{}, ErrBudgetRetryUnbounded
		}
		return BudgetReservationResult{}, ErrBudgetInput
	}
	digest, err := budgetReserveDigest(command, windowStart, windowEnd)
	if err != nil {
		return BudgetReservationResult{}, err
	}
	tx, err := service.database.Begin(ctx)
	if err != nil {
		return BudgetReservationResult{}, fmt.Errorf("begin governance budget reservation: %w", err)
	}
	defer tx.Rollback(ctx)
	guards, err := LockConcurrencyResources(ctx, tx, command.WorkspaceID, command.Resource)
	if err != nil {
		return BudgetReservationResult{}, err
	}
	reservation, err := readBudgetReservation(ctx, tx, command.WorkspaceID, command.ReservationID, false)
	if err == nil {
		if reservation.RequestDigest != digest {
			return BudgetReservationResult{}, ErrBudgetConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return BudgetReservationResult{}, fmt.Errorf("commit duplicate governance budget reservation: %w", err)
		}
		return BudgetReservationResult{Reservation: reservation, Duplicate: true}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return BudgetReservationResult{}, err
	}
	now := service.clock.Now().UTC()
	if now.Before(windowStart) || !now.Before(windowEnd) {
		return BudgetReservationResult{}, ErrBudgetWindow
	}
	if _, err := readBudgetReservationByAttempt(ctx, tx, command.WorkspaceID, command.AttemptID, command.ObligationID); err == nil {
		return BudgetReservationResult{}, ErrBudgetConflict
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return BudgetReservationResult{}, err
	}
	if _, err := guards.Reserve(ctx, command.Resource, command.ReservationID, command.ControlEpoch, command.SlotLimit); err != nil {
		return BudgetReservationResult{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO governance_budget_root (workspace_id, budget_root_id, spend_cap_micro_usd)
		VALUES ($1, $2, $3) ON CONFLICT (workspace_id, budget_root_id) DO NOTHING
	`, command.WorkspaceID, command.BudgetRootID, command.RootCapMicroUSD); err != nil {
		return BudgetReservationResult{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO governance_budget_window (workspace_id, window_start, window_end, spend_cap_micro_usd)
		VALUES ($1, $2, $3, $4) ON CONFLICT (workspace_id, window_start) DO NOTHING
	`, command.WorkspaceID, windowStart, windowEnd, command.WindowCapMicroUSD); err != nil {
		return BudgetReservationResult{}, err
	}
	state, err := loadBudgetState(ctx, tx, command.WorkspaceID)
	if err != nil {
		return BudgetReservationResult{}, err
	}
	root, ok := state.roots[command.BudgetRootID]
	if !ok {
		return BudgetReservationResult{}, ErrBudgetInvariant
	}
	window, ok := state.windows[budgetWindowKey(windowStart)]
	if !ok {
		return BudgetReservationResult{}, ErrBudgetInvariant
	}
	if !window.end.Equal(windowEnd) {
		return BudgetReservationResult{}, ErrBudgetWindowConflict
	}
	if err := checkBudgetAdmission(state, command, totalCap, now, root, window); err != nil {
		return BudgetReservationResult{}, err
	}
	effectiveRootCap := minBudgetCap(root.cap, command.RootCapMicroUSD)
	effectiveWindowCap := minBudgetCap(window.cap, command.WindowCapMicroUSD)
	_, err = tx.Exec(ctx, `
		INSERT INTO governance_budget_reservation (
			workspace_id, reservation_id, budget_root_id, case_id, attempt_id, obligation_id,
			resource, control_epoch, window_start, window_end, root_cap_micro_usd,
			window_cap_micro_usd, max_attempt_cost_micro_usd, retry_allowance,
			retry_policy_bounded, retry_allowance_remaining, attempts_started,
			total_cap_micro_usd, remaining_micro_usd, request_digest
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, TRUE, $14, 1, $15, $15, $16)
	`, command.WorkspaceID, command.ReservationID, command.BudgetRootID, command.CaseID,
		command.AttemptID, command.ObligationID, command.Resource, command.ControlEpoch,
		windowStart, windowEnd, effectiveRootCap, effectiveWindowCap,
		command.MaxAttemptCostMicroUSD, command.RetryAllowance, totalCap, digest)
	if err != nil {
		return BudgetReservationResult{}, err
	}
	if err := adjustBudgetCounters(ctx, tx, command.WorkspaceID, command.BudgetRootID, windowStart, totalCap, 0, totalCap, 0); err != nil {
		return BudgetReservationResult{}, err
	}
	if err := insertBudgetJournal(ctx, tx, command.WorkspaceID, command.ReservationID, command.EventKey, "reserve", digest, 0, 1); err != nil {
		return BudgetReservationResult{}, err
	}
	if err := insertBudgetOutbox(ctx, tx, command.WorkspaceID, command.ReservationID, command.EventKey, "admit", command.CaseID, command.AttemptID, command.ObligationID); err != nil {
		return BudgetReservationResult{}, err
	}
	reservation, err = readBudgetReservation(ctx, tx, command.WorkspaceID, command.ReservationID, false)
	if err != nil {
		return BudgetReservationResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return BudgetReservationResult{}, fmt.Errorf("commit governance budget reservation: %w", err)
	}
	return BudgetReservationResult{Reservation: reservation}, nil
}

func (service *BudgetService) Debit(ctx context.Context, command BudgetDebitCommand) (BudgetReservationResult, error) {
	if !validConcurrencyUUID(command.WorkspaceID) || !validConcurrencyUUID(command.ReservationID) ||
		command.ExpectedRevision < 1 || command.AmountMicroUSD <= 0 || !validBudgetKey(command.EventKey) {
		return BudgetReservationResult{}, ErrBudgetInput
	}
	tx, err := service.database.Begin(ctx)
	if err != nil {
		return BudgetReservationResult{}, fmt.Errorf("begin governance budget debit: %w", err)
	}
	defer tx.Rollback(ctx)
	location, err := readBudgetLocation(ctx, tx, command.WorkspaceID, command.ReservationID)
	if err != nil {
		return BudgetReservationResult{}, err
	}
	if _, err := LockConcurrencyResources(ctx, tx, command.WorkspaceID, location.resource); err != nil {
		return BudgetReservationResult{}, err
	}
	state, err := loadBudgetState(ctx, tx, command.WorkspaceID)
	if err != nil {
		return BudgetReservationResult{}, err
	}
	reservation, ok := state.reservations[command.ReservationID]
	if !ok {
		return BudgetReservationResult{}, ErrBudgetNotFound
	}
	digest := budgetDebitDigest(command)
	duplicate, err := findBudgetJournal(ctx, tx, command.WorkspaceID, command.ReservationID, command.EventKey, digest)
	if err != nil {
		return BudgetReservationResult{}, err
	}
	if duplicate {
		if err := tx.Commit(ctx); err != nil {
			return BudgetReservationResult{}, fmt.Errorf("commit duplicate governance budget debit: %w", err)
		}
		return BudgetReservationResult{Reservation: reservation, Duplicate: true}, nil
	}
	if reservation.Revision != command.ExpectedRevision {
		return BudgetReservationResult{}, ErrBudgetRevision
	}
	if reservation.State != "reserved" || command.AmountMicroUSD > reservation.MaxAttemptCostMicroUsd || command.AmountMicroUSD > reservation.RemainingMicroUsd {
		return BudgetReservationResult{}, ErrBudgetLimit
	}
	resultingRevision := reservation.Revision + 1
	if resultingRevision <= reservation.Revision {
		return BudgetReservationResult{}, ErrBudgetOverflow
	}
	if err := adjustBudgetCounters(ctx, tx, command.WorkspaceID, reservation.BudgetRootID, reservation.WindowStart.Time, -command.AmountMicroUSD, command.AmountMicroUSD, -command.AmountMicroUSD, command.AmountMicroUSD); err != nil {
		return BudgetReservationResult{}, err
	}
	_, err = tx.Exec(ctx, `
		UPDATE governance_budget_reservation
		SET remaining_micro_usd = remaining_micro_usd - $3,
		    debited_micro_usd = debited_micro_usd + $3,
		    revision = $4, updated_at = now()
		WHERE workspace_id = $1 AND reservation_id = $2 AND state = 'reserved'
	`, command.WorkspaceID, command.ReservationID, command.AmountMicroUSD, resultingRevision)
	if err != nil {
		return BudgetReservationResult{}, err
	}
	if err := insertBudgetJournal(ctx, tx, command.WorkspaceID, command.ReservationID, command.EventKey, "debit", digest, reservation.Revision, resultingRevision); err != nil {
		return BudgetReservationResult{}, err
	}
	reservation, err = readBudgetReservation(ctx, tx, command.WorkspaceID, command.ReservationID, false)
	if err != nil {
		return BudgetReservationResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return BudgetReservationResult{}, fmt.Errorf("commit governance budget debit: %w", err)
	}
	return BudgetReservationResult{Reservation: reservation}, nil
}

func (service *BudgetService) Settle(ctx context.Context, command BudgetSettleCommand) (BudgetSettlementResult, error) {
	if !validConcurrencyUUID(command.WorkspaceID) || !validConcurrencyUUID(command.ReservationID) ||
		command.ExpectedRevision < 1 || !validBudgetKey(command.EventKey) || command.UsageMicroUSD < 0 ||
		(command.TerminationKnown && !validBudgetKey(command.ReceiptID)) ||
		(!command.TerminationKnown && command.ReceiptID != "") || (!command.UsageKnown && command.UsageMicroUSD != 0) {
		return BudgetSettlementResult{}, ErrBudgetInput
	}
	tx, err := service.database.Begin(ctx)
	if err != nil {
		return BudgetSettlementResult{}, fmt.Errorf("begin governance budget settlement: %w", err)
	}
	defer tx.Rollback(ctx)
	location, err := readBudgetLocation(ctx, tx, command.WorkspaceID, command.ReservationID)
	if err != nil {
		return BudgetSettlementResult{}, err
	}
	guards, err := LockConcurrencyResources(ctx, tx, command.WorkspaceID, location.resource)
	if err != nil {
		return BudgetSettlementResult{}, err
	}
	state, err := loadBudgetState(ctx, tx, command.WorkspaceID)
	if err != nil {
		return BudgetSettlementResult{}, err
	}
	reservation, ok := state.reservations[command.ReservationID]
	if !ok {
		return BudgetSettlementResult{}, ErrBudgetNotFound
	}
	digest := budgetSettleDigest(command)
	duplicate, err := findBudgetJournal(ctx, tx, command.WorkspaceID, command.ReservationID, command.EventKey, digest)
	if err != nil {
		return BudgetSettlementResult{}, err
	}
	if duplicate {
		if err := tx.Commit(ctx); err != nil {
			return BudgetSettlementResult{}, fmt.Errorf("commit duplicate governance budget settlement: %w", err)
		}
		return BudgetSettlementResult{Reservation: reservation, Duplicate: true, Finalized: reservation.State == "settled"}, nil
	}
	if reservation.State == "settled" {
		settledAmount := reservation.TotalCapMicroUsd
		if command.UsageKnown {
			settledAmount = command.UsageMicroUSD
		}
		if reservation.SettlementReceiptID.Valid && reservation.SettlementReceiptID.String == command.ReceiptID &&
			reservation.UsageKnown == command.UsageKnown && reservation.SettledMicroUsd == settledAmount {
			if err := tx.Commit(ctx); err != nil {
				return BudgetSettlementResult{}, fmt.Errorf("commit duplicate governance budget receipt: %w", err)
			}
			return BudgetSettlementResult{Reservation: reservation, Duplicate: true, Finalized: true}, nil
		}
		return BudgetSettlementResult{}, ErrBudgetConflict
	}
	if reservation.Revision != command.ExpectedRevision {
		return BudgetSettlementResult{}, ErrBudgetRevision
	}
	if command.UsageKnown && command.UsageMicroUSD > reservation.TotalCapMicroUsd {
		return BudgetSettlementResult{}, ErrBudgetLimit
	}
	if !command.TerminationKnown {
		if command.UsageKnown {
			if command.UsageMicroUSD < reservation.DebitedMicroUsd {
				return BudgetSettlementResult{}, ErrBudgetInvariant
			}
			additionalDebit := command.UsageMicroUSD - reservation.DebitedMicroUsd
			if additionalDebit > reservation.RemainingMicroUsd {
				return BudgetSettlementResult{}, ErrBudgetInvariant
			}
			if additionalDebit > 0 {
				if err := adjustBudgetCounters(ctx, tx, command.WorkspaceID, reservation.BudgetRootID, reservation.WindowStart.Time, -additionalDebit, additionalDebit, -additionalDebit, additionalDebit); err != nil {
					return BudgetSettlementResult{}, err
				}
			}
			_, err = tx.Exec(ctx, `
				UPDATE governance_budget_reservation
				SET remaining_micro_usd = remaining_micro_usd - $3,
				    debited_micro_usd = debited_micro_usd + $3,
				    usage_known = TRUE, revision = $4, updated_at = now()
				WHERE workspace_id = $1 AND reservation_id = $2 AND state = 'reserved'
			`, command.WorkspaceID, command.ReservationID, additionalDebit, reservation.Revision+1)
		} else {
			_, err = tx.Exec(ctx, `
				UPDATE governance_budget_reservation
				SET revision = revision + 1, updated_at = now()
				WHERE workspace_id = $1 AND reservation_id = $2 AND state = 'reserved'
			`, command.WorkspaceID, command.ReservationID)
		}
		if err != nil {
			return BudgetSettlementResult{}, err
		}
		if err := insertBudgetJournal(ctx, tx, command.WorkspaceID, command.ReservationID, command.EventKey, "settle", digest, reservation.Revision, reservation.Revision+1); err != nil {
			return BudgetSettlementResult{}, err
		}
		reservation, err = readBudgetReservation(ctx, tx, command.WorkspaceID, command.ReservationID, false)
		if err != nil {
			return BudgetSettlementResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return BudgetSettlementResult{}, fmt.Errorf("commit unresolved governance budget settlement: %w", err)
		}
		return BudgetSettlementResult{Reservation: reservation}, nil
	}
	settledAmount := reservation.TotalCapMicroUsd
	if command.UsageKnown {
		settledAmount = command.UsageMicroUSD
	}
	if settledAmount < reservation.DebitedMicroUsd {
		return BudgetSettlementResult{}, ErrBudgetInvariant
	}
	additionalSpend := settledAmount - reservation.DebitedMicroUsd
	remaining := reservation.RemainingMicroUsd
	if err := adjustBudgetCounters(ctx, tx, command.WorkspaceID, reservation.BudgetRootID, reservation.WindowStart.Time, -remaining, additionalSpend, -remaining, additionalSpend); err != nil {
		return BudgetSettlementResult{}, err
	}
	_, err = tx.Exec(ctx, `
		UPDATE governance_budget_reservation
		SET remaining_micro_usd = 0, debited_micro_usd = $3, settled_micro_usd = $3,
		    state = 'settled', revision = $4, settlement_receipt_id = $5,
		    usage_known = $6, termination_known = TRUE, settled_at = $7, updated_at = now()
		WHERE workspace_id = $1 AND reservation_id = $2 AND state = 'reserved'
	`, command.WorkspaceID, command.ReservationID, settledAmount, reservation.Revision+1,
		command.ReceiptID, command.UsageKnown, service.clock.Now().UTC())
	if err != nil {
		return BudgetSettlementResult{}, err
	}
	if err := insertBudgetJournal(ctx, tx, command.WorkspaceID, command.ReservationID, command.EventKey, "settle", digest, reservation.Revision, reservation.Revision+1); err != nil {
		return BudgetSettlementResult{}, err
	}
	if err := insertBudgetOutbox(ctx, tx, command.WorkspaceID, command.ReservationID, command.EventKey, "settled", reservation.CaseID, reservation.AttemptID, reservation.ObligationID); err != nil {
		return BudgetSettlementResult{}, err
	}
	if released, err := guards.Release(ctx, reservation.Resource, command.ReservationID); err != nil {
		return BudgetSettlementResult{}, err
	} else if !released {
		return BudgetSettlementResult{}, ErrBudgetInvariant
	}
	reservation, err = readBudgetReservation(ctx, tx, command.WorkspaceID, command.ReservationID, false)
	if err != nil {
		return BudgetSettlementResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return BudgetSettlementResult{}, fmt.Errorf("commit governance budget settlement: %w", err)
	}
	return BudgetSettlementResult{Reservation: reservation, Finalized: true}, nil
}

func validBudgetReserveCommand(command BudgetReserveCommand, windowStart, windowEnd time.Time) bool {
	return validConcurrencyUUID(command.WorkspaceID) && validConcurrencyUUID(command.ReservationID) &&
		validConcurrencyUUID(command.BudgetRootID) && validConcurrencyUUID(command.CaseID) &&
		validConcurrencyUUID(command.AttemptID) && validConcurrencyUUID(command.ObligationID) &&
		command.Resource != "" && command.ControlEpoch >= 0 && command.RootCapMicroUSD > 0 &&
		command.WindowCapMicroUSD > 0 && command.MaxAttemptCostMicroUSD > 0 && command.RetryAllowance >= 0 &&
		command.RetryPolicyBounded && command.SlotLimit >= 0 && validBudgetKey(command.EventKey) &&
		windowStart.Before(windowEnd)
}

func budgetTotalCap(maxAttemptCost, retryAllowance int64) (int64, error) {
	if maxAttemptCost <= 0 || retryAllowance < 0 {
		return 0, ErrBudgetInput
	}
	if retryAllowance == math.MaxInt64 || maxAttemptCost > math.MaxInt64/(retryAllowance+1) {
		return 0, ErrBudgetOverflow
	}
	return maxAttemptCost * (retryAllowance + 1), nil
}
