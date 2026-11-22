package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Failed chained run (CHE-7675). A run that carried an issue into a
// chain_same_owner step and then ended failed or cancelled leaves the issue on
// that step with nobody working it, and no handoff will start a run. One retry
// re-enters the step for its resolved handler; a second consecutive failure
// blocks the issue.

// activityChainedRunEntered records that a run carried the issue into a step
// itself (a chain_same_owner move, no handoff run). Its details name the task
// and the step. A failed run is retried only with this evidence, so a run
// dispatched straight into a flagged step is never mistaken for a chained one.
const activityChainedRunEntered = "chained_run_entered"

// activityChainedRunRetry records the retry a failed chained run got. Its
// details name the step, the failed task and the retry task, so a failure of
// the retry (or of an infrastructure retry of it) is the second one.
const activityChainedRunRetry = "chained_run_retry"

// blockedReasonChainedRunFailed prefixes the blocked_reason of an issue whose
// chained run failed twice on the same step.
const blockedReasonChainedRunFailed = "chained-run-failed"

// chainedRunLineageDepth bounds the walk up a task's retry parents.
const chainedRunLineageDepth = 16

// RegisterChainedRunRetry retries a chained run that ended failed or
// cancelled. Register it after RegisterDeferredWorkflowSwitch: handlers run in
// registration order, and a pending switch must apply before a retry is
// considered (ApplyChainedRunRetry also steps aside for a pending row, so the
// order only decides which of the two does the work first).
func (h *Handler) RegisterChainedRunRetry(bus *events.Bus) {
	for _, eventType := range []string{protocol.EventTaskFailed, protocol.EventTaskCancelled} {
		bus.Subscribe(eventType, func(e events.Event) {
			payload, _ := e.Payload.(map[string]any)
			taskID, _ := payload["task_id"].(string)
			id, err := util.ParseUUID(taskID)
			if err != nil || !id.Valid {
				return
			}
			h.ApplyChainedRunRetry(context.Background(), id)
		})
	}
}

// chainedRunRetryRecord is the details of a chained_run_retry activity row.
type chainedRunRetryRecord struct {
	Step          string `json:"step"`
	FailedTaskID  string `json:"failed_task_id"`
	RetryTaskID   string `json:"retry_task_id"`
	FailureReason string `json:"failure_reason,omitempty"`
}

// ApplyChainedRunRetry handles one terminal non-completed task. It writes
// nothing unless the task is a chained run on its issue's current step, ended
// by the server and not by a person, with no pending switch and no live task on
// the issue, and that itself (or a run it retried) carried the issue into the step. A failure that a repeat can clear is logged; the next terminal
// event tries again.
func (h *Handler) ApplyChainedRunRetry(ctx context.Context, taskID pgtype.UUID) {
	task, err := h.Queries.GetAgentTask(ctx, taskID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("chained run retry: read task failed", "err", err, "task_id", uuidToString(taskID))
		}
		return
	}
	if !chainedRunRetryCandidate(task) {
		return
	}
	if err := h.applyChainedRunRetry(ctx, task); err != nil {
		slog.Warn("chained run retry: not applied", "err", err, "task_id", uuidToString(task.ID), "issue_id", uuidToString(task.IssueID))
	}
}

// chainedRunRetryCandidate is the task-only part of the decision: an issue
// run on a workflow step that ended failed, or cancelled by the server. A
// person's or an agent's cancel is a decision to stop.
func chainedRunRetryCandidate(task db.AgentTaskQueue) bool {
	if !task.IssueID.Valid || !task.WorkflowStep.Valid || task.ChatSessionID.Valid || task.AutopilotRunID.Valid {
		return false
	}
	switch task.Status {
	case "failed":
		return true
	case "cancelled":
		return !task.CancelledByType.Valid || task.CancelledByType.String == "system"
	}
	return false
}

