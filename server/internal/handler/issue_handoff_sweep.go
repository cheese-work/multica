package handler

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-5488 (S1c): the recovery sweep for handoff intents. An intent the status
// write could not turn into a run (the dispatch failed, or the process stopped)
// stays pending; the sweep finds it and calls dispatchHandoffIntent with the
// stored actor and context, so the run is attributed as the first try would
// have been. The claim counts the attempt and leases the row by its backoff
// (30 s, 30 s, 60 s, 60 s), so several servers can sweep.
const (
	handoffSweepInterval = 15 * time.Second
	handoffSweepMinAge   = 30 * time.Second // the first try gets this long alone
	handoffSweepBatch    = 50
	handoffMaxAttempts   = 5
)

// RunHandoffIntentSweeper sweeps until ctx ends.
func (h *Handler) RunHandoffIntentSweeper(ctx context.Context) {
	ticker := time.NewTicker(handoffSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.SweepHandoffIntents(ctx, time.Now())
		}
	}
}

// SweepHandoffIntents recovers every due pending intent once.
func (h *Handler) SweepHandoffIntents(ctx context.Context, now time.Time) {
	claimed, err := h.Queries.ClaimDueHandoffDispatches(ctx, db.ClaimDueHandoffDispatchesParams{
		Now:           pgtype.Timestamptz{Time: now, Valid: true},
		CreatedBefore: pgtype.Timestamptz{Time: now.Add(-handoffSweepMinAge), Valid: true},
		BatchSize:     handoffSweepBatch,
	})
	if err != nil {
		slog.Warn("handoff intent sweep: claim failed", "error", err)
		return
	}
	for _, intent := range claimed {
		h.recoverHandoffIntent(ctx, intent)
	}
	h.pruneHandoffIntents(ctx, now)
}

// pruneHandoffIntents is the hook for pruning settled issue_handoff_dispatch
// rows. It also deletes operation receipts older than 7 days (CHE-5491).
func (h *Handler) pruneHandoffIntents(ctx context.Context, now time.Time) {
	h.pruneHandoffOperations(ctx, now)
}

func (h *Handler) recoverHandoffIntent(ctx context.Context, intent db.IssueHandoffDispatch) {
	_, err := h.dispatchHandoffIntentOpt(ctx, intent.ID, true)
	if err == nil {
		return
	}
	slog.Warn("handoff intent sweep: dispatch failed", "intent_id", util.UUIDToString(intent.ID), "attempt", intent.Attempts, "error", err)
	if intent.Attempts < handoffMaxAttempts {
		return
	}
	reason := err.Error()
	if len(reason) > 200 {
		reason = reason[:200]
	}
	if _, err := h.Queries.SettleHandoffDispatch(ctx, db.SettleHandoffDispatchParams{
		ID: intent.ID, State: handoffFailed, SkipReason: pgtype.Text{String: reason, Valid: true},
	}); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("handoff intent sweep: settle as failed failed", "intent_id", util.UUIDToString(intent.ID), "error", err)
		}
		return
	}
	h.recordIssueActivity(ctx, intent.WorkspaceID, intent.IssueID, "system", "", "handoff_run_not_started", map[string]string{
		"message": "handoff run not started: " + reason, "intent_id": util.UUIDToString(intent.ID),
	})
}
