package service

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Outcome kinds an agent reports at the end of a DM turn (plan r4 T6-T8). "unclassified" is not
// reportable: it is what a completed turn with no outcome is recorded as (T9).
const (
	ChatOutcomeQuestion     = "question"
	ChatOutcomeDone         = "done"
	ChatOutcomeInfo         = "info"
	chatOutcomeUnclassified = "unclassified"
)

var (
	ErrChatOutcomeInvalid     = errors.New("invalid chat outcome")
	ErrChatOutcomeTaskUnknown = errors.New("chat task not found")
	ErrChatOutcomeNotChat     = errors.New("task is not a chat task")
	ErrChatOutcomeTaskEnded   = errors.New("chat task already finished")
)

type ChatOutcomeInput struct {
	Kind   string
	Ask    string   // question: continue this ask
	New    bool     // question: open another ask
	AskIDs []string // done: the asks the agent finished
}

func chatOutcomeInvalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrChatOutcomeInvalid, fmt.Sprintf(format, args...))
}

// RecordChatOutcome applies one agent outcome bound to its own chat task and returns the ask ids it
// concerns. The call is all-or-nothing: an invalid outcome changes no ask and records nothing, so
// the turn finishes unclassified. Ask changes are guarded per ask by the task-order watermark, so a
// repeat or an older task has no effect on an ask a newer outcome already touched (T11).
func (s *TaskService) RecordChatOutcome(ctx context.Context, taskID pgtype.UUID, in ChatOutcomeInput) ([]pgtype.UUID, error) {
	tx, err := s.TxStarter.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	q := s.Queries.WithTx(tx)

	// Session row first, then the task: the repo-wide order every terminal report uses (see
	// lockChatSessionForTaskWrite). A report that settles this task holds the session until it
	// commits, so the status read below is final. No rows: unknown task, or not a chat task.
	if _, err := q.LockChatSessionForTask(ctx, taskID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("lock chat session: %w", err)
	}
	task, err := q.GetAgentTask(ctx, taskID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrChatOutcomeTaskUnknown
	} else if err != nil {
		return nil, fmt.Errorf("load task: %w", err)
	}
	if !task.ChatSessionID.Valid {
		return nil, ErrChatOutcomeNotChat
	}
	if task.Status != "dispatched" && task.Status != "running" {
		return nil, ErrChatOutcomeTaskEnded
	}
	session, err := q.GetChatSession(ctx, task.ChatSessionID)
	if err != nil {
		return nil, fmt.Errorf("load chat session: %w", err)
	}

	scope := db.ListChatAttentionAsksOfAgentParams{ChainRootSessionID: session.ChainRootSessionID, UserID: session.CreatorID, AgentID: task.AgentID}
	var ids []pgtype.UUID
	switch in.Kind {
	case ChatOutcomeQuestion:
		ids, err = recordQuestion(ctx, q, task, session, scope, in)
	case ChatOutcomeDone:
		ids, err = recordDone(ctx, q, task, scope, in)
	case ChatOutcomeInfo:
	default:
		err = chatOutcomeInvalid("kind must be question, done or info")
	}
	if err != nil {
		return nil, err
	}
	if err := q.RecordChatTurnOutcome(ctx, db.RecordChatTurnOutcomeParams{
		TaskID: task.ID, WorkspaceID: session.WorkspaceID, ChatSessionID: session.ID, ChainRootSessionID: session.ChainRootSessionID, UserID: session.CreatorID, Kind: in.Kind,
	}); err != nil {
		return nil, fmt.Errorf("record turn outcome: %w", err)
	}
	return ids, tx.Commit(ctx)
}