func (h *Handler) applyChainedRunRetry(ctx context.Context, task db.AgentTaskQueue) error {
	lk, err := h.lockDeferredSwitchIssue(ctx, task.IssueID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	defer lk.close(ctx)
	qtx, issue := lk.qtx, lk.issue

	// A pending switch wins: its apply starts the one run, in the new workflow.
	if pending, err := qtx.HasIssueWorkflowSwitchPending(ctx, issue.ID); err != nil || pending {
		return err
	}
	live, err := qtx.HasLiveTaskForIssue(ctx, issue.ID)
	if err != nil || live {
		return err
	}
	if issue.Status != task.WorkflowStep.String || issue.AssigneeType.String != "agent" || issue.AssigneeID != task.AgentID {
		return nil
	}
	def, _, err := issueworkflow.ForIssue(ctx, qtx, issue.WorkspaceID, issue.WorkflowID, issue.ProjectID)
	if err != nil || def == nil {
		return err
	}
	step, ok := def.Step(issue.Status)
	if !ok || !step.ChainSameOwner || !step.HandsOff() {
		return nil
	}

	chained, second, err := h.chainedFailureKind(ctx, qtx, issue, task)
	if err != nil || !chained {
		return err
	}
	if second {
		return h.blockAfterChainedFailures(ctx, lk, task)
	}
	return h.retryChainedRun(ctx, lk, task)
}

// chainedFailureKind reads the failed task's lineage (the task, then its retry
// parents) against the evidence. chained: the task or an ancestor carried the
// issue into the step, or is the retry this feature recorded for it. second:
// the lineage reaches the retry the newest chained_run_retry row recorded for
// this step, so this is the second consecutive failure.
func (h *Handler) chainedFailureKind(ctx context.Context, q *db.Queries, issue db.Issue, task db.AgentTaskQueue) (chained, second bool, err error) {
	step := task.WorkflowStep.String
	var retryTaskID string
	raw, err := q.GetLatestChainedRunRetryDetails(ctx, issue.ID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, false, err
	}
	if err == nil {
		var rec chainedRunRetryRecord
		if json.Unmarshal(raw, &rec) == nil && rec.Step == step {
			retryTaskID = rec.RetryTaskID
		}
	}
	current := task
	for range chainedRunLineageDepth {
		id := uuidToString(current.ID)
		if retryTaskID != "" && id == retryTaskID {
			return true, true, nil
		}
		entered, err := q.HasChainedRunEntered(ctx, db.HasChainedRunEnteredParams{IssueID: issue.ID, TaskID: id, Step: step})
		if err != nil {
			return false, false, err
		}
		chained = chained || entered
		if !current.ParentTaskID.Valid {
			return chained, false, nil
		}
		if current, err = q.GetAgentTask(ctx, current.ParentTaskID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return chained, false, nil
			}
			return false, false, err
		}
	}
	return chained, false, nil
}

// retryChainedRun re-enters the issue's current step, which starts a run for
// the step's resolved handler, and records the retry with the new task.
func (h *Handler) retryChainedRun(ctx context.Context, lk *lockedDeferredSwitchIssue, task db.AgentTaskQueue) error {
	extra := map[string]any{"failed_task_id": uuidToString(task.ID)}
	move, err := h.moveIssueOnEdgeTxAs(ctx, lk.qtx, lk.issue, lk.issue.Status, "chained_run_retry", extra, true, nil, workflowCaller{enterCurrent: true})
	if err != nil {
		var notConfirmed *service.StopNotConfirmedError
		if errors.As(err, &notConfirmed) {
			_ = lk.tx.Rollback(ctx)
			h.recordStopNotConfirmed(ctx, lk.issue, "system", "", notConfirmed)
		}
		return err
	}
	if move.RunRefused != nil {
		return fmt.Errorf("retry run not started: %w", move.RunRefused)
	}
	if !move.TaskID.Valid {
		return errors.New("retry run not started: no task was created")
	}
	details, _ := json.Marshal(chainedRunRetryRecord{
		Step: task.WorkflowStep.String, FailedTaskID: uuidToString(task.ID), RetryTaskID: uuidToString(move.TaskID),
		FailureReason: task.FailureReason.String,
	})
	if _, err := lk.qtx.CreateActivity(ctx, db.CreateActivityParams{
		ID: dbid.NewV7(), WorkspaceID: lk.issue.WorkspaceID, IssueID: lk.issue.ID,
		ActorType: pgtype.Text{String: "system", Valid: true}, Action: activityChainedRunRetry, Details: details,
	}); err != nil {
		return fmt.Errorf("record chained run retry: %w", err)
	}
	if err := lk.tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit chained run retry: %w", err)
	}
	lk.release()
	move.AfterCommit(ctx)
	return nil
}

