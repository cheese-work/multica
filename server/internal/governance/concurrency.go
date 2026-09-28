package governance

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	ErrConcurrencyInput     = errors.New("invalid governance concurrency input")
	ErrConcurrencyControl   = errors.New("governance concurrency control is stale or disabled")
	ErrConcurrencyLimit     = errors.New("governance concurrency limit reached")
	ErrConcurrencyReleased  = errors.New("governance concurrency reservation was released")
	ErrConcurrencyInvariant = errors.New("governance concurrency counter is inconsistent")
)

type ConcurrencyGuards struct {
	tx           pgx.Tx
	workspaceID  pgtype.UUID
	controlEpoch int64
	enabled      bool
	settings     WorkspaceSettings
	resources    []string
}

func LockConcurrencyResources(ctx context.Context, tx pgx.Tx, workspaceID pgtype.UUID, resources ...string) (*ConcurrencyGuards, error) {
	if tx == nil || !validConcurrencyUUID(workspaceID) || len(resources) == 0 {
		return nil, ErrConcurrencyInput
	}
	resources = slices.Clone(resources)
	for _, resource := range resources {
		if resource == "" || strings.TrimSpace(resource) != resource || len(resource) > 128 || strings.ContainsRune(resource, '\x00') {
			return nil, ErrConcurrencyInput
		}
	}
	slices.Sort(resources)
	resources = slices.Compact(resources)
	if _, err := tx.Exec(ctx, `
		INSERT INTO governance_workspace_config (workspace_id) VALUES ($1)
		ON CONFLICT (workspace_id) DO NOTHING
	`, workspaceID); err != nil {
		return nil, err
	}
	guards := &ConcurrencyGuards{tx: tx, workspaceID: workspaceID, resources: resources}
	var settingsJSON []byte
	if err := tx.QueryRow(ctx, `
		SELECT control_epoch, settings FROM governance_workspace_config
		WHERE workspace_id = $1 FOR UPDATE
	`, workspaceID).Scan(&guards.controlEpoch, &settingsJSON); err != nil {
		return nil, err
	}
	var settings WorkspaceSettings
	if err := json.Unmarshal(settingsJSON, &settings); err != nil {
		return nil, err
	}
	guards.enabled = settings.JevGovernanceEnabled && (settings.RuleMode == RuleModeShadow || settings.RuleMode == RuleModeCorrection)
	guards.settings = settings
	for _, resource := range resources {
		if _, err := tx.Exec(ctx, `
			INSERT INTO governance_concurrency_guard (workspace_id, resource) VALUES ($1, $2)
			ON CONFLICT (workspace_id, resource) DO NOTHING
		`, workspaceID, resource); err != nil {
			return nil, err
		}
		var heldSlots int64
		if err := tx.QueryRow(ctx, `
			SELECT held_slots FROM governance_concurrency_guard
			WHERE workspace_id = $1 AND resource = $2 FOR UPDATE
		`, workspaceID, resource).Scan(&heldSlots); err != nil {
			return nil, err
		}
	}
	return guards, nil
}

func (guards *ConcurrencyGuards) Reserve(ctx context.Context, resource string, reservationID pgtype.UUID, expectedControlEpoch, limit int64) (bool, error) {
	if !guards.validReservation(resource, reservationID) || expectedControlEpoch < 0 || limit < 0 {
		return false, ErrConcurrencyInput
	}
	if !guards.enabled || guards.controlEpoch != expectedControlEpoch {
		return false, ErrConcurrencyControl
	}
	var state string
	err := guards.tx.QueryRow(ctx, `
		SELECT state FROM governance_concurrency_hold
		WHERE workspace_id = $1 AND resource = $2 AND reservation_id = $3
	`, guards.workspaceID, resource, reservationID).Scan(&state)
	if err == nil {
		if state == "released" {
			return false, ErrConcurrencyReleased
		}
		return true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	result, err := guards.tx.Exec(ctx, `
		UPDATE governance_concurrency_guard SET held_slots = held_slots + 1
		WHERE workspace_id = $1 AND resource = $2 AND held_slots < $3
	`, guards.workspaceID, resource, limit)
	if err != nil {
		return false, err
	}
	if result.RowsAffected() != 1 {
		return false, ErrConcurrencyLimit
	}
	_, err = guards.tx.Exec(ctx, `
		INSERT INTO governance_concurrency_hold (workspace_id, resource, reservation_id)
		VALUES ($1, $2, $3)
	`, guards.workspaceID, resource, reservationID)
	return false, err
}

func (guards *ConcurrencyGuards) Release(ctx context.Context, resource string, reservationID pgtype.UUID) (bool, error) {
	if !guards.validReservation(resource, reservationID) {
		return false, ErrConcurrencyInput
	}
	result, err := guards.tx.Exec(ctx, `
		UPDATE governance_concurrency_hold SET state = 'released', released_at = now()
		WHERE workspace_id = $1 AND resource = $2 AND reservation_id = $3 AND state = 'held'
	`, guards.workspaceID, resource, reservationID)
	if err != nil || result.RowsAffected() == 0 {
		return false, err
	}
	result, err = guards.tx.Exec(ctx, `
		UPDATE governance_concurrency_guard SET held_slots = held_slots - 1
		WHERE workspace_id = $1 AND resource = $2 AND held_slots > 0
	`, guards.workspaceID, resource)
	if err != nil {
		return false, err
	}
	if result.RowsAffected() != 1 {
		return false, ErrConcurrencyInvariant
	}
	return true, nil
}

func (guards *ConcurrencyGuards) validReservation(resource string, reservationID pgtype.UUID) bool {
	return guards != nil && validConcurrencyUUID(reservationID) && slices.Contains(guards.resources, resource)
}

func validConcurrencyUUID(value pgtype.UUID) bool {
	return value.Valid && value.Bytes != [16]byte{}
}
