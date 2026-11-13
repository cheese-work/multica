package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// The pr_merge_owed row (CHE-5243) is a write-ahead intent: a PR merge owes its
// issue a completion decision until one settles. The scheduler runs
// evaluatePRMergeOwed when the row falls due. Completion is fenced by the row's
// (id, revision): an attempt reads that generation before it reads any decision
// fact, and only that generation may be deleted or rescheduled afterwards. A
// merge event that lands mid-attempt bumps the revision, so the row survives
// the older attempt and is decided again.

const (
	prMergeOwedBaseDelay = 30 * time.Second
	prMergeOwedMaxDelay  = time.Hour
	// 30s doubled 7 times is already past the cap, so larger counts stop growing.
	prMergeOwedMaxDoublings = 7
	prMergeOwedErrorRunes   = 500
)

// prMergeOwedBackoff is the wait after failed attempt number fireCount+1 of one
// generation: 30 seconds, doubling, up to one hour.
func prMergeOwedBackoff(fireCount int32) time.Duration {
	if fireCount < 0 {
		fireCount = 0
	}
	if fireCount >= prMergeOwedMaxDoublings {
		return prMergeOwedMaxDelay
	}
	return min(prMergeOwedBaseDelay<<fireCount, prMergeOwedMaxDelay)
}

// upsertPRMergeOwed records the debt for an issue: a new row at revision 1, or
// the existing row with its revision bumped (same id). It writes nothing when
// the workspace's PR merge status is none, since no merge can move an issue
// there. Pass transaction-bound queries to make the row atomic with the write
// that owes it.
func upsertPRMergeOwed(ctx context.Context, q *db.Queries, workspaceID, issueID pgtype.UUID) error {
	ws, err := q.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return fmt.Errorf("load workspace: %w", err)
	}
	if prMergeStatusSetting(ws) == prMergeStatusNone {
		return nil
	}
	_, err = q.UpsertPRMergeOwed(ctx, db.UpsertPRMergeOwedParams{ID: dbid.NewV7(), WorkspaceID: workspaceID, IssueID: issueID})
	return err
}

// runPRMergeOwed is one fenced attempt. It reads the generation first, runs
// decide, then settles that generation only: a delete after success (a move, or
// a state that moves nothing), a reschedule after failure. Neither write ever
// changes the revision. With no row at read time there is no generation, so
// decide still runs but nothing is deleted or rescheduled.
func (h *Handler) runPRMergeOwed(ctx context.Context, workspaceID, issueID pgtype.UUID, decide func(context.Context) error) error {
	gen, err := h.Queries.GetPRMergeOwedGeneration(ctx, issueID)
	hasGen := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("read pr_merge_owed generation: %w", err)
	}
	if err := decide(ctx); err != nil {
		if hasGen {
			_, rerr := h.Queries.ReschedulePRMergeOwedGeneration(ctx, db.ReschedulePRMergeOwedGenerationParams{
				ID: gen.ID, Revision: gen.Revision,
				NextFireAt: pgtype.Timestamptz{Time: time.Now().Add(prMergeOwedBackoff(gen.FireCount)), Valid: true},
				LastError:  truncateRunes(err.Error(), prMergeOwedErrorRunes),
			})
			if rerr != nil {
				slog.Warn("pr_merge_owed: reschedule failed", "err", rerr, "issue_id", uuidToString(issueID))
			}
		}
		return err
	}
	if !hasGen {
		return nil
	}
	if _, err := h.Queries.DeletePRMergeOwedGeneration(ctx, db.DeletePRMergeOwedGenerationParams{ID: gen.ID, Revision: gen.Revision}); err != nil {
		return fmt.Errorf("settle pr_merge_owed: %w", err)
	}
	return nil
}

// evaluatePRMergeOwed settles one due pr_merge_owed row by re-running the PR
// merge completion decision for its issue. It records its own failures on the
// row (fenced), so the scheduler's id-only failure note skips this rule.
func (h *Handler) evaluatePRMergeOwed(ctx context.Context, w db.IssueWakeup) error {
	return h.runPRMergeOwed(ctx, w.WorkspaceID, w.IssueID, func(ctx context.Context) error {
		return h.autoCompleteIssue(ctx, w.WorkspaceID, w.IssueID, nil)
	})
}