// blockAfterChainedFailures blocks the issue with the reason and one system
// comment. A workflow with no way to blocked keeps the issue where it is and
// still records the reason, once.
func (h *Handler) blockAfterChainedFailures(ctx context.Context, lk *lockedDeferredSwitchIssue, task db.AgentTaskQueue) error {
	qtx, issue := lk.qtx, lk.issue
	reason := fmt.Sprintf("%s: step %s failed twice (last task %s)", blockedReasonChainedRunFailed, task.WorkflowStep.String, uuidToString(task.ID))
	var move *edgeMove
	if issue.Status != issuestatus.Blocked {
		def, _, err := issueworkflow.ForIssue(ctx, qtx, issue.WorkspaceID, issue.WorkflowID, issue.ProjectID)
		if err != nil {
			return err
		}
		if def != nil && def.Has(issuestatus.Blocked) {
			m, err := h.moveIssueOnEdgeTx(ctx, qtx, issue, issuestatus.Blocked, "chained_run_failed", map[string]any{"reason": reason}, false, nil)
			switch {
			case err == nil:
				move = &m
				issue = m.Issue
			case isWorkflowRefusal(err):
				slog.Warn("chained run retry: workflow refuses blocking, recording the reason only", "err", err, "issue_id", uuidToString(issue.ID))
			default:
				var notConfirmed *service.StopNotConfirmedError
				if errors.As(err, &notConfirmed) {
					_ = lk.tx.Rollback(ctx)
					h.recordStopNotConfirmed(ctx, issue, "system", "", notConfirmed)
				}
				return err
			}
		}
	}
	if recordedBlockedReason(issue.Metadata) == reason {
		return nil
	}
	outcome := "The issue is blocked. Fix the cause, then move it on."
	if move == nil {
		outcome = "The issue stays on its step: its workflow has no way to blocked."
	}
	created, err := qtx.CreateComment(ctx, db.CreateCommentParams{
		ID: dbid.NewV7(), IssueID: issue.ID, WorkspaceID: issue.WorkspaceID,
		AuthorType: "system", AuthorID: pgtype.UUID{Valid: true}, Type: "system",
		Content: "Chained run failed twice (`" + blockedReasonChainedRunFailed + "`): " + reason + "\n\n" + outcome,
	})
	if err != nil {
		return err
	}
	if _, err := setIssueMetadataValue(ctx, qtx, issue.WorkspaceID, issue.ID, "blocked_reason", reason); err != nil {
		return err
	}
	if err := lk.tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit chained run block: %w", err)
	}
	lk.release()
	if move != nil {
		move.AfterCommit(ctx)
	}
	h.publish(protocol.EventCommentCreated, uuidToString(issue.WorkspaceID), "system", "", map[string]any{
		"comment":             commentToResponse(created.Comment(), nil, nil),
		"issue_title":         issue.Title,
		"issue_assignee_type": textToPtr(issue.AssigneeType),
		"issue_assignee_id":   uuidToPtr(issue.AssigneeID),
		"issue_status":        issue.Status,
		"issue_revision":      created.IssueRevision,
	})
	return nil
}
