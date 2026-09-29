package governance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const budgetReservationColumns = `
	workspace_id, reservation_id, budget_root_id, case_id, attempt_id, obligation_id,
	resource, control_epoch, window_start, window_end, root_cap_micro_usd,
	window_cap_micro_usd, max_attempt_cost_micro_usd, retry_allowance,
	retry_policy_bounded, retry_allowance_remaining, attempts_started,
	total_cap_micro_usd, remaining_micro_usd, debited_micro_usd, settled_micro_usd,
	state, revision, request_digest, settlement_receipt_id, usage_known,
	termination_known, created_at, updated_at, settled_at`

type budgetRootState struct {
	cap      int64
	reserved int64
	spent    int64
}

type budgetWindowState struct {
	start    time.Time
	end      time.Time
	cap      int64
	reserved int64
	spent    int64
}

type budgetCounters struct {
	reserved int64
	spent    int64
}

type budgetState struct {
	roots        map[pgtype.UUID]budgetRootState
	windows      map[int64]budgetWindowState
	reservations map[pgtype.UUID]db.GovernanceBudgetReservation
}

type budgetLocation struct {
	resource    string
	rootID      pgtype.UUID
	windowStart time.Time
}

func readBudgetReservation(ctx context.Context, tx pgx.Tx, workspaceID, reservationID pgtype.UUID, forUpdate bool) (db.GovernanceBudgetReservation, error) {
	query := "SELECT " + budgetReservationColumns + " FROM governance_budget_reservation WHERE workspace_id = $1 AND reservation_id = $2"
	if forUpdate {
		query += " FOR UPDATE"
	}
	return scanBudgetReservation(tx.QueryRow(ctx, query, workspaceID, reservationID))
}

func readBudgetReservationByAttempt(ctx context.Context, tx pgx.Tx, workspaceID, attemptID, obligationID pgtype.UUID) (db.GovernanceBudgetReservation, error) {
	query := "SELECT " + budgetReservationColumns + `
		FROM governance_budget_reservation
		WHERE workspace_id = $1 AND attempt_id = $2 AND obligation_id = $3`
	return scanBudgetReservation(tx.QueryRow(ctx, query, workspaceID, attemptID, obligationID))
}

func scanBudgetReservation(row pgx.Row) (db.GovernanceBudgetReservation, error) {
	var reservation db.GovernanceBudgetReservation
	err := row.Scan(
		&reservation.WorkspaceID,
		&reservation.ReservationID,
		&reservation.BudgetRootID,
		&reservation.CaseID,
		&reservation.AttemptID,
		&reservation.ObligationID,
		&reservation.Resource,
		&reservation.ControlEpoch,
		&reservation.WindowStart,
		&reservation.WindowEnd,
		&reservation.RootCapMicroUsd,
		&reservation.WindowCapMicroUsd,
		&reservation.MaxAttemptCostMicroUsd,
		&reservation.RetryAllowance,
		&reservation.RetryPolicyBounded,
		&reservation.RetryAllowanceRemaining,
		&reservation.AttemptsStarted,
		&reservation.TotalCapMicroUsd,
		&reservation.RemainingMicroUsd,
		&reservation.DebitedMicroUsd,
		&reservation.SettledMicroUsd,
		&reservation.State,
		&reservation.Revision,
		&reservation.RequestDigest,
		&reservation.SettlementReceiptID,
		&reservation.UsageKnown,
		&reservation.TerminationKnown,
		&reservation.CreatedAt,
		&reservation.UpdatedAt,
		&reservation.SettledAt,
	)
	return reservation, err
}

func readBudgetLocation(ctx context.Context, tx pgx.Tx, workspaceID, reservationID pgtype.UUID) (budgetLocation, error) {
	var location budgetLocation
	var windowStart pgtype.Timestamptz
	err := tx.QueryRow(ctx, `
		SELECT resource, budget_root_id, window_start
		FROM governance_budget_reservation
		WHERE workspace_id = $1 AND reservation_id = $2
	`, workspaceID, reservationID).Scan(&location.resource, &location.rootID, &windowStart)
	if errors.Is(err, pgx.ErrNoRows) {
		return budgetLocation{}, ErrBudgetNotFound
	}
	if err != nil {
		return budgetLocation{}, err
	}
	if !windowStart.Valid || location.resource == "" || !validConcurrencyUUID(location.rootID) {
		return budgetLocation{}, ErrBudgetInvariant
	}
	location.windowStart = windowStart.Time.UTC()
	return location, nil
}

