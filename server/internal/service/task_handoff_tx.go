package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// CancelStaleStepRunsInTx is CancelStaleStepRuns with the caller's queries: the
// agent's pending runs stamped for a step the issue has left are cancelled in
// the caller's transaction, so the run for the step it entered can take the
// pending-run slot in the same commit. The returned func publishes the
// cancellations and runs after commit.
func (s *TaskService) CancelStaleStepRunsInTx(ctx context.Context, q *db.Queries, issue db.Issue, agentID pgtype.UUID) (afterCommit func(), err error) {
	stale, err := q.ListStalePendingStepTasks(ctx, db.ListStalePendingStepTasksParams{IssueID: issue.ID, AgentID: agentID, Step: issue.Status})
	if err != nil {
		return nil, fmt.Errorf("list stale step runs: %w", err)
	}
	var cancelled []db.AgentTaskQueue
	for _, task := range stale {
		// Conditional at the statement: a run that started after the selection
		// is in flight and stays.
		done, err := q.CancelStalePendingStepTask(ctx, db.CancelStalePendingStepTaskParams{ID: task.ID, Step: issue.Status})
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("cancel stale step run: %w", err)
		}
		if err := q.MarkPiTaskProcessCleanupPending(ctx, done.ID); err != nil {
			return nil, fmt.Errorf("mark cancelled runtime cleanup pending: %w", err)
		}
		if err := SettleTerminalTaskState(ctx, q, done); err != nil {
			return nil, err
		}
		cancelled = append(cancelled, done)
	}
	return func() {
		for _, task := range cancelled {
			slog.Info("task cancelled", "task_id", util.UUIDToString(task.ID), "issue_id", util.UUIDToString(task.IssueID))
			s.captureTaskCancelled(ctx, task)
			s.ReconcileAgentStatus(ctx, task.AgentID)
			s.broadcastTaskEvent(ctx, protocol.EventTaskCancelled, task)
			s.NotifyTaskFinished(task)
		}
	}, nil
}

// EnqueueIssueRunInTx inserts the run an assignment to the issue's agent
// starts, with the caller's queries: the run commits or rolls back with the
// caller's transaction. A pending assignment-level run of the agent
// is checked under the caller's issue row lock, so the insert never trips the
// pending-run unique index, whose violation would abort the transaction; it is
// ErrDuplicatePendingTask. The admission refusals (run budget, triage, a
// recovery-caused run) are returned as they are, with nothing inserted. The
// returned func publishes the run after commit.
func (s *TaskService) EnqueueIssueRunInTx(ctx context.Context, q *db.Queries, issue db.Issue, actorUserID pgtype.UUID) (afterCommit func(), err error) {
	pending, err := q.HasPendingAssignmentTaskForAgent(ctx, db.HasPendingAssignmentTaskForAgentParams{IssueID: issue.ID, AgentID: issue.AssigneeID})
	if err != nil {
		return nil, fmt.Errorf("check pending run: %w", err)
	}
	if pending {
		return nil, ErrDuplicatePendingTask
	}
	txCtx, notifyAfterCommit := deferEnqueuedTaskHook(ctx)
	task, attr, err := s.enqueueIssueTaskWithQueries(txCtx, q, issue, pgtype.UUID{}, nil, false, "", actorUserID, pgtype.UUID{}, pgtype.Timestamptz{}, OriginDerived)
	if err != nil {
		return nil, err
	}
	return func() {
		notifyAfterCommit()
		s.RecordHumanDelegation(ctx, attr)
		s.PublishTaskEnqueued(ctx, task)
	}, nil
}

// EnqueueSquadLeaderRunInTx inserts the squad leader's run an assignment to a
// squad starts, with the caller's queries. A pending run of the leader on the
// reviewed head is ErrDuplicatePendingTask, checked under the caller's issue
// row lock so the insert never trips the unique index. The returned func
// publishes the run after commit.
func (s *TaskService) EnqueueSquadLeaderRunInTx(ctx context.Context, q *db.Queries, issue db.Issue, leaderID, squadID, actorUserID pgtype.UUID) (afterCommit func(), err error) {
	pending, err := q.HasPendingTaskForIssueAndAgent(ctx, db.HasPendingTaskForIssueAndAgentParams{
		IssueID: issue.ID,
		AgentID: leaderID,
		HeadSha: s.ResolveIssueReviewSHAParam(ctx, issue.ID),
	})
	if err != nil {
		return nil, fmt.Errorf("check pending leader run: %w", err)
	}
	if pending {
		return nil, ErrDuplicatePendingTask
	}
	params, attr, err := s.prepareMentionTaskWithQueries(ctx, q, issue, leaderID, pgtype.UUID{}, nil, true, squadID, false, "", actorUserID, pgtype.UUID{}, OriginDerived)
	if err != nil {
		return nil, err
	}
	params.FreshRerun = pgtype.Bool{Bool: false, Valid: true}
	task, err := s.enqueuePreparedInTx(ctx, q, issue, params, OriginDerived, "squad leader callback")
	if err != nil {
		return nil, err
	}
	return func() {
		notifyTaskEnqueued(ctx, task)
		s.RecordHumanDelegation(ctx, attr)
		s.PublishTaskEnqueued(ctx, task)
	}, nil
}
