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
// linked PR pass on headSHA and the issue's current step asks for it
// (advance_on_ci_green). It reports whether the move started a run for the next
// step's handler, so the caller does not wake the same owner twice.
//
// The write is the one a person's status change makes: the project's workflow
// decides the handoff, and the run starts through the same trigger predicate.
// What differs is the actor: the platform, on a step whose handler a workflow
// admin already validated when saving it, so the invoke gate a request-bound
// assignee pick passes is not asked again.
func (h *Handler) advanceIssueOnCIGreen(ctx context.Context, issueID pgtype.UUID, headSHA string) (ranHandler bool) {
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
	step, ok := def.Step(issue.Status)
	next := def.NextStatus(issue.Status)
	if !ok || !step.AdvanceOnCIGreen || next == "" {
		return false
	}
	claimed, err := h.Queries.ClaimIssueCIAdvance(ctx, db.ClaimIssueCIAdvanceParams{IssueID: issue.ID, HeadSha: headSHA})
	if err != nil || claimed == 0 {
		if err != nil {
			slog.Warn("ci advance: claim failed", "err", err, "issue_id", uuidToString(issueID))
		}
		return false
	}
	// Another writer moving the issue first (revision) wins; the claim is given
	// back so the next snapshot of this head can try the issue where it is.
	moved := false
	defer func() {
		if !moved {
			if err := h.Queries.ReleaseIssueCIAdvance(ctx, db.ReleaseIssueCIAdvanceParams{IssueID: issue.ID, HeadSha: headSHA}); err != nil {
				slog.Warn("ci advance: release claim failed", "err", err, "issue_id", uuidToString(issueID))
			}
		}
	}()

	params := db.UpdateIssueParams{
		ID:               issue.ID,
		ExpectedRevision: pgtype.Int8{Int64: issue.Revision, Valid: true},
		Status:           pgtype.Text{String: next, Valid: true},
		AssigneeType:     issue.AssigneeType,
		AssigneeID:       issue.AssigneeID,
		StartDate:        issue.StartDate,
		DueDate:          issue.DueDate,
		ParentIssueID:    issue.ParentIssueID,
		ProjectID:        issue.ProjectID,
		Stage:            issue.Stage,
	}
	handoff, write, err := h.decideIssueWorkflow(ctx, issue, &params, true, false, false, nil)
	if err != nil {
		slog.Warn("ci advance: workflow refused the move", "err", err, "issue_id", uuidToString(issueID), "to", next)
		return false
	}
	updated, err := h.updateIssueWithStatusGuard(ctx, issue.WorkspaceID, next, params, write)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("ci advance: status write failed", "err", err, "issue_id", uuidToString(issueID), "to", next)
		}
		return false
	}
	moved = true

	assigneeChanged := handoff != nil &&
		(issue.AssigneeType.String != updated.AssigneeType.String || uuidToString(issue.AssigneeID) != uuidToString(updated.AssigneeID))
	var willRun bool
	var trigger service.IssueRunTrigger
	if handoff != nil {
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
		"source":           "ci_green_advance",
		"head_sha":         headSHA,
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
	return ranHandler
}

// issueResponseWithCategory is the issue as clients receive it on an update.
func (h *Handler) issueResponseWithCategory(ctx context.Context, issue db.Issue) IssueResponse {
	resp := issueToResponse(issue, h.getIssuePrefix(ctx, issue.WorkspaceID))
	h.fillStatusCategory(ctx, issue.WorkspaceID, &resp)
	return resp
}