func loadBudgetState(ctx context.Context, tx pgx.Tx, workspaceID pgtype.UUID) (budgetState, error) {
	state := budgetState{
		roots:        make(map[pgtype.UUID]budgetRootState),
		windows:      make(map[int64]budgetWindowState),
		reservations: make(map[pgtype.UUID]db.GovernanceBudgetReservation),
	}
	rootRows, err := tx.Query(ctx, `
		SELECT budget_root_id, spend_cap_micro_usd, reserved_micro_usd, spent_micro_usd
		FROM governance_budget_root
		WHERE workspace_id = $1
		ORDER BY budget_root_id
		FOR UPDATE
	`, workspaceID)
	if err != nil {
		return budgetState{}, err
	}
	for rootRows.Next() {
		var rootID pgtype.UUID
		var root budgetRootState
		if err := rootRows.Scan(&rootID, &root.cap, &root.reserved, &root.spent); err != nil {
			rootRows.Close()
			return budgetState{}, err
		}
		if !validConcurrencyUUID(rootID) || root.cap <= 0 || root.reserved < 0 || root.spent < 0 {
			rootRows.Close()
			return budgetState{}, ErrBudgetInvariant
		}
		state.roots[rootID] = root
	}
	if err := rootRows.Err(); err != nil {
		rootRows.Close()
		return budgetState{}, err
	}
	rootRows.Close()

	windowRows, err := tx.Query(ctx, `
		SELECT window_start, window_end, spend_cap_micro_usd, reserved_micro_usd, spent_micro_usd
		FROM governance_budget_window
		WHERE workspace_id = $1
		ORDER BY window_start
		FOR UPDATE
	`, workspaceID)
	if err != nil {
		return budgetState{}, err
	}
	for windowRows.Next() {
		var window budgetWindowState
		if err := windowRows.Scan(&window.start, &window.end, &window.cap, &window.reserved, &window.spent); err != nil {
			windowRows.Close()
			return budgetState{}, err
		}
		window.start = window.start.UTC()
		window.end = window.end.UTC()
		if !window.start.Before(window.end) || window.cap <= 0 || window.reserved < 0 || window.spent < 0 {
			windowRows.Close()
			return budgetState{}, ErrBudgetInvariant
		}
		state.windows[budgetWindowKey(window.start)] = window
	}
	if err := windowRows.Err(); err != nil {
		windowRows.Close()
		return budgetState{}, err
	}
	windowRows.Close()

	reservationRows, err := tx.Query(ctx, "SELECT "+budgetReservationColumns+` FROM governance_budget_reservation
		WHERE workspace_id = $1
		ORDER BY reservation_id
		FOR UPDATE`, workspaceID)
	if err != nil {
		return budgetState{}, err
	}
	for reservationRows.Next() {
		reservation, err := scanBudgetReservation(reservationRows)
		if err != nil {
			reservationRows.Close()
			return budgetState{}, err
		}
		if !validConcurrencyUUID(reservation.ReservationID) {
			reservationRows.Close()
			return budgetState{}, ErrBudgetInvariant
		}
		state.reservations[reservation.ReservationID] = reservation
	}
	if err := reservationRows.Err(); err != nil {
		reservationRows.Close()
		return budgetState{}, err
	}
	reservationRows.Close()
	if err := validateBudgetState(state); err != nil {
		return budgetState{}, err
	}
	return state, nil
}

