package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
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

// owePRMerge writes the intent for each issue and for the parent of each whose
// workflow counts child PRs, through q. Pass the transaction-bound queries of
// the event that owes the decision: the rows commit with it, or an error rolls
// both back so the sender retries. It returns every issue it wrote, for the
// post-commit attempt.
func (h *Handler) owePRMerge(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID, issueIDs []pgtype.UUID) ([]pgtype.UUID, error) {
	return h.owePRMergeWithLinked(ctx, q, workspaceID, issueIDs, nil)
}

// owePRMergeWithLinked is owePRMerge plus the parent, not the issue itself, of
// each linked issue. A PR event that changes no link (a close of an unmerged
// PR) still changes what its linked children's parent counts (CHE-5242).
func (h *Handler) owePRMergeWithLinked(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID, issueIDs, linkedIDs []pgtype.UUID) ([]pgtype.UUID, error) {
	var owed []pgtype.UUID
	seen := map[pgtype.UUID]bool{}
	add := func(id pgtype.UUID) error {
		if seen[id] {
			return nil
		}
		seen[id] = true
		if err := upsertPRMergeOwed(ctx, q, workspaceID, id); err != nil {
			return fmt.Errorf("owe pr merge for issue %s: %w", uuidToString(id), err)
		}
		owed = append(owed, id)
		return nil
	}
	addParent := func(id pgtype.UUID) error {
		parentID, err := parentCountingChildPRs(ctx, q, workspaceID, id)
		if err != nil || !parentID.Valid {
			return err
		}
		return add(parentID)
	}
	for _, id := range issueIDs {
		if err := add(id); err != nil {
			return nil, err
		}
		if err := addParent(id); err != nil {
			return nil, err
		}
	}
	for _, id := range linkedIDs {
		if err := addParent(id); err != nil {
			return nil, err
		}
	}
	return owed, nil
}

// parentCountingChildPRs is the parent of issueID when its workflow reads the
// PRs of its children, else the zero UUID.
func parentCountingChildPRs(ctx context.Context, q *db.Queries, workspaceID, issueID pgtype.UUID) (pgtype.UUID, error) {
	issue, err := q.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: issueID, WorkspaceID: workspaceID})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !issue.ParentIssueID.Valid) {
		return pgtype.UUID{}, nil
	}
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("load issue for parent: %w", err)
	}
	parent, err := q.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: issue.ParentIssueID, WorkspaceID: workspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, nil
	}
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("load parent: %w", err)
	}
	def, _, err := issueworkflow.ForIssue(ctx, q, workspaceID, parent.WorkflowID, parent.ProjectID)
	if errors.Is(err, issueworkflow.ErrOverrideMissing) || (err == nil && def == nil) {
		return pgtype.UUID{}, nil
	}
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("load parent workflow: %w", err)
	}
	if !def.IncludeChildPRs {
		return pgtype.UUID{}, nil
	}
	return parent.ID, nil
}

// attemptPRMergeOwed is the inline attempt after the source transaction
// committed, fenced like the scheduler's. A failure leaves the row for it.
func (h *Handler) attemptPRMergeOwed(ctx context.Context, workspaceID pgtype.UUID, issueIDs []pgtype.UUID, resolver *issuestatus.Resolver) []error {
	var errs []error
	for _, id := range issueIDs {
		err := h.runPRMergeOwed(ctx, workspaceID, id, func(ctx context.Context) error {
			return h.autoCompleteIssue(ctx, workspaceID, id, resolver)
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("issue %s: %w", uuidToString(id), err))
		}
	}
	return errs
}