// recordQuestion is T6: continue the named ask, or the agent's only open ask, or open a new one.
func recordQuestion(ctx context.Context, q *db.Queries, task db.AgentTaskQueue, session db.ChatSession, scope db.ListChatAttentionAsksOfAgentParams, in ChatOutcomeInput) ([]pgtype.UUID, error) {
	var ids []pgtype.UUID
	switch {
	case in.New && in.Ask != "":
		return nil, chatOutcomeInvalid("use either ask or new, not both")
	case in.Ask != "":
		id, err := util.ParseUUID(in.Ask)
		if err != nil {
			return nil, chatOutcomeInvalid("ask is not an id")
		}
		scope.Ids = []pgtype.UUID{id}
		rows, err := q.ListChatAttentionAsksOfAgent(ctx, scope)
		if err != nil {
			return nil, fmt.Errorf("load ask: %w", err)
		}
		if len(rows) != 1 || !rows[0].Open {
			return nil, chatOutcomeInvalid("ask is not one of your open asks in this DM")
		}
		ids = scope.Ids
	case in.New:
	default:
		open, err := q.ListOpenChatAttentionAsksOfAgent(ctx, db.ListOpenChatAttentionAsksOfAgentParams{
			ChainRootSessionID: scope.ChainRootSessionID, UserID: scope.UserID, AgentID: scope.AgentID,
		})
		if err != nil {
			return nil, fmt.Errorf("list open asks: %w", err)
		}
		if len(open) > 1 {
			return nil, chatOutcomeInvalid("%d open asks: name one with ask, or pass new", len(open))
		}
		ids = open
	}
	if len(ids) == 0 {
		id, err := q.GetChatAttentionAskOpenedByTask(ctx, task.ID) // a retried "new" opens nothing more
		if errors.Is(err, pgx.ErrNoRows) {
			id, err = q.CreateChatAttentionAsk(ctx, db.CreateChatAttentionAskParams{
				WorkspaceID: session.WorkspaceID, ChainRootSessionID: session.ChainRootSessionID, ChatSessionID: session.ID,
				UserID: session.CreatorID, AgentID: task.AgentID, SourceTaskID: task.ID,
			})
		}
		if err != nil {
			return nil, fmt.Errorf("open ask: %w", err)
		}
		return []pgtype.UUID{id}, nil
	}
	if err := q.ContinueChatAttentionAsks(ctx, db.ContinueChatAttentionAsksParams{TaskID: task.ID, Ids: ids}); err != nil {
		return nil, fmt.Errorf("continue asks: %w", err)
	}
	return ids, nil
}

// recordDone is T7: resolve exactly the named asks. One id that is not the agent's ask in this
// chain voids the call. An ask that is already resolved is accepted, so a retry succeeds.
func recordDone(ctx context.Context, q *db.Queries, task db.AgentTaskQueue, scope db.ListChatAttentionAsksOfAgentParams, in ChatOutcomeInput) ([]pgtype.UUID, error) {
	if len(in.AskIDs) == 0 {
		return nil, chatOutcomeInvalid("done needs the ask ids it finishes")
	}
	unique := slices.Clone(in.AskIDs)
	slices.Sort(unique)
	unique = slices.Compact(unique)
	for _, s := range unique {
		id, err := util.ParseUUID(s)
		if err != nil {
			return nil, chatOutcomeInvalid("%q is not an ask id", s)
		}
		scope.Ids = append(scope.Ids, id)
	}
	rows, err := q.ListChatAttentionAsksOfAgent(ctx, scope)
	if err != nil {
		return nil, fmt.Errorf("load asks: %w", err)
	}
	if len(rows) != len(scope.Ids) {
		return nil, chatOutcomeInvalid("every ask must be one of your asks in this DM")
	}
	if err := q.ResolveChatAttentionAsksDone(ctx, db.ResolveChatAttentionAsksDoneParams{TaskID: task.ID, Ids: scope.Ids}); err != nil {
		return nil, fmt.Errorf("resolve asks: %w", err)
	}
	return scope.Ids, nil
}

// settleChatTurns is T9 and T10 for tasks entering a terminal state: asks that waited on the task
// return to Cheese however it ended, and a completed turn with no reported outcome is recorded as
// unclassified. It runs inside SettleTerminalTaskState, on the status write's transaction.
func settleChatTurns(ctx context.Context, q *db.Queries, tasks []db.AgentTaskQueue) error {
	ids := make([]pgtype.UUID, 0, len(tasks))
	for _, task := range tasks {
		if task.ChatSessionID.Valid {
			ids = append(ids, task.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	if err := q.ClearChatAttentionWaitingForTasks(ctx, ids); err != nil {
		return fmt.Errorf("clear attention waiting: %w", err)
	}
	if err := q.RecordUnclassifiedChatTurns(ctx, ids); err != nil {
		return fmt.Errorf("record unclassified turns: %w", err)
	}
	return nil
}
