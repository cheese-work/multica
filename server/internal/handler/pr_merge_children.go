package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The PR merge rule for a workflow with include_child_prs (CHE-5242). The
// issue's delivery PRs are its own and its direct children's, read the way a
// workflow edge reads them (ListEdgePullRequests), and the edge's rule decides:
// at least one merged PR and none open or draft. A closed-unmerged PR does not
// block, and the children's own status is not checked. Without the flag the
// issue's own closing links decide and every child must be done or cancelled.

// prMergeDeliveryPRs is the PR set the merge rule reads for issueID.
func prMergeDeliveryPRs(ctx context.Context, q *db.Queries, issueID pgtype.UUID, includeChildren bool) ([]db.ListIssueLinkedPullRequestStatesRow, error) {
	if !includeChildren {
		return q.ListIssueLinkedPullRequestStates(ctx, issueID)
	}
	children, err := q.ListChildIssues(ctx, issueID)
	if err != nil {
		return nil, err
	}
	ids := []pgtype.UUID{issueID}
	for _, child := range children {
		ids = append(ids, child.ID)
	}
	rows, err := q.ListEdgePullRequests(ctx, ids)
	if err != nil {
		return nil, err
	}
	// A PR linked to the issue and to a child, or to two children, is one PR.
	seen := make(map[pgtype.UUID]struct{}, len(rows))
	prs := make([]db.ListIssueLinkedPullRequestStatesRow, 0, len(rows))
	for _, r := range rows {
		if _, dup := seen[r.PrID]; dup {
			continue
		}
		seen[r.PrID] = struct{}{}
		prs = append(prs, db.ListIssueLinkedPullRequestStatesRow{
			ID: r.PrID, Provider: r.Provider, PrNumber: r.PrNumber, State: r.State,
			RepoOwner: r.RepoOwner, RepoName: r.RepoName, MergeCommitSha: r.MergeCommitSha,
		})
	}
	return prs, nil
}

// classifyDeliveryPRs buckets the PRs into a decision state and the PRs that
// state is about. prs is not empty.
func classifyDeliveryPRs(prs []db.ListIssueLinkedPullRequestStatesRow, includeChildren bool) (string, []db.ListIssueLinkedPullRequestStatesRow) {
	var open, closed, merged []db.ListIssueLinkedPullRequestStatesRow
	for _, pr := range prs {
		switch pr.State {
		case "open", "draft":
			open = append(open, pr)
		case "closed":
			closed = append(closed, pr)
		case "merged":
			merged = append(merged, pr)
		}
	}
	switch {
	case len(open) > 0:
		return prAutoCompleteWaiting, open
	case includeChildren && len(merged) == 0, !includeChildren && len(closed) > 0:
		return prAutoCompleteNotMerged, closed
	case includeChildren:
		return prAutoCompleteAllMerged, merged
	default:
		return prAutoCompleteAllMerged, prs
	}
}

// prMergeStillReady re-reads the PRs the merge rule counts and reports whether
// the decision still holds, for the write's own transaction.
func prMergeStillReady(ctx context.Context, q *db.Queries, issueID pgtype.UUID, includeChildren bool) (bool, error) {
	prs, err := prMergeDeliveryPRs(ctx, q, issueID, includeChildren)
	if err != nil || len(prs) == 0 {
		return false, err
	}
	state, _ := classifyDeliveryPRs(prs, includeChildren)
	return state == prAutoCompleteAllMerged, nil
}

// includeChildPRs reports whether the issue's workflow counts child PRs.
func (h *Handler) includeChildPRs(ctx context.Context, issue db.Issue) (bool, error) {
	def, err := h.issueWorkflowDef(ctx, issue)
	return def != nil && def.IncludeChildPRs, err
}

// addIncludeChildParents adds to touched the parent of each issue whose
// workflow counts child PRs, so a child's PR event re-runs the parent's
// decision. A failed read is returned: the caller retries or logs it the way it
// does a failed decision, since dropping it would lose the parent's event.
func (h *Handler) addIncludeChildParents(ctx context.Context, workspaceID pgtype.UUID, touched map[pgtype.UUID]struct{}, issueIDs ...pgtype.UUID) error {
	for _, id := range issueIDs {
		parentID, err := h.includeChildParent(ctx, workspaceID, id)
		if err != nil {
			return fmt.Errorf("resolve child parent of %s: %w", uuidToString(id), err)
		}
		if parentID.Valid {
			touched[parentID] = struct{}{}
		}
	}
	return nil
}

// includeChildParent is the parent of issueID when that parent's workflow has
// include_child_prs, else the zero UUID.
func (h *Handler) includeChildParent(ctx context.Context, workspaceID, issueID pgtype.UUID) (pgtype.UUID, error) {
	issue, err := h.Queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: issueID, WorkspaceID: workspaceID})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !issue.ParentIssueID.Valid) {
		return pgtype.UUID{}, nil
	}
	if err != nil {
		return pgtype.UUID{}, err
	}
	parent, err := h.Queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: issue.ParentIssueID, WorkspaceID: workspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, nil
	}
	if err != nil {
		return pgtype.UUID{}, err
	}
	include, err := h.includeChildPRs(ctx, parent)
	if err != nil || !include {
		return pgtype.UUID{}, err
	}
	return parent.ID, nil
}

// rerunPRMergeForParents re-runs the PR merge decision of each parent after a
// child reached a new status: without include_child_prs the parent waits for
// every child to be done or cancelled, and no PR event announces that.
func (h *Handler) rerunPRMergeForParents(ctx context.Context, parentIDs ...pgtype.UUID) {
	var resolver *issuestatus.Resolver
	for _, id := range parentIDs {
		if !id.Valid {
			continue
		}
		parent, err := h.Queries.GetIssue(ctx, id)
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				slog.Warn("pr merge: load parent failed", "err", err, "issue_id", uuidToString(id))
			}
			continue
		}
		if resolver == nil {
			resolver = issuestatus.NewResolver(parent.WorkspaceID)
		}
		h.maybeAutoCompleteIssue(ctx, parent.WorkspaceID, parent.ID, resolver)
	}
}

// rerunIncludeChildParent re-runs the decision of the issue's parent after a
// manual PR link or unlink on the issue, when the parent counts child PRs.
func (h *Handler) rerunIncludeChildParent(ctx context.Context, issue db.Issue) {
	touched := map[pgtype.UUID]struct{}{}
	if err := h.addIncludeChildParents(ctx, issue.WorkspaceID, touched, issue.ID); err != nil {
		slog.Warn("pr merge: resolve parent failed", "err", err, "issue_id", uuidToString(issue.ID))
		return
	}
	for id := range touched {
		h.maybeAutoCompleteIssue(ctx, issue.WorkspaceID, id, nil)
	}
}
