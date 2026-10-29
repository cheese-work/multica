package handler

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
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
	params := db.UpdateIssueParams{
		ID:               issue.ID,
		ExpectedRevision: pgtype.Int8{Int64: issue.Revision, Valid: true},
		Status:           pgtype.Text{String: to, Valid: true},
		AssigneeType:     issue.AssigneeType,
		AssigneeID:       issue.AssigneeID,
		StartDate:        issue.StartDate,
		DueDate:          issue.DueDate,
		ParentIssueID:    issue.ParentIssueID,
		ProjectID:        issue.ProjectID,
		Stage:            issue.Stage,
	}
	handoff, write, err := h.decideIssueWorkflow(ctx, issue, &params, true, false, false, workflowCaller{})
	if err != nil {
		slog.Warn("workflow edge: workflow refused the move", "err", err, "issue_id", uuidToString(issue.ID), "to", to, "source", source)
		return false, false
	}
	updated, err := h.updateIssueWithStatusGuard(ctx, issue.WorkspaceID, to, params, write)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("workflow edge: status write failed", "err", err, "issue_id", uuidToString(issue.ID), "to", to, "source", source)
		}
		return false, false
	}

	assigneeChanged := handoff != nil &&
		(issue.AssigneeType.String != updated.AssigneeType.String || uuidToString(issue.AssigneeID) != uuidToString(updated.AssigneeID))
	var willRun bool
	var trigger service.IssueRunTrigger
	if handoff != nil && startHandler {
		trigger, willRun = h.IssueService.WillEnqueueRun(ctx, service.IssueTriggerInput{
			Issue: updated, PrevStatus: issue.Status, AssigneeChanged: true, StatusChanged: true, EnteredHandoffStep: true,
		}, service.IssueTriggerProbe{})
	}
	payload := map[string]any{
		"issue":            h.issueResponseWithCategory(ctx, updated),
		"assignee_changed": assigneeChanged,
		"status_changed":   true,
		"prev_status":      issue.Status,
		"creator_type":     issue.CreatorType,
		"creator_id":       uuidToString(issue.CreatorID),
		"source":           source,
	}
	for k, v := range extra {
		payload[k] = v
	}
	if assigneeChanged {
		payload["prev_assignee_type"] = textToPtr(issue.AssigneeType)
		payload["prev_assignee_id"] = uuidToPtr(issue.AssigneeID)
	}
	if handoff != nil {
		payload["workflow_handoff"] = workflowHandoffPayload(updated, handoff, h.workflowHandoffBrief(ctx, updated, handoff), willRun, "")
	}
	h.publish(protocol.EventIssueUpdated, uuidToString(updated.WorkspaceID), "system", "", payload)
	if willRun {
		h.IssueService.CancelStaleStepRuns(ctx, updated, trigger.AgentID)
		ranHandler = h.dispatchIssueRun(ctx, updated, trigger, "system", "", "")
	}
	if updated.ParentIssueID.Valid || issue.ParentIssueID.Valid {
		h.processChildEvents(ctx, updated.ParentIssueID, issue.ParentIssueID)
	}
	return true, ranHandler
}

// issueResponseWithCategory is the issue as clients receive it on an update.
func (h *Handler) issueResponseWithCategory(ctx context.Context, issue db.Issue) IssueResponse {
	resp := issueToResponse(issue, h.getIssuePrefix(ctx, issue.WorkspaceID))
	h.fillStatusCategory(ctx, issue.WorkspaceID, &resp)
	return resp
}
