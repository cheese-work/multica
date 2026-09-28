package governance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	ErrBudgetInput               = errors.New("invalid governance budget input")
	ErrBudgetControl             = errors.New("governance budget control is stale or disabled")
	ErrBudgetLimit               = errors.New("governance budget limit reached")
	ErrBudgetWindowClosed        = errors.New("governance budget window is not open")
	ErrBudgetWindowConflict      = errors.New("governance budget window identity conflicts")
	ErrBudgetUnboundedRetries    = errors.New("unbounded provider retries cannot reserve budget")
	ErrBudgetIdempotencyConflict = errors.New("governance budget idempotency key conflicts")
	ErrBudgetCAS                 = errors.New("governance budget reservation revision is stale")
	ErrBudgetRetryAllowance      = errors.New("governance budget retry allowance exhausted")
	ErrBudgetLiabilityUnknown    = errors.New("governance budget liability is unresolved")
	ErrBudgetAlreadySettled      = errors.New("governance budget reservation is already settled")
	ErrBudgetUsageExceedsCap     = errors.New("reported usage exceeds the reserved cap")
	ErrBudgetInvariant           = errors.New("governance budget ledger is inconsistent")
)

type BudgetRequest struct {
	ReservationID          pgtype.UUID `json:"reservation_id"`
	BudgetRootID           pgtype.UUID `json:"budget_root_id"`
	CaseID                 pgtype.UUID `json:"case_id"`
	AttemptID              pgtype.UUID `json:"attempt_id"`
	ObligationID           pgtype.UUID `json:"obligation_id"`
	Resource               string      `json:"resource"`
	ConcurrencyLimit       int64       `json:"concurrency_limit"`
	ControlEpoch           int64       `json:"control_epoch"`
	RootCapMicroUSD        int64       `json:"root_cap_micro_usd"`
	WindowStart            time.Time   `json:"window_start"`
	WindowEnd              time.Time   `json:"window_end"`
	MaxAttemptCostMicroUSD int64       `json:"max_attempt_cost_micro_usd"`
	RetryAllowance         int64       `json:"retry_allowance"`
	RetryPolicyBounded     bool        `json:"retry_policy_bounded"`
}

type BudgetReservation struct {
	ReservationID           pgtype.UUID
	BudgetRootID            pgtype.UUID
	CaseID                  pgtype.UUID
	AttemptID               pgtype.UUID
	ObligationID            pgtype.UUID
	Resource                string
	ControlEpoch            int64
	WindowStart             time.Time
	WindowEnd               time.Time
	RootCapMicroUSD         int64
	WindowCapMicroUSD       int64
	MaxAttemptCostMicroUSD  int64
	RetryAllowance          int64
	RetryAllowanceRemaining int64
	AttemptsStarted         int64
	TotalCapMicroUSD        int64
	RemainingMicroUSD       int64
	DebitedMicroUSD         int64
	SettledMicroUSD         int64
	State                   string
	Revision                int64
	RequestDigest           string
	SettlementReceiptID     string
	UsageKnown              bool
	TerminationKnown        bool
}

type BudgetDebit struct {
	ReservationID    pgtype.UUID
	ExpectedRevision int64
	EventKey         string
}

type BudgetSettlement struct {
	ReservationID    pgtype.UUID
	ExpectedRevision int64
	EventKey         string
	ReceiptID        string
	UsageKnown       bool
	UsageMicroUSD    int64
	TerminationKnown bool
}