func validateBudgetState(state budgetState) error {
	rootTotals := make(map[pgtype.UUID]budgetCounters)
	windowTotals := make(map[int64]budgetCounters)
	for _, reservation := range state.reservations {
		if !validConcurrencyUUID(reservation.WorkspaceID) || !validConcurrencyUUID(reservation.BudgetRootID) ||
			!validConcurrencyUUID(reservation.CaseID) || !validConcurrencyUUID(reservation.AttemptID) ||
			!validConcurrencyUUID(reservation.ObligationID) || !reservation.WindowStart.Valid || !reservation.WindowEnd.Valid ||
			!reservation.WindowStart.Time.Before(reservation.WindowEnd.Time) || reservation.TotalCapMicroUsd <= 0 ||
			reservation.RootCapMicroUsd <= 0 || reservation.WindowCapMicroUsd <= 0 || reservation.MaxAttemptCostMicroUsd <= 0 ||
			!reservation.RetryPolicyBounded || reservation.RetryAllowance < 0 || reservation.RetryAllowanceRemaining < 0 ||
			reservation.RetryAllowanceRemaining > reservation.RetryAllowance || reservation.AttemptsStarted < 1 ||
			reservation.AttemptsStarted > reservation.RetryAllowance+1 || reservation.RemainingMicroUsd < 0 ||
			reservation.DebitedMicroUsd < 0 || reservation.SettledMicroUsd < 0 || reservation.Revision < 1 ||
			!validBudgetDigest(reservation.RequestDigest) || !validBudgetKey(reservation.Resource) {
			return ErrBudgetInvariant
		}
		totalParts, err := budgetAddAmounts(reservation.RemainingMicroUsd, reservation.DebitedMicroUsd)
		if err != nil || (reservation.State == "reserved" && totalParts != reservation.TotalCapMicroUsd) {
			return ErrBudgetInvariant
		}
		root, ok := state.roots[reservation.BudgetRootID]
		if !ok || reservation.RootCapMicroUsd > root.cap {
			return ErrBudgetInvariant
		}
		window, ok := state.windows[budgetWindowKey(reservation.WindowStart.Time)]
		if !ok || !window.end.Equal(reservation.WindowEnd.Time) || reservation.WindowCapMicroUsd > window.cap {
			return ErrBudgetInvariant
		}
		rootTotal := rootTotals[reservation.BudgetRootID]
		windowKey := budgetWindowKey(reservation.WindowStart.Time)
		windowTotal := windowTotals[windowKey]
		switch reservation.State {
		case "reserved":
			rootTotal.reserved, err = budgetAddAmounts(rootTotal.reserved, reservation.RemainingMicroUsd)
			if err == nil {
				rootTotal.spent, err = budgetAddAmounts(rootTotal.spent, reservation.DebitedMicroUsd)
			}
			if err == nil {
				windowTotal.reserved, err = budgetAddAmounts(windowTotal.reserved, reservation.RemainingMicroUsd)
			}
			if err == nil {
				windowTotal.spent, err = budgetAddAmounts(windowTotal.spent, reservation.DebitedMicroUsd)
			}
		case "settled":
			if reservation.RemainingMicroUsd != 0 || reservation.DebitedMicroUsd != reservation.SettledMicroUsd ||
				reservation.SettledMicroUsd > reservation.TotalCapMicroUsd || !reservation.TerminationKnown ||
				!reservation.SettlementReceiptID.Valid || !reservation.SettledAt.Valid ||
				(!reservation.UsageKnown && reservation.SettledMicroUsd != reservation.TotalCapMicroUsd) {
				return ErrBudgetInvariant
			}
			rootTotal.spent, err = budgetAddAmounts(rootTotal.spent, reservation.SettledMicroUsd)
			if err == nil {
				windowTotal.spent, err = budgetAddAmounts(windowTotal.spent, reservation.SettledMicroUsd)
			}
		default:
			return ErrBudgetInvariant
		}
		if err != nil {
			return err
		}
		rootTotals[reservation.BudgetRootID] = rootTotal
		windowTotals[windowKey] = windowTotal
	}
	for rootID, root := range state.roots {
		if rootTotals[rootID].reserved != root.reserved || rootTotals[rootID].spent != root.spent {
			return ErrBudgetInvariant
		}
	}
	for windowKey, window := range state.windows {
		if windowTotals[windowKey].reserved != window.reserved || windowTotals[windowKey].spent != window.spent {
			return ErrBudgetInvariant
		}
	}
	return nil
}

func checkBudgetAdmission(state budgetState, command BudgetReserveCommand, totalCap int64, now time.Time, root budgetRootState, incoming budgetWindowState) error {
	rootExposure, err := budgetAddAmounts(root.reserved, root.spent)
	if err != nil {
		return err
	}
	rootCap := minBudgetCap(root.cap, command.RootCapMicroUSD)
	if rootExposure > rootCap || totalCap > rootCap-rootExposure {
		return ErrBudgetLimit
	}
	for _, boundary := range state.windows {
		protected := !now.Before(boundary.start) && boundary.end.After(now)
		for _, reservation := range state.reservations {
			if reservation.State == "reserved" && budgetWindowKey(reservation.WindowStart.Time) == budgetWindowKey(boundary.start) {
				protected = true
				break
			}
		}
		if !protected {
			continue
		}
		capMicroUSD := minBudgetCap(boundary.cap, command.WindowCapMicroUSD)
		if budgetWindowKey(boundary.start) == budgetWindowKey(incoming.start) {
			capMicroUSD = minBudgetCap(capMicroUSD, incoming.cap)
		}
		var exposure int64
		for _, reservation := range state.reservations {
			if reservation.State != "reserved" && !budgetIntervalsOverlap(reservation.WindowStart.Time, reservation.WindowEnd.Time, boundary.start, boundary.end) {
				continue
			}
			charge := budgetReservationExposure(reservation)
			exposure, err = budgetAddAmounts(exposure, charge)
			if err != nil {
				return err
			}
		}
		if exposure > capMicroUSD || totalCap > capMicroUSD-exposure {
			return ErrBudgetLimit
		}
	}
	return nil
}

