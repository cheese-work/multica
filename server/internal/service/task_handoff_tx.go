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

// TxScoped is a TaskService whose reads and writes go through q, the queries
// of a transaction the caller holds open. A read through the pool would wait
// for a second connection while the caller holds one, and enough concurrent
// callers exhaust the pool. Configuration is shared; the locks and caches are
// not copied, so the copy is for one transaction's preparation and insert. The
// func a *InTx method returns runs on the original service, after the commit.
// Composio is absent, as in createDeferredChannelIssueTaskWithQueries: building
// its overlay is a network call and reads through the pool, neither of which
// can run under the caller's locks. A run inserted this way has the overlay
// the caller prepared before its transaction, or none.
func (s *TaskService) TxScoped(q *db.Queries) *TaskService {
	return &TaskService{
		RecordMemberActivity: s.RecordMemberActivity,
		Queries:              q,
		TxStarter:            s.TxStarter,
		Hub:                  s.Hub,
		Bus:                  s.Bus,
		Analytics:            s.Analytics,
		Metrics:              s.Metrics,
		Wakeup:               s.Wakeup,
		PRSnapshotsEnabled:   s.PRSnapshotsEnabled,
		WorkflowEdges:        s.WorkflowEdges,
		PRMergeOwed:          s.PRMergeOwed,
		PRMergeOwe:           s.PRMergeOwe,
		Entitlements:         s.Entitlements,
		SourceContextStorage: s.SourceContextStorage,
		FeatureFlags:         s.FeatureFlags,
		EmptyClaim:           s.EmptyClaim,
		ReclaimCheck:         s.ReclaimCheck,
		QuickActions:         s.QuickActions,
		ProviderFailureProbe: s.ProviderFailureProbe,
		committed:            s.afterCommit(),
	}
}

// afterCommit is the service that runs the work following a commit.
func (s *TaskService) afterCommit() *TaskService {
	if s.committed != nil {
		return s.committed
	}
	return s
}

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
	after := s.afterCommit()
	return func() {
		for _, task := range cancelled {
			slog.Info("task cancelled", "task_id", util.UUIDToString(task.ID), "issue_id", util.UUIDToString(task.IssueID))
			after.captureTaskCancelled(ctx, task)
			after.ReconcileAgentStatus(ctx, task.AgentID)
			after.broadcastTaskEvent(ctx, protocol.EventTaskCancelled, task)
			after.NotifyTaskFinished(task)
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
// returned func publishes the run after commit. overlay is the run's Composio
// overlay, built before the transaction opened (PrepareIssueRunOverlay); a run
// inserted without one has none. taskID is the run inserted, or the pending run
// that held the slot when the error is ErrDuplicatePendingTask.
func (s *TaskService) EnqueueIssueRunInTx(ctx context.Context, q *db.Queries, issue db.Issue, triggerCommentID, actorUserID pgtype.UUID, overlay *PreparedRuntimeMCPOverlay) (taskID pgtype.UUID, afterCommit func(), err error) {
	survivor, err := q.PendingAssignmentTaskForAgent(ctx, db.PendingAssignmentTaskForAgentParams{IssueID: issue.ID, AgentID: issue.AssigneeID})
	if err == nil {
		return survivor, nil, s.noteCoalescedIn(ctx, q, issue, issue.AssigneeID, triggerCommentID, "assignment")
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, nil, fmt.Errorf("check pending run: %w", err)
	}
	txCtx, notifyAfterCommit := deferEnqueuedTaskHook(ctx)
	task, attr, err := s.enqueueIssueTaskWithQueriesAndOverlay(txCtx, q, issue, triggerCommentID, nil, false, "", actorUserID, pgtype.UUID{}, pgtype.Timestamptz{}, OriginDerived, overlay)
	if err != nil {
		return pgtype.UUID{}, nil, err
	}
	after := s.afterCommit()
	return task.ID, func() {
		notifyAfterCommit()
		after.RecordHumanDelegation(ctx, attr)
		after.PublishTaskEnqueued(ctx, task)
	}, nil
}

// EnqueueSquadLeaderRunInTx inserts the squad leader's run an assignment to a
// squad starts, with the caller's queries. A pending run of the leader on the
// reviewed head is ErrDuplicatePendingTask, checked under the caller's issue
// row lock so the insert never trips the unique index. The returned func
// publishes the run after commit. taskID is as for EnqueueIssueRunInTx.
func (s *TaskService) EnqueueSquadLeaderRunInTx(ctx context.Context, q *db.Queries, issue db.Issue, leaderID, squadID, triggerCommentID, actorUserID pgtype.UUID) (taskID pgtype.UUID, afterCommit func(), err error) {
	survivor, err := q.PendingTaskForIssueAndAgent(ctx, db.PendingTaskForIssueAndAgentParams{
		IssueID: issue.ID,
		AgentID: leaderID,
		HeadSha: s.ResolveIssueReviewSHAParam(ctx, issue.ID),
	})
	if err == nil {
		return survivor, nil, s.noteCoalescedIn(ctx, q, issue, leaderID, triggerCommentID, "squad_leader")
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, nil, fmt.Errorf("check pending leader run: %w", err)
	}
	params, attr, err := s.prepareMentionTaskWithQueries(ctx, q, issue, leaderID, triggerCommentID, nil, true, squadID, false, "", actorUserID, pgtype.UUID{}, OriginDerived)
	if err != nil {
		return pgtype.UUID{}, nil, err
	}
	params.FreshRerun = pgtype.Bool{Bool: false, Valid: true}
	task, err := s.enqueuePreparedInTx(ctx, q, issue, params, OriginDerived, "squad leader callback")
	if err != nil {
		return pgtype.UUID{}, nil, err
	}
	after := s.afterCommit()
	return task.ID, func() {
		notifyTaskEnqueued(ctx, task)
		after.RecordHumanDelegation(ctx, attr)
		after.PublishTaskEnqueued(ctx, task)
	}, nil
}
