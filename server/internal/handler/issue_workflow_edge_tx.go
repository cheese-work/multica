package handler

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/dispatch"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// edgeMove is what moveIssueOnEdgeTx leaves for the caller.
type edgeMove struct {
	Issue db.Issue
	// RunRefused is why the next step's run was not inserted (budget, triage,
	// a pending run already holds the slot). The move is written regardless; a
	// caller that needs both or neither rolls back on it.
	RunRefused error
	// AfterCommit publishes the move and runs the work that follows a commit;
	// it reports whether a run for the next step's handler now exists. Call it
	// only after the caller's transaction committed.
	AfterCommit func(ctx context.Context) (ranHandler bool)
}

// moveIssueOnEdgeTx writes the status change a workflow edge stands for, its
// history row, the run budget's step grant and the next step's run with q, the
// queries of the caller's transaction. Nothing is visible until the caller
// commits, and a later failure in the caller's transaction takes all of it
// back. The caller holds no issue locks yet: this takes the status-archive
// guard, the project lock and the issue row in the order every issue write
// uses. issue is the row the caller decided on; a concurrent writer that
// changed it first (revision) wins and the error is pgx.ErrNoRows.
//
// A handoff check that cannot run inside the transaction refuses the move: a
// run whose stop is not confirmed returns *service.StopNotConfirmedError with
// nothing written. A squad leader's run is the one handoff that still starts
// after commit, because its access gate reads outside the transaction.
func (h *Handler) moveIssueOnEdgeTx(ctx context.Context, q *db.Queries, issue db.Issue, to, source string, extra map[string]any, startHandler bool) (edgeMove, error) {
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
	if err := assertIssueStatusStillActive(ctx, q, issue.WorkspaceID, to); err != nil {
		return edgeMove{}, err
	}
	handoff, write, err := h.decideIssueWorkflowWith(ctx, q, issue, &params, true, false, false, workflowCaller{})
	if err != nil {
		return edgeMove{}, fmt.Errorf("workflow refused the move: %w", err)
	}
	write.system = true
	updated, cancelledWakeups, err := h.writeIssueLocked(ctx, q, issue.WorkspaceID, params, write)
	if err != nil {
		return edgeMove{}, fmt.Errorf("status write failed: %w", err)
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
	var handoffDetails map[string]string
	if handoff != nil {
		handoffDetails = workflowHandoffPayload(updated, handoff, h.workflowHandoffBrief(ctx, updated, handoff), willRun, "")
	}
	var rec *statusRecord
	if issue.Status != updated.Status {
		row, err := service.RecordStatusChange(ctx, q, updated, issue.Status, updated.Status, service.StatusTrigger{
			Cause:   service.StatusCause{Kind: source, By: service.StatusCauseActor{Type: "system"}},
			Channel: "system",
		}, handoffDetails)
		if err != nil {
			return edgeMove{}, fmt.Errorf("record status change: %w", err)
		}
		rec = &statusRecord{workspaceID: updated.WorkspaceID, activity: &row}
	}
	// The block an exhausted budget causes is a consequence of exhaustion, and
	// it lands before the window's exhaustion claim: it must not grant. The
	// grant comes before the run is admitted, so the run reads the balance it
	// grants.
	if source != "run_budget" {
		if err := service.GrantRunBudgetStep(ctx, q, updated, issue.Status, dispatch.BudgetRecoveryCaller(ctx)); err != nil {
			return edgeMove{}, fmt.Errorf("grant run budget step: %w", err)
		}
	}

	var (
		runAfter     func()
		runRefused   error
		cancelAfter  func()
		squadAfter   bool
		duplicateRun bool
	)
	if willRun {
		var notConfirmed *service.StopNotConfirmedError
		if err := service.StopGate(ctx, q, updated.ID); errors.As(err, &notConfirmed) {
			return edgeMove{}, err
		}
		if trigger.AssigneeType == "agent" {
			if cancelAfter, err = h.TaskService.CancelStaleStepRunsInTx(ctx, q, updated, trigger.AgentID); err != nil {
				return edgeMove{}, err
			}
			runAfter, err = h.TaskService.EnqueueIssueRunInTx(ctx, q, updated, pgtype.UUID{})
			switch {
			case err == nil:
			case errors.Is(err, service.ErrDuplicatePendingTask):
				runRefused, duplicateRun = err, true
			case errors.Is(err, service.ErrRunBudgetExhausted), errors.Is(err, service.ErrIssueInTriage), errors.Is(err, service.ErrRecoveryCaused):
				runRefused = err
			default:
				return edgeMove{}, err
			}
		} else {
			squadAfter = true
		}
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
	if handoffDetails != nil {
		payload["workflow_handoff"] = handoffDetails
	}
	if rec != nil {
		payload["activity_id"] = uuidToString(rec.activity.ID)
	}

	afterCommit := func(ctx context.Context) (ranHandler bool) {
		h.broadcastCancelledWakeups(ctx, issue.WorkspaceID, cancelledWakeups)
		h.publish(protocol.EventIssueUpdated, uuidToString(updated.WorkspaceID), "system", "", payload)
		h.publishRecordedStatusActivity(rec, "system", "")
		if cancelAfter != nil {
			cancelAfter()
		}
		switch {
		case squadAfter:
			h.IssueService.CancelStaleStepRuns(ctx, updated, trigger.AgentID)
			ranHandler = h.dispatchIssueRun(ctx, updated, trigger, "system", "", "")
		case duplicateRun:
			// A sibling run holds the slot; it delivers the move only if it is
			// the queued assignment-level run for the step the issue is in now.
			ranHandler = h.queuedRunCarriesWrite(ctx, updated, trigger.AgentID)
		case runAfter != nil:
			runAfter()
			ranHandler = true
		}
		if updated.ParentIssueID.Valid || issue.ParentIssueID.Valid {
			h.processChildEvents(ctx, updated.ParentIssueID, issue.ParentIssueID)
		}
		return ranHandler
	}
	return edgeMove{Issue: updated, RunRefused: runRefused, AfterCommit: afterCommit}, nil
}