func budgetReservationExposure(reservation db.GovernanceBudgetReservation) int64 {
	if reservation.State == "reserved" || !reservation.UsageKnown {
		return reservation.TotalCapMicroUsd
	}
	return reservation.SettledMicroUsd
}

func budgetIntervalsOverlap(firstStart, firstEnd, secondStart, secondEnd time.Time) bool {
	return firstStart.Before(secondEnd) && firstEnd.After(secondStart)
}

func adjustBudgetCounters(ctx context.Context, tx pgx.Tx, workspaceID, rootID pgtype.UUID, windowStart time.Time, rootReservedDelta, rootSpentDelta, windowReservedDelta, windowSpentDelta int64) error {
	result, err := tx.Exec(ctx, `
		UPDATE governance_budget_root
		SET reserved_micro_usd = reserved_micro_usd + $3,
		    spent_micro_usd = spent_micro_usd + $4,
		    updated_at = now()
		WHERE workspace_id = $1 AND budget_root_id = $2
		  AND reserved_micro_usd + $3 >= 0 AND spent_micro_usd + $4 >= 0
	`, workspaceID, rootID, rootReservedDelta, rootSpentDelta)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrBudgetInvariant
	}
	result, err = tx.Exec(ctx, `
		UPDATE governance_budget_window
		SET reserved_micro_usd = reserved_micro_usd + $3,
		    spent_micro_usd = spent_micro_usd + $4,
		    updated_at = now()
		WHERE workspace_id = $1 AND window_start = $2
		  AND reserved_micro_usd + $3 >= 0 AND spent_micro_usd + $4 >= 0
	`, workspaceID, windowStart.UTC(), windowReservedDelta, windowSpentDelta)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrBudgetInvariant
	}
	return nil
}

func insertBudgetJournal(ctx context.Context, tx pgx.Tx, workspaceID, reservationID pgtype.UUID, eventKey, eventType, digest string, expectedRevision, resultingRevision int64) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO governance_budget_journal (
			workspace_id, reservation_id, event_key, event_type, event_digest,
			expected_revision, resulting_revision
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, workspaceID, reservationID, eventKey, eventType, digest, expectedRevision, resultingRevision)
	return err
}

func findBudgetJournal(ctx context.Context, tx pgx.Tx, workspaceID, reservationID pgtype.UUID, eventKey, digest string) (bool, error) {
	var existingDigest string
	err := tx.QueryRow(ctx, `
		SELECT event_digest FROM governance_budget_journal
		WHERE workspace_id = $1 AND reservation_id = $2 AND event_key = $3
	`, workspaceID, reservationID, eventKey).Scan(&existingDigest)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if existingDigest != digest {
		return false, ErrBudgetConflict
	}
	return true, nil
}

func insertBudgetOutbox(ctx context.Context, tx pgx.Tx, workspaceID, reservationID pgtype.UUID, eventKey, eventType string, caseID, attemptID, obligationID pgtype.UUID) error {
	_, err := insertBudgetOutboxIntent(ctx, tx, workspaceID, reservationID, eventKey, eventType, caseID, attemptID, obligationID, "")
	return err
}