func ReserveBudget(ctx context.Context, tx pgx.Tx, workspaceID pgtype.UUID, request BudgetRequest, now time.Time) (BudgetReservation, bool, error) {
	if tx == nil || !validConcurrencyUUID(workspaceID) || !validBudgetRequest(request) {
		return BudgetReservation{}, false, ErrBudgetInput
	}
	digestRequest := request
	digestRequest.ReservationID = pgtype.UUID{}
	requestDigest, err := digestBudgetValue(digestRequest)
	if err != nil {
		return BudgetReservation{}, false, err
	}
	guards, err := LockConcurrencyResources(ctx, tx, workspaceID, request.Resource)
	if err != nil {
		return BudgetReservation{}, false, err
	}
	existing, loadErr := loadBudgetReservation(ctx, tx, workspaceID, request.ReservationID, false)
	if errors.Is(loadErr, pgx.ErrNoRows) {
		existing, loadErr = loadBudgetReservationByAttempt(ctx, tx, workspaceID, request.AttemptID, request.ObligationID)
	}
	if loadErr == nil {
		if existing.RequestDigest != requestDigest || existing.BudgetRootID != request.BudgetRootID {
			return BudgetReservation{}, false, ErrBudgetIdempotencyConflict
		}
		var holdState string
		err = tx.QueryRow(ctx, `
			SELECT state FROM governance_concurrency_hold
			WHERE workspace_id = $1 AND resource = $2 AND reservation_id = $3
		`, workspaceID, existing.Resource, existing.ReservationID).Scan(&holdState)
		if err != nil || (existing.State == "reserved" && holdState != "held") || (existing.State == "settled" && holdState != "released") {
			return BudgetReservation{}, false, ErrBudgetInvariant
		}
		return existing, true, nil
	} else if !errors.Is(loadErr, pgx.ErrNoRows) {
		return BudgetReservation{}, false, loadErr
	}
	if !request.RetryPolicyBounded {
		return BudgetReservation{}, false, ErrBudgetUnboundedRetries
	}
	windowSeconds := guards.settings.Limits.AdmissionWindowSeconds
	if !guards.enabled || guards.controlEpoch != request.ControlEpoch || request.ConcurrencyLimit <= 0 ||
		guards.settings.Limits.WorkspaceSpendCapMicroUSD == nil || *guards.settings.Limits.WorkspaceSpendCapMicroUSD <= 0 ||
		windowSeconds == nil || *windowSeconds <= 0 || *windowSeconds > math.MaxInt64/int64(time.Second) {
		return BudgetReservation{}, false, ErrBudgetControl
	}
	if request.WindowEnd.Sub(request.WindowStart) != time.Duration(*windowSeconds)*time.Second {
		return BudgetReservation{}, false, ErrBudgetInput
	}
	windowStartUnix := now.Unix()
	windowOffset := windowStartUnix % *windowSeconds
	if windowOffset < 0 {
		windowOffset += *windowSeconds
	}
	if !request.WindowStart.Equal(time.Unix(windowStartUnix-windowOffset, 0)) {
		return BudgetReservation{}, false, ErrBudgetInput
	}
	if now.Before(request.WindowStart) || !now.Before(request.WindowEnd) {
		return BudgetReservation{}, false, ErrBudgetWindowClosed
	}
	workspaceCap := *guards.settings.Limits.WorkspaceSpendCapMicroUSD
	totalCap := request.MaxAttemptCostMicroUSD * (request.RetryAllowance + 1)
	var overlappingLiability int64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(reserved_micro_usd + spent_micro_usd), 0)
		FROM governance_budget_window
		WHERE workspace_id = $1 AND window_start < $3 AND window_end > $2
	`, workspaceID, request.WindowStart, request.WindowEnd).Scan(&overlappingLiability); err != nil {
		return BudgetReservation{}, false, err
	}
	if !budgetCanFit(0, overlappingLiability, totalCap, workspaceCap) {
		return BudgetReservation{}, false, ErrBudgetLimit
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO governance_budget_window (workspace_id, window_start, window_end, spend_cap_micro_usd)
		VALUES ($1, $2, $3, $4) ON CONFLICT (workspace_id, window_start) DO NOTHING
	`, workspaceID, request.WindowStart, request.WindowEnd, workspaceCap); err != nil {
		return BudgetReservation{}, false, err
	}
	var windowEnd time.Time
	var windowReserved, windowSpent int64
	if err := tx.QueryRow(ctx, `
		SELECT window_end, reserved_micro_usd, spent_micro_usd FROM governance_budget_window
		WHERE workspace_id = $1 AND window_start = $2 FOR UPDATE
	`, workspaceID, request.WindowStart).Scan(&windowEnd, &windowReserved, &windowSpent); err != nil {
		return BudgetReservation{}, false, err
	}
	if !windowEnd.Equal(request.WindowEnd) {
		return BudgetReservation{}, false, ErrBudgetWindowConflict
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO governance_budget_root (workspace_id, budget_root_id, spend_cap_micro_usd)
		VALUES ($1, $2, $3) ON CONFLICT (workspace_id, budget_root_id) DO NOTHING
	`, workspaceID, request.BudgetRootID, request.RootCapMicroUSD); err != nil {
		return BudgetReservation{}, false, err
	}
	var storedRootCap, rootReserved, rootSpent int64
	if err := tx.QueryRow(ctx, `
		SELECT spend_cap_micro_usd, reserved_micro_usd, spent_micro_usd FROM governance_budget_root
		WHERE workspace_id = $1 AND budget_root_id = $2 FOR UPDATE
	`, workspaceID, request.BudgetRootID).Scan(&storedRootCap, &rootReserved, &rootSpent); err != nil {
		return BudgetReservation{}, false, err
	}
	rootCap := min(storedRootCap, request.RootCapMicroUSD)
	if !budgetCanFit(rootReserved, rootSpent, totalCap, rootCap) || !budgetCanFit(windowReserved, windowSpent, totalCap, workspaceCap) {
		return BudgetReservation{}, false, ErrBudgetLimit
	}
	duplicateSlot, err := guards.Reserve(ctx, request.Resource, request.ReservationID, request.ControlEpoch, request.ConcurrencyLimit)
	if err != nil {
		return BudgetReservation{}, false, err
	}
	if duplicateSlot {
		return BudgetReservation{}, false, ErrBudgetInvariant
	}
	if _, err := tx.Exec(ctx, `
		UPDATE governance_budget_window SET spend_cap_micro_usd = $3,
		    reserved_micro_usd = reserved_micro_usd + $4, updated_at = now()
		WHERE workspace_id = $1 AND window_start = $2
	`, workspaceID, request.WindowStart, workspaceCap, totalCap); err != nil {
		return BudgetReservation{}, false, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE governance_budget_root SET spend_cap_micro_usd = $3,
		    reserved_micro_usd = reserved_micro_usd + $4, updated_at = now()
		WHERE workspace_id = $1 AND budget_root_id = $2
	`, workspaceID, request.BudgetRootID, rootCap, totalCap); err != nil {
		return BudgetReservation{}, false, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO governance_budget_reservation (
		    workspace_id, reservation_id, budget_root_id, case_id, attempt_id, obligation_id,
		    resource, control_epoch, window_start, window_end, root_cap_micro_usd,
		    window_cap_micro_usd, max_attempt_cost_micro_usd, retry_allowance,
		    retry_policy_bounded, retry_allowance_remaining, total_cap_micro_usd, remaining_micro_usd, request_digest
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$14,$16,$16,$17)
	`, workspaceID, request.ReservationID, request.BudgetRootID, request.CaseID, request.AttemptID,
		request.ObligationID, request.Resource, request.ControlEpoch, request.WindowStart, request.WindowEnd,
		rootCap, workspaceCap, request.MaxAttemptCostMicroUSD, request.RetryAllowance,
		request.RetryPolicyBounded, totalCap, requestDigest); err != nil {
		return BudgetReservation{}, false, err
	}
	if err := insertBudgetJournal(ctx, tx, workspaceID, request.ReservationID, "reserve", "reserve", requestDigest, 0, 1); err != nil {
		return BudgetReservation{}, false, err
	}
	payload, err := json.Marshal(struct {
		ReservationID string    `json:"reservation_id"`
		BudgetRootID  string    `json:"budget_root_id"`
		AttemptID     string    `json:"attempt_id"`
		ObligationID  string    `json:"obligation_id"`
		WindowEnd     time.Time `json:"window_end"`
		CapMicroUSD   int64     `json:"cap_micro_usd"`
		Retries       int64     `json:"retry_allowance"`
	}{uuidText(request.ReservationID), uuidText(request.BudgetRootID), uuidText(request.AttemptID),
		uuidText(request.ObligationID), request.WindowEnd, totalCap, request.RetryAllowance})
	if err != nil {
		return BudgetReservation{}, false, err
	}
	if err := insertBudgetOutbox(ctx, tx, workspaceID, request.ReservationID, "admit/"+uuidText(request.ReservationID), "admit", request.CaseID, request.AttemptID, request.ObligationID, payload); err != nil {
		return BudgetReservation{}, false, err
	}
	reservation, err := loadBudgetReservation(ctx, tx, workspaceID, request.ReservationID, false)
	return reservation, false, err
}

func DebitBudget(ctx context.Context, tx pgx.Tx, workspaceID pgtype.UUID, debit BudgetDebit, now time.Time) (BudgetReservation, bool, error) {
	if tx == nil || !validConcurrencyUUID(workspaceID) || !validConcurrencyUUID(debit.ReservationID) || debit.ExpectedRevision <= 0 || !validBudgetKey(debit.EventKey) {
		return BudgetReservation{}, false, ErrBudgetInput
	}
	guards, reservation, err := lockBudgetReservation(ctx, tx, workspaceID, debit.ReservationID)
	if err != nil {
		return BudgetReservation{}, false, err
	}
	digest, err := digestBudgetValue(struct {
		Type     string `json:"type"`
		EventKey string `json:"event_key"`
	}{"debit", debit.EventKey})
	if err != nil {
		return BudgetReservation{}, false, err
	}
	duplicate, err := budgetEventExists(ctx, tx, workspaceID, debit.ReservationID, debit.EventKey, digest)
	if err != nil || duplicate {
		return reservation, duplicate, err
	}
	if reservation.State != "reserved" {
		return BudgetReservation{}, false, ErrBudgetAlreadySettled
	}
	if !guards.enabled || guards.controlEpoch != reservation.ControlEpoch {
		return BudgetReservation{}, false, ErrBudgetControl
	}
	if now.Before(reservation.WindowStart) || !now.Before(reservation.WindowEnd) {
		return BudgetReservation{}, false, ErrBudgetWindowClosed
	}
	if reservation.Revision != debit.ExpectedRevision {
		return BudgetReservation{}, false, ErrBudgetCAS
	}
	if reservation.AttemptsStarted > 0 && reservation.RetryAllowanceRemaining == 0 {
		return BudgetReservation{}, false, ErrBudgetRetryAllowance
	}
	if reservation.RemainingMicroUSD < reservation.MaxAttemptCostMicroUSD {
		return BudgetReservation{}, false, ErrBudgetInvariant
	}
	if err := transferBudgetAmount(ctx, tx, workspaceID, reservation, reservation.MaxAttemptCostMicroUSD); err != nil {
		return BudgetReservation{}, false, err
	}
	remainingRetries := reservation.RetryAllowanceRemaining
	if reservation.AttemptsStarted > 0 {
		remainingRetries--
	}
	result, err := tx.Exec(ctx, `
		UPDATE governance_budget_reservation SET
		    retry_allowance_remaining = $4, attempts_started = attempts_started + 1,
		    remaining_micro_usd = remaining_micro_usd - $5,
		    debited_micro_usd = debited_micro_usd + $5,
		    revision = revision + 1, updated_at = now()
		WHERE workspace_id = $1 AND reservation_id = $2 AND revision = $3 AND state = 'reserved'
	`, workspaceID, debit.ReservationID, debit.ExpectedRevision, remainingRetries, reservation.MaxAttemptCostMicroUSD)
	if err != nil {
		return BudgetReservation{}, false, err
	}
	if result.RowsAffected() != 1 {
		return BudgetReservation{}, false, ErrBudgetCAS
	}
	if err := insertBudgetJournal(ctx, tx, workspaceID, debit.ReservationID, debit.EventKey, "debit", digest, debit.ExpectedRevision, debit.ExpectedRevision+1); err != nil {
		return BudgetReservation{}, false, err
	}
	reservation, err = loadBudgetReservation(ctx, tx, workspaceID, debit.ReservationID, false)
	return reservation, false, err
}

func SettleBudget(ctx context.Context, tx pgx.Tx, workspaceID pgtype.UUID, settlement BudgetSettlement, now time.Time) (BudgetReservation, bool, error) {
	if tx == nil || !validConcurrencyUUID(workspaceID) || !validConcurrencyUUID(settlement.ReservationID) ||
		settlement.ExpectedRevision <= 0 || !validBudgetKey(settlement.EventKey) || !validBudgetKey(settlement.ReceiptID) || settlement.UsageMicroUSD < 0 {
		return BudgetReservation{}, false, ErrBudgetInput
	}
	guards, reservation, err := lockBudgetReservation(ctx, tx, workspaceID, settlement.ReservationID)
	if err != nil {
		return BudgetReservation{}, false, err
	}
	digest, err := digestBudgetValue(struct {
		Type             string `json:"type"`
		EventKey         string `json:"event_key"`
		ReceiptID        string `json:"receipt_id"`
		UsageKnown       bool   `json:"usage_known"`
		UsageMicroUSD    int64  `json:"usage_micro_usd"`
		TerminationKnown bool   `json:"termination_known"`
	}{"settle", settlement.EventKey, settlement.ReceiptID, settlement.UsageKnown, settlement.UsageMicroUSD, settlement.TerminationKnown})
	if err != nil {
		return BudgetReservation{}, false, err
	}
	duplicate, err := budgetEventExists(ctx, tx, workspaceID, settlement.ReservationID, settlement.EventKey, digest)
	if err != nil || duplicate {
		return reservation, duplicate, err
	}
	if reservation.Revision != settlement.ExpectedRevision {
		return BudgetReservation{}, false, ErrBudgetCAS
	}
	if reservation.State != "reserved" {
		return BudgetReservation{}, false, ErrBudgetAlreadySettled
	}
	if !settlement.TerminationKnown {
		return BudgetReservation{}, false, ErrBudgetLiabilityUnknown
	}
	settledAmount := reservation.TotalCapMicroUSD
	if settlement.UsageKnown {
		settledAmount = settlement.UsageMicroUSD
		if settledAmount > reservation.TotalCapMicroUSD {
			return BudgetReservation{}, false, ErrBudgetUsageExceedsCap
		}
	}
	if reservation.RemainingMicroUSD+reservation.DebitedMicroUSD != reservation.TotalCapMicroUSD {
		return BudgetReservation{}, false, ErrBudgetInvariant
	}
	spentDelta := settledAmount - reservation.DebitedMicroUSD
	if spentDelta > reservation.RemainingMicroUSD || spentDelta < -reservation.DebitedMicroUSD {
		return BudgetReservation{}, false, ErrBudgetInvariant
	}
	if err := settleBudgetAmount(ctx, tx, workspaceID, reservation, spentDelta); err != nil {
		return BudgetReservation{}, false, err
	}
	result, err := tx.Exec(ctx, `
		UPDATE governance_budget_reservation SET
		    remaining_micro_usd = 0, debited_micro_usd = $4, settled_micro_usd = $4, state = 'settled',
		    settlement_receipt_id = $5, usage_known = $6, termination_known = true,
		    revision = revision + 1, updated_at = now(), settled_at = $7
		WHERE workspace_id = $1 AND reservation_id = $2 AND revision = $3 AND state = 'reserved'
	`, workspaceID, settlement.ReservationID, settlement.ExpectedRevision, settledAmount,
		settlement.ReceiptID, settlement.UsageKnown, now)
	if err != nil {
		return BudgetReservation{}, false, normalizeBudgetUniqueError(err)
	}
	if result.RowsAffected() != 1 {
		return BudgetReservation{}, false, ErrBudgetCAS
	}
	if released, err := guards.Release(ctx, reservation.Resource, settlement.ReservationID); err != nil {
		return BudgetReservation{}, false, err
	} else if !released {
		return BudgetReservation{}, false, ErrBudgetInvariant
	}
	if err := insertBudgetJournal(ctx, tx, workspaceID, settlement.ReservationID, settlement.EventKey, "settle", digest, settlement.ExpectedRevision, settlement.ExpectedRevision+1); err != nil {
		return BudgetReservation{}, false, err
	}
	payload, err := json.Marshal(struct {
		ReceiptID        string `json:"receipt_id"`
		UsageKnown       bool   `json:"usage_known"`
		UsageMicroUSD    int64  `json:"usage_micro_usd"`
		TerminationKnown bool   `json:"termination_known"`
	}{settlement.ReceiptID, settlement.UsageKnown, settledAmount, true})
	if err != nil {
		return BudgetReservation{}, false, err
	}
	if err := insertBudgetOutbox(ctx, tx, workspaceID, settlement.ReservationID,
		"settled/"+uuidText(settlement.ReservationID), "settled", reservation.CaseID, reservation.AttemptID,
		reservation.ObligationID, payload); err != nil {
		return BudgetReservation{}, false, err
	}
	reservation, err = loadBudgetReservation(ctx, tx, workspaceID, settlement.ReservationID, false)
	return reservation, false, err
}

func validBudgetRequest(request BudgetRequest) bool {
	return validConcurrencyUUID(request.ReservationID) && validConcurrencyUUID(request.BudgetRootID) &&
		validConcurrencyUUID(request.CaseID) && validConcurrencyUUID(request.AttemptID) &&
		validConcurrencyUUID(request.ObligationID) && request.Resource != "" && request.ConcurrencyLimit >= 0 &&
		request.ControlEpoch >= 0 && request.RootCapMicroUSD > 0 && request.MaxAttemptCostMicroUSD > 0 &&
		request.RetryAllowance >= 0 && request.RetryAllowance < math.MaxInt64 &&
		request.MaxAttemptCostMicroUSD <= math.MaxInt64/(request.RetryAllowance+1) &&
		request.WindowStart.Before(request.WindowEnd)
}

func budgetCanFit(reserved, spent, amount, cap int64) bool {
	if reserved < 0 || spent < 0 || amount < 0 || cap <= 0 || reserved > cap {
		return false
	}
	available := cap - reserved
	if spent > available {
		return false
	}
	return amount <= available-spent
}

func loadBudgetReservation(ctx context.Context, tx pgx.Tx, workspaceID, reservationID pgtype.UUID, lock bool) (BudgetReservation, error) {
	query := `
		SELECT reservation_id, budget_root_id, case_id, attempt_id, obligation_id, resource,
		    control_epoch, window_start, window_end, root_cap_micro_usd, window_cap_micro_usd,
		    max_attempt_cost_micro_usd, retry_allowance, retry_allowance_remaining, attempts_started,
		    total_cap_micro_usd, remaining_micro_usd, debited_micro_usd, settled_micro_usd,
		    state, revision, request_digest, COALESCE(settlement_receipt_id, ''), usage_known, termination_known
		FROM governance_budget_reservation
		WHERE workspace_id = $1 AND reservation_id = $2`
	if lock {
		query += " FOR UPDATE"
	}
	var reservation BudgetReservation
	err := tx.QueryRow(ctx, query, workspaceID, reservationID).Scan(
		&reservation.ReservationID, &reservation.BudgetRootID, &reservation.CaseID, &reservation.AttemptID,
		&reservation.ObligationID, &reservation.Resource, &reservation.ControlEpoch, &reservation.WindowStart,
		&reservation.WindowEnd, &reservation.RootCapMicroUSD, &reservation.WindowCapMicroUSD,
		&reservation.MaxAttemptCostMicroUSD, &reservation.RetryAllowance, &reservation.RetryAllowanceRemaining,
		&reservation.AttemptsStarted, &reservation.TotalCapMicroUSD, &reservation.RemainingMicroUSD,
		&reservation.DebitedMicroUSD, &reservation.SettledMicroUSD, &reservation.State, &reservation.Revision,
		&reservation.RequestDigest, &reservation.SettlementReceiptID, &reservation.UsageKnown, &reservation.TerminationKnown)
	return reservation, err
}

func loadBudgetReservationByAttempt(ctx context.Context, tx pgx.Tx, workspaceID, attemptID, obligationID pgtype.UUID) (BudgetReservation, error) {
	var reservationID pgtype.UUID
	err := tx.QueryRow(ctx, `
		SELECT reservation_id FROM governance_budget_reservation
		WHERE workspace_id = $1 AND attempt_id = $2 AND obligation_id = $3
	`, workspaceID, attemptID, obligationID).Scan(&reservationID)
	if err != nil {
		return BudgetReservation{}, err
	}
	return loadBudgetReservation(ctx, tx, workspaceID, reservationID, false)
}

func lockBudgetReservation(ctx context.Context, tx pgx.Tx, workspaceID, reservationID pgtype.UUID) (*ConcurrencyGuards, BudgetReservation, error) {
	var resource string
	var rootID pgtype.UUID
	var windowStart time.Time
	if err := tx.QueryRow(ctx, `
		SELECT resource, budget_root_id, window_start FROM governance_budget_reservation
		WHERE workspace_id = $1 AND reservation_id = $2
	`, workspaceID, reservationID).Scan(&resource, &rootID, &windowStart); err != nil {
		return nil, BudgetReservation{}, err
	}
	guards, err := LockConcurrencyResources(ctx, tx, workspaceID, resource)
	if err != nil {
		return nil, BudgetReservation{}, err
	}
	var ignored int64
	if err := tx.QueryRow(ctx, `
		SELECT reserved_micro_usd FROM governance_budget_window
		WHERE workspace_id = $1 AND window_start = $2 FOR UPDATE
	`, workspaceID, windowStart).Scan(&ignored); err != nil {
		return nil, BudgetReservation{}, ErrBudgetInvariant
	}
	if err := tx.QueryRow(ctx, `
		SELECT reserved_micro_usd FROM governance_budget_root
		WHERE workspace_id = $1 AND budget_root_id = $2 FOR UPDATE
	`, workspaceID, rootID).Scan(&ignored); err != nil {
		return nil, BudgetReservation{}, ErrBudgetInvariant
	}
	reservation, err := loadBudgetReservation(ctx, tx, workspaceID, reservationID, true)
	if err != nil {
		return nil, BudgetReservation{}, err
	}
	return guards, reservation, nil
}

func transferBudgetAmount(ctx context.Context, tx pgx.Tx, workspaceID pgtype.UUID, reservation BudgetReservation, amount int64) error {
	window, err := tx.Exec(ctx, `
		UPDATE governance_budget_window SET reserved_micro_usd = reserved_micro_usd - $3,
		    spent_micro_usd = spent_micro_usd + $3, updated_at = now()
		WHERE workspace_id = $1 AND window_start = $2 AND reserved_micro_usd >= $3
	`, workspaceID, reservation.WindowStart, amount)
	if err != nil || window.RowsAffected() != 1 {
		return ErrBudgetInvariant
	}
	root, err := tx.Exec(ctx, `
		UPDATE governance_budget_root SET reserved_micro_usd = reserved_micro_usd - $3,
		    spent_micro_usd = spent_micro_usd + $3, updated_at = now()
		WHERE workspace_id = $1 AND budget_root_id = $2 AND reserved_micro_usd >= $3
	`, workspaceID, reservation.BudgetRootID, amount)
	if err != nil || root.RowsAffected() != 1 {
		return ErrBudgetInvariant
	}
	return nil
}

func settleBudgetAmount(ctx context.Context, tx pgx.Tx, workspaceID pgtype.UUID, reservation BudgetReservation, spentDelta int64) error {
	window, err := tx.Exec(ctx, `
		UPDATE governance_budget_window SET reserved_micro_usd = reserved_micro_usd - $3,
		    spent_micro_usd = spent_micro_usd + $4, updated_at = now()
		WHERE workspace_id = $1 AND window_start = $2 AND reserved_micro_usd >= $3
		  AND spent_micro_usd + $4 >= 0
	`, workspaceID, reservation.WindowStart, reservation.RemainingMicroUSD, spentDelta)
	if err != nil || window.RowsAffected() != 1 {
		return ErrBudgetInvariant
	}
	root, err := tx.Exec(ctx, `
		UPDATE governance_budget_root SET reserved_micro_usd = reserved_micro_usd - $3,
		    spent_micro_usd = spent_micro_usd + $4, updated_at = now()
		WHERE workspace_id = $1 AND budget_root_id = $2 AND reserved_micro_usd >= $3
		  AND spent_micro_usd + $4 >= 0
	`, workspaceID, reservation.BudgetRootID, reservation.RemainingMicroUSD, spentDelta)
	if err != nil || root.RowsAffected() != 1 {
		return ErrBudgetInvariant
	}
	return nil
}

func budgetEventExists(ctx context.Context, tx pgx.Tx, workspaceID, reservationID pgtype.UUID, eventKey, digest string) (bool, error) {
	var storedDigest string
	err := tx.QueryRow(ctx, `
		SELECT event_digest FROM governance_budget_journal
		WHERE workspace_id = $1 AND reservation_id = $2 AND event_key = $3
	`, workspaceID, reservationID, eventKey).Scan(&storedDigest)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if storedDigest != digest {
		return false, ErrBudgetIdempotencyConflict
	}
	return true, nil
}

func insertBudgetJournal(ctx context.Context, tx pgx.Tx, workspaceID, reservationID pgtype.UUID, eventKey, eventType, digest string, expectedRevision, resultingRevision int64) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO governance_budget_journal (
		    workspace_id, reservation_id, event_key, event_type, event_digest,
		    expected_revision, resulting_revision
		) VALUES ($1,$2,$3,$4,$5,$6,$7)
	`, workspaceID, reservationID, eventKey, eventType, digest, expectedRevision, resultingRevision)
	return normalizeBudgetUniqueError(err)
}

func insertBudgetOutbox(ctx context.Context, tx pgx.Tx, workspaceID, reservationID pgtype.UUID, eventKey, eventType string, caseID, attemptID, obligationID pgtype.UUID, payload []byte) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO governance_budget_outbox (
		    workspace_id, reservation_id, event_key, event_type, case_id, attempt_id, obligation_id, payload
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
	`, workspaceID, reservationID, eventKey, eventType, caseID, attemptID, obligationID, payload)
	return normalizeBudgetUniqueError(err)
}

func digestBudgetValue(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func validBudgetKey(value string) bool {
	return value != "" && len(value) <= 200 && strings.TrimSpace(value) == value && !strings.ContainsRune(value, '\x00')
}

func uuidText(value pgtype.UUID) string {
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", value.Bytes[0:4], value.Bytes[4:6], value.Bytes[6:8], value.Bytes[8:10], value.Bytes[10:16])
}

func normalizeBudgetUniqueError(err error) error {
	if err == nil {
		return nil
	}
	var pgError *pgconn.PgError
	if errors.As(err, &pgError) && pgError.Code == "23505" {
		return ErrBudgetIdempotencyConflict
	}
	return err
}
