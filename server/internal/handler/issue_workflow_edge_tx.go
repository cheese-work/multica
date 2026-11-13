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
	// StatusActivityID is the history row the move wrote; invalid when none.
	StatusActivityID pgtype.UUID
	// RunRefused is why the next step's run was not inserted (budget, triage,
	// a pending run already holds the slot). The move is written regardless; a
	// caller that needs both or neither rolls back on it.
	RunRefused error
	// TaskID is the run that carries the move: the one the move inserted, or the
	// pending run that held the slot (a duplicate). Invalid when no run was
	// started or found (CHE-2860).
	TaskID pgtype.UUID
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
// nothing written. The move reads through q wherever it can, so the caller's
// connection is the one it waits on. overlay is the next run's Composio
// overlay, built before the transaction opened (prepareEdgeRunOverlay).
func (h *Handler) moveIssueOnEdgeTx(ctx context.Context, q *db.Queries, issue db.Issue, to, source string, extra map[string]any, startHandler bool, overlay *service.PreparedRuntimeMCPOverlay) (edgeMove, error) {
	th := h.onQueries(q)
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
	rec := &statusRecord{workspaceID: issue.WorkspaceID, trigger: statusTriggerFromSystem(edgeMoveCause(source))}
	// The block an exhausted budget causes names its refusal, so a later refusal
	// can find the status and target the block hid.
	if id, ok := extra["refusal_id"].(string); ok && source == "run_budget" {
		rec.trigger.Cause.Ref = map[string]string{"refusal_id": id}
	}
	// An approved action's restore names the action that paid for it.
	if id, ok := extra["action_id"].(string); ok && source == "run_budget" {
		rec.trigger.Cause.Ref = map[string]string{"action_id": id}
	}
	// A merge names the PRs that moved the issue, in the timeline and in the row.
	if source == "pr_automation" {
		if prs, ok := extra["pull_requests"].(string); ok {
			rec.trigger.Cause.Ref = map[string]string{"pr": prs}
		}
		rec.trigger.Cause.PRs, _ = extra["pr_refs"].([]service.StatusPRRef)
	}
	if handoff != nil {
		rec.handoffWorkflow = handoff.def.Name
	}
	updated, cancelledWakeups, err := h.writeIssueLocked(withStatusRecord(ctx, rec), q, issue.WorkspaceID, params, write)
	if err != nil {
		return edgeMove{}, fmt.Errorf("status write failed: %w", err)
	}

	assigneeChanged := handoff != nil &&
		(issue.AssigneeType.String != updated.AssigneeType.String || uuidToString(issue.AssigneeID) != uuidToString(updated.AssigneeID))
	var willRun bool
	var trigger service.IssueRunTrigger
	if handoff != nil && startHandler {
		trigger, willRun = th.IssueService.WillEnqueueRun(ctx, service.IssueTriggerInput{
			Issue: updated, PrevStatus: issue.Status, AssigneeChanged: true, StatusChanged: true, EnteredHandoffStep: true, RecordCoalesced: true,
		}, service.IssueTriggerProbe{})
	}
	var handoffDetails map[string]string
	if handoff != nil {
		handoffDetails = workflowHandoffPayload(updated, handoff, th.workflowHandoffBrief(ctx, updated, handoff), willRun, "")
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
		runTaskID    pgtype.UUID
		runRefused   error
		cancelAfter  func()
		duplicateRun bool
	)
	// A handler step whose agent is archived runs nothing: the caller that needs
	// the run (a budget resume) sees why and rolls the move back.
	if handoff != nil && startHandler && !willRun && updated.AssigneeType.String == "agent" {
		if agent, err := q.GetAgent(ctx, updated.AssigneeID); err == nil && agent.ArchivedAt.Valid {
			runRefused = service.ErrAgentArchived
		}
	}
	if willRun {
		var notConfirmed *service.StopNotConfirmedError
		if err := service.StopGate(ctx, q, updated.ID); errors.As(err, &notConfirmed) {
			return edgeMove{}, err
		}
		if cancelAfter, err = th.TaskService.CancelStaleStepRunsInTx(ctx, q, updated, trigger.AgentID); err != nil {
			return edgeMove{}, err
		}
		if trigger.AssigneeType == "agent" {
			runTaskID, runAfter, err = th.TaskService.EnqueueIssueRunInTx(ctx, q, updated, pgtype.UUID{}, pgtype.UUID{}, overlay)
		} else {
			runTaskID, runAfter, err = th.enqueueSquadLeaderRunInTx(ctx, q, updated)
		}
		duplicateRun = errors.Is(err, service.ErrDuplicatePendingTask)
		switch {
		case err == nil:
		case duplicateRun, errors.Is(err, service.ErrRunBudgetExhausted), errors.Is(err, service.ErrStageQueueFull), errors.Is(err, service.ErrAdmissionDeferred), errors.Is(err, service.ErrReviewHeadAnswered), errors.Is(err, service.ErrIssueInTriage), errors.Is(err, service.ErrAgentPaused),
			errors.Is(err, service.ErrRecoveryCaused), errors.Is(err, errSquadLeaderNotAdmitted):
			runRefused = err
		default:
			return edgeMove{}, err
		}
	}

	if errors.Is(runRefused, service.ErrStageQueueFull) && handoffDetails != nil {
		handoffDetails["run"] = "false"
		handoffDetails["run_reason"] = handoffRunReasonStageQueueFull
		delete(handoffDetails, "note")
	}

	payload := map[string]any{
		"assignee_changed": assigneeChanged,
		"status_changed":   true,
		"prev_status":      issue.Status,
		"creator_type":     issue.CreatorType,
		"creator_id":       uuidToString(issue.CreatorID),
		"source":           source,
	}
	for k, v := range extra {
		if k != "pr_refs" {
			payload[k] = v
		}
	}
	if assigneeChanged {
		payload["prev_assignee_type"] = textToPtr(issue.AssigneeType)
		payload["prev_assignee_id"] = uuidToPtr(issue.AssigneeID)
	}
	if handoffDetails != nil {
		payload["workflow_handoff"] = handoffDetails
	}
	if rec.activity != nil {
		payload["activity_id"] = uuidToString(rec.activity.ID)
	}

	afterCommit := func(ctx context.Context) (ranHandler bool) {
		payload["issue"] = h.issueResponseWithCategory(ctx, updated)
		h.broadcastCancelledWakeups(ctx, issue.WorkspaceID, cancelledWakeups)
		h.finishRecordedHandoff(ctx, rec, handoffDetails)
		h.publish(protocol.EventIssueUpdated, uuidToString(updated.WorkspaceID), "system", "", payload)
		h.publishRecordedStatusActivity(rec, "system", "")
		if cancelAfter != nil {
			cancelAfter()
		}
		switch {
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
	var activityID pgtype.UUID
	if rec.activity != nil {
		activityID = rec.activity.ID
	}
	return edgeMove{Issue: updated, StatusActivityID: activityID, RunRefused: runRefused, TaskID: runTaskID, AfterCommit: afterCommit}, nil
}

// prepareEdgeRunOverlay builds, outside any transaction, the Composio overlay
// of the run moving issue to `to` would insert: it decides the move's handoff
// the way moveIssueOnEdgeTx will and builds the overlay for that handler. Nil
// when no agent run follows. If the workflow changes before the move, the
// overlay no longer matches the run's agent and the run takes none.
func (h *Handler) prepareEdgeRunOverlay(ctx context.Context, issue db.Issue, to string, startHandler bool) *service.PreparedRuntimeMCPOverlay {
	if !startHandler {
		return nil
	}
	params := db.UpdateIssueParams{
		ID:           issue.ID,
		Status:       pgtype.Text{String: to, Valid: true},
		AssigneeType: issue.AssigneeType,
		AssigneeID:   issue.AssigneeID,
		ProjectID:    issue.ProjectID,
	}
	handoff, err := h.applyIssueWorkflow(ctx, h.Queries, issue, &params, true, false, false, workflowCaller{})
	if err != nil || handoff == nil || params.AssigneeType.String != "agent" {
		return nil
	}
	issue.Status, issue.AssigneeType, issue.AssigneeID = to, params.AssigneeType, params.AssigneeID
	return h.TaskService.PrepareIssueRunOverlay(ctx, issue, pgtype.UUID{})
}

// errSquadLeaderNotAdmitted is why a squad's leader got no run: the access gate
// refused it, or the squad is gone.
var errSquadLeaderNotAdmitted = errors.New("squad leader run not admitted")

// enqueueSquadLeaderRunInTx is the in-transaction form of
// enqueueSquadLeaderTaskOutcome for the platform as the actor: the leader's
// access gate and the run insert both read and write through q.
func (h *Handler) enqueueSquadLeaderRunInTx(ctx context.Context, q *db.Queries, issue db.Issue) (pgtype.UUID, func(), error) {
	return h.enqueueSquadLeaderRunAsInTx(ctx, q, issue, pgtype.UUID{}, "system", "")
}

// enqueueSquadLeaderRunAsInTx is enqueueSquadLeaderRunInTx for a caller: a
// member who moves the issue is the leader run's accountable human, so the gate
// judges that member, never the issue's creator (mirrors
// enqueueSquadLeaderTaskOutcome).
func (h *Handler) enqueueSquadLeaderRunAsInTx(ctx context.Context, q *db.Queries, issue db.Issue, triggerCommentID pgtype.UUID, actorType, actorID string) (pgtype.UUID, func(), error) {
	squad, err := q.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: issue.AssigneeID, WorkspaceID: issue.WorkspaceID})
	if err != nil {
		return pgtype.UUID{}, nil, errSquadLeaderNotAdmitted
	}
	originator := actorID
	if actorType != "member" {
		originator = uuidToString(h.TaskService.OriginatorForIssueTask(ctx, issue, triggerCommentID))
	}
	if !h.canEnqueueSquadLeader(ctx, squad.LeaderID, actorType, actorID, originator, uuidToString(issue.WorkspaceID)) {
		return pgtype.UUID{}, nil, errSquadLeaderNotAdmitted
	}
	return h.TaskService.EnqueueSquadLeaderRunInTx(ctx, q, issue, squad.LeaderID, squad.ID, triggerCommentID, memberActorUserID(actorType, actorID))
}

// onQueries is a handler whose reads go through q, the queries of a
// transaction the caller holds open: a read through the pool would wait for a
// second connection while this one is held, and enough such moves at once
// starve the pool. Only the fields the edge move reads are carried: copying
// all of *h would read FeatureFlags, which a test rewrites while a detached
// run-budget goroutine is still moving an edge.
func (h *Handler) onQueries(q *db.Queries) *Handler {
	th := &Handler{Queries: q, TaskService: h.TaskService.TxScoped(q)}
	if h.IssueService != nil {
		issues := *h.IssueService
		issues.Queries = q
		issues.TaskService = th.TaskService
		th.IssueService = &issues
	}
	return th
}