func insertBudgetOutboxIntent(ctx context.Context, tx pgx.Tx, workspaceID, reservationID pgtype.UUID, eventKey, eventType string, caseID, attemptID, obligationID pgtype.UUID, admissionDigest string) (pgtype.UUID, error) {
	payload, err := json.Marshal(struct {
		ReservationID   string `json:"reservation_id"`
		CaseID          string `json:"case_id"`
		AttemptID       string `json:"attempt_id"`
		ObligationID    string `json:"obligation_id"`
		AdmissionDigest string `json:"admission_digest,omitempty"`
	}{
		ReservationID:   budgetUUIDString(reservationID),
		CaseID:          budgetUUIDString(caseID),
		AttemptID:       budgetUUIDString(attemptID),
		ObligationID:    budgetUUIDString(obligationID),
		AdmissionDigest: admissionDigest,
	})
	if err != nil {
		return pgtype.UUID{}, err
	}
	var eventID pgtype.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO governance_budget_outbox (
			workspace_id, reservation_id, event_key, event_type, case_id,
			attempt_id, obligation_id, payload
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING event_id
	`, workspaceID, reservationID, eventKey, eventType, caseID, attemptID, obligationID, payload).Scan(&eventID)
	return eventID, err
}

func readBudgetOutboxIntent(ctx context.Context, tx pgx.Tx, workspaceID, reservationID pgtype.UUID, eventKey string) (pgtype.UUID, string, error) {
	var eventID pgtype.UUID
	var admissionDigest string
	err := tx.QueryRow(ctx, `
		SELECT event_id, COALESCE(payload->>'admission_digest', '')
		FROM governance_budget_outbox
		WHERE workspace_id = $1 AND reservation_id = $2 AND event_key = $3 AND event_type = 'admit'
	`, workspaceID, reservationID, eventKey).Scan(&eventID, &admissionDigest)
	return eventID, admissionDigest, err
}

func budgetReserveDigest(command BudgetReserveCommand, windowStart, windowEnd time.Time) (string, error) {
	return hashBudgetValue(struct {
		WorkspaceID            string    `json:"workspace_id"`
		ReservationID          string    `json:"reservation_id"`
		BudgetRootID           string    `json:"budget_root_id"`
		CaseID                 string    `json:"case_id"`
		AttemptID              string    `json:"attempt_id"`
		ObligationID           string    `json:"obligation_id"`
		Resource               string    `json:"resource"`
		ControlEpoch           int64     `json:"control_epoch"`
		WindowStart            time.Time `json:"window_start"`
		WindowEnd              time.Time `json:"window_end"`
		RootCapMicroUSD        int64     `json:"root_cap_micro_usd"`
		WindowCapMicroUSD      int64     `json:"window_cap_micro_usd"`
		MaxAttemptCostMicroUSD int64     `json:"max_attempt_cost_micro_usd"`
		RetryAllowance         int64     `json:"retry_allowance"`
		RetryPolicyBounded     bool      `json:"retry_policy_bounded"`
		SlotLimit              int64     `json:"slot_limit"`
	}{budgetUUIDString(command.WorkspaceID), budgetUUIDString(command.ReservationID),
		budgetUUIDString(command.BudgetRootID), budgetUUIDString(command.CaseID),
		budgetUUIDString(command.AttemptID), budgetUUIDString(command.ObligationID),
		command.Resource, command.ControlEpoch, windowStart, windowEnd, command.RootCapMicroUSD,
		command.WindowCapMicroUSD, command.MaxAttemptCostMicroUSD, command.RetryAllowance,
		command.RetryPolicyBounded, command.SlotLimit}), nil
}

func budgetDebitDigest(command BudgetDebitCommand) string {
	return hashBudgetValue(struct {
		WorkspaceID    string `json:"workspace_id"`
		ReservationID  string `json:"reservation_id"`
		AmountMicroUSD int64  `json:"amount_micro_usd"`
	}{budgetUUIDString(command.WorkspaceID), budgetUUIDString(command.ReservationID), command.AmountMicroUSD})
}

func budgetSettleDigest(command BudgetSettleCommand) string {
	return hashBudgetValue(struct {
		WorkspaceID      string `json:"workspace_id"`
		ReservationID    string `json:"reservation_id"`
		ReceiptID        string `json:"receipt_id"`
		UsageKnown       bool   `json:"usage_known"`
		TerminationKnown bool   `json:"termination_known"`
		UsageMicroUSD    int64  `json:"usage_micro_usd"`
	}{budgetUUIDString(command.WorkspaceID), budgetUUIDString(command.ReservationID), command.ReceiptID,
		command.UsageKnown, command.TerminationKnown, command.UsageMicroUSD})
}

func hashBudgetValue(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func validBudgetDigest(digest string) bool {
	if len(digest) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(digest)
	return err == nil && hex.EncodeToString(decoded) == digest
}

func validBudgetKey(key string) bool {
	return utf8.ValidString(key) && utf8.RuneCountInString(key) > 0 && utf8.RuneCountInString(key) <= 200 && strings.TrimSpace(key) == key
}

func budgetWindowKey(start time.Time) int64 {
	return start.UTC().UnixMicro()
}

func minBudgetCap(first, second int64) int64 {
	if first < second {
		return first
	}
	return second
}

func budgetAddAmounts(first, second int64) (int64, error) {
	if second > 0 && first > math.MaxInt64-second || second < 0 && first < math.MinInt64-second {
		return 0, ErrBudgetOverflow
	}
	return first + second, nil
}

func budgetUUIDString(value pgtype.UUID) string {
	return uuid.UUID(value.Bytes).String()
}
