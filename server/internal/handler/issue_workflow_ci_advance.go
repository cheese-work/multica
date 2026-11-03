package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// advanceIssueOnCIGreen moves an issue one workflow step when the checks of a
// linked PR pass on headSHA and the issue's current step has an edge that
// fact satisfies (advance_on_ci_green stands for a pr.checks_green edge to the
// next status); the first such edge in list order is taken. It reports whether
// the move started a run for the next step's handler, so the caller does not wake the same owner twice.
//
// The write is the one a person's status change makes: the project's workflow
// decides the handoff, and the run starts through the same trigger predicate.
// What differs is the actor: the platform, on a step whose handler a workflow
// admin already validated when saving it, so the invoke gate a request-bound
// assignee pick passes is not asked again.
func (h *Handler) advanceIssueOnCIGreen(ctx context.Context, issueID pgtype.UUID, headSHA, prState string) (ranHandler bool) {
	issue, err := h.Queries.GetIssue(ctx, issueID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("ci advance: load issue failed", "err", err, "issue_id", uuidToString(issueID))
		}
		return false
	}
	if (!issue.ProjectID.Valid && !issue.WorkflowID.Valid) || issue.TriageState.Valid ||
		issuestatus.Category(ctx, h.Queries, issue.WorkspaceID, issue.Status) == issuestatus.CategoryClosed {
		return false
	}
	def, _, err := issueworkflow.ForIssue(ctx, h.Queries, issue.WorkspaceID, issue.WorkflowID, issue.ProjectID)
	if err != nil || def == nil {
		if err != nil {
			slog.Warn("ci advance: load workflow failed", "err", err, "issue_id", uuidToString(issueID))
		}
		return false
	}
	// The caller saw a fresh head with green checks on an open or draft PR;
	// the first edge that fact satisfies, in list order, is the move.
	edge, ok := issueworkflow.FirstMatch(def.EdgesOf(issue.Status), issueworkflow.Facts{
		PRs: []issueworkflow.PRFact{{State: prState, ChecksRollup: "SUCCESS", HeadFresh: true}},
	})
	if !ok {
		return false
	}
	next := edge.To
	claimed, err := h.Queries.ClaimIssueCIAdvance(ctx, db.ClaimIssueCIAdvanceParams{IssueID: issue.ID, HeadSha: headSHA})
	if err != nil || claimed == 0 {
		if err != nil {
			slog.Warn("ci advance: claim failed", "err", err, "issue_id", uuidToString(issueID))
		}
		return false
	}
	// Another writer moving the issue first (revision) wins; the claim is given
	// back so the next snapshot of this head can try the issue where it is.
	// The kill switch (CHE-2220) finds this run by the mark: nothing else on it
	// says a GitHub snapshot started it. The mark goes on the row this move's
	// own enqueue creates, never on a later run of the issue.
	markCtx := service.WithEnqueuedTaskHook(ctx, func(task db.AgentTaskQueue) {
		if task.IssueID != issue.ID {
			return
		}
		if err := h.Queries.MarkGitHubDispatchRun(ctx, db.MarkGitHubDispatchRunParams{TaskID: task.ID, IssueID: issue.ID, Source: "ci_green"}); err != nil {
			slog.Warn("ci advance: mark run failed", "err", err, "issue_id", uuidToString(issueID))
		}
	})
	moved, ranHandler := h.moveIssueOnEdge(markCtx, issue, next, "ci_green_advance", map[string]any{"head_sha": headSHA}, true)
	if !moved {
		if err := h.Queries.ReleaseIssueCIAdvance(ctx, db.ReleaseIssueCIAdvanceParams{IssueID: issue.ID, HeadSha: headSHA}); err != nil {
			slog.Warn("ci advance: release claim failed", "err", err, "issue_id", uuidToString(issueID))
		}
	}
	return ranHandler
}

// moveIssueOnEdge writes the status change a workflow edge stands for and
// publishes it as the platform. issue is the row the caller decided on: a
// concurrent writer that changed it first (revision) wins and nothing moves.
// source names the cause in the event payload and extra adds its details.
// startHandler false leaves the target step's handler unrun, for a move whose
// cause is that nothing more should run. It reports whether the status moved
// and whether the move started a run for the next step's handler, so the caller
// does not wake the same owner twice.
func (h *Handler) moveIssueOnEdge(ctx context.Context, issue db.Issue, to, source string, extra map[string]any, startHandler bool) (moved, ranHandler bool) {
	moved, ranHandler, err := h.tryMoveIssueOnEdge(ctx, issue, to, source, extra, startHandler)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		slog.Warn("workflow edge: move failed", "err", err, "issue_id", uuidToString(issue.ID), "to", to, "source", source)
	}
	return moved, ranHandler
}

// tryMoveIssueOnEdge is moveIssueOnEdge for a caller that needs the reason a
// move did not happen: pgx.ErrNoRows means a concurrent writer won.
func (h *Handler) tryMoveIssueOnEdge(ctx context.Context, issue db.Issue, to, source string, extra map[string]any, startHandler bool) (moved, ranHandler bool, err error) {
	// The lock holds through the commit, so a person's move cannot read this
	// issue's status between this write and its grant. The block an exhausted
	// budget causes neither grants nor needs it: it runs under the refused
	// admission's locks, after that admission wrote the window's ledger row, and
	// the boundary lock comes before those locks everywhere else.
	release := func() {}
	if source != "run_budget" {
		if release, err = h.lockRunBudgetBoundary(ctx, issue.ID); err != nil {
			return false, false, fmt.Errorf("run budget boundary: %w", err)
		}
	}
	defer release()
	tx, err := h.beginWakeupWrite(ctx)
	if err != nil {
		return false, false, err
	}
	defer tx.Rollback(ctx)
	move, err := h.moveIssueOnEdgeTx(ctx, h.Queries.WithTx(tx), issue, to, source, extra, startHandler)
	if err != nil {
		var notConfirmed *service.StopNotConfirmedError
		if errors.As(err, &notConfirmed) {
			h.recordStopNotConfirmed(ctx, issue, "system", "", notConfirmed)
		}
		return false, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, false, fmt.Errorf("commit edge move: %w", err)
	}
	release()
	if move.RunRefused != nil && !errors.Is(move.RunRefused, service.ErrDuplicatePendingTask) {
		slog.Debug("workflow edge: run refused", "err", move.RunRefused, "issue_id", uuidToString(issue.ID), "to", to, "source", source)
	}
	ranHandler = move.AfterCommit(ctx)
	return true, ranHandler, nil
}

// issueResponseWithCategory is the issue as clients receive it on an update.
func (h *Handler) issueResponseWithCategory(ctx context.Context, issue db.Issue) IssueResponse {
	resp := issueToResponse(issue, h.getIssuePrefix(ctx, issue.WorkspaceID))
	h.fillStatusCategory(ctx, issue.WorkspaceID, &resp)
	return resp
}
