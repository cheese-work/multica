package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Scheduling pause stops claim, not enqueue: queued tasks wait, running tasks
// finish. The fence itself lives in the claim queries (agent.sql); these
// methods only flip the columns and, on resume, invalidate the empty-claim
// verdict so a daemon that cached "nothing to claim" while paused polls again.
//
// Each method reports whether this call made the transition. The prior state is
// read under a row lock in the same transaction as the write, so of n racing
// callers exactly one sees changed == true.

func (s *TaskService) beginScheduling(ctx context.Context) (pgx.Tx, *db.Queries, error) {
	if s.TxStarter == nil {
		return nil, nil, errors.New("scheduling pause requires transaction support")
	}
	tx, err := s.TxStarter.Begin(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("begin scheduling change: %w", err)
	}
	return tx, s.Queries.WithTx(tx), nil
}

// AgentSchedulingHook runs on the scheduling change's own transaction, after
// the write and before commit, with the agent as it was and as it is now. An
// error rolls the change back (the admin audit row, CHE-5646).
type AgentSchedulingHook func(ctx context.Context, q *db.Queries, prev, updated db.Agent) error

// reason is optional free text kept with the pause; the first pause wins.
func (s *TaskService) PauseAgentScheduling(ctx context.Context, workspaceID, agentID, actorID pgtype.UUID, reason pgtype.Text) (db.Agent, bool, error) {
	return s.PauseAgentSchedulingInTx(ctx, workspaceID, agentID, actorID, reason, nil)
}

func (s *TaskService) PauseAgentSchedulingInTx(ctx context.Context, workspaceID, agentID, actorID pgtype.UUID, reason pgtype.Text, inTx AgentSchedulingHook) (db.Agent, bool, error) {
	return s.setAgentScheduling(ctx, workspaceID, agentID, func(q *db.Queries) (db.Agent, error) {
		return q.PauseAgentScheduling(ctx, db.PauseAgentSchedulingParams{
			ID: agentID, WorkspaceID: workspaceID, ActorID: actorID, Reason: reason,
		})
	}, true, inTx)
}

func (s *TaskService) ResumeAgentScheduling(ctx context.Context, workspaceID, agentID pgtype.UUID) (db.Agent, bool, error) {
	return s.ResumeAgentSchedulingInTx(ctx, workspaceID, agentID, nil)
}

func (s *TaskService) ResumeAgentSchedulingInTx(ctx context.Context, workspaceID, agentID pgtype.UUID, inTx AgentSchedulingHook) (db.Agent, bool, error) {
	agent, changed, err := s.setAgentScheduling(ctx, workspaceID, agentID, func(q *db.Queries) (db.Agent, error) {
		return q.ResumeAgentScheduling(ctx, db.ResumeAgentSchedulingParams{
			ID: agentID, WorkspaceID: workspaceID,
		})
	}, false, inTx)
	if err != nil {
		return db.Agent{}, false, err
	}
	s.notifyRuntimeMayHaveWork(agent.RuntimeID, "")
	return agent, changed, nil
}

// setAgentScheduling locks the agent row, runs write, and reports whether the
// paused flag moved to pause.
func (s *TaskService) setAgentScheduling(ctx context.Context, workspaceID, agentID pgtype.UUID, write func(*db.Queries) (db.Agent, error), pause bool, inTx AgentSchedulingHook) (db.Agent, bool, error) {
	tx, q, err := s.beginScheduling(ctx)
	if err != nil {
		return db.Agent{}, false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	prev, err := q.GetAgentForUpdate(ctx, agentID)
	if err != nil {
		return db.Agent{}, false, err
	}
	if prev.WorkspaceID != workspaceID {
		return db.Agent{}, false, pgx.ErrNoRows
	}
	updated, err := write(q)
	if err != nil {
		return db.Agent{}, false, err
	}
	if inTx != nil {
		if err := inTx(ctx, q, prev, updated); err != nil {
			return db.Agent{}, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return db.Agent{}, false, fmt.Errorf("commit scheduling change: %w", err)
	}
	return updated, prev.SchedulingPausedAt.Valid != pause, nil
}

// WorkspaceSchedulingAuditFn writes the audit row for a workspace pause or
// resume inside the transaction that applies it, with the row before and after.
// It runs only when the call changes the state; an error rolls the change back.
type WorkspaceSchedulingAuditFn func(ctx context.Context, q *db.Queries, prev, updated db.Workspace) error

func (s *TaskService) PauseWorkspaceScheduling(ctx context.Context, workspaceID, actorID pgtype.UUID, audit WorkspaceSchedulingAuditFn) (db.Workspace, bool, error) {
	return s.setWorkspaceScheduling(ctx, workspaceID, func(q *db.Queries) (db.Workspace, error) {
		return q.PauseWorkspaceScheduling(ctx, db.PauseWorkspaceSchedulingParams{
			ID: workspaceID, ActorID: actorID,
		})
	}, true, audit)
}

func (s *TaskService) ResumeWorkspaceScheduling(ctx context.Context, workspaceID pgtype.UUID, audit WorkspaceSchedulingAuditFn) (db.Workspace, bool, error) {
	// List before resuming: a failure here leaves the workspace paused instead
	// of resumed with no runtime woken.
	runtimeIDs, err := s.Queries.ListAgentRuntimeIDsByWorkspace(ctx, workspaceID)
	if err != nil {
		return db.Workspace{}, false, err
	}
	ws, changed, err := s.setWorkspaceScheduling(ctx, workspaceID, func(q *db.Queries) (db.Workspace, error) {
		return q.ResumeWorkspaceScheduling(ctx, workspaceID)
	}, false, audit)
	if err != nil {
		return db.Workspace{}, false, err
	}
	for _, id := range runtimeIDs {
		s.notifyRuntimeMayHaveWork(id, "")
	}
	return ws, changed, nil
}

func (s *TaskService) setWorkspaceScheduling(ctx context.Context, workspaceID pgtype.UUID, write func(*db.Queries) (db.Workspace, error), pause bool, audit WorkspaceSchedulingAuditFn) (db.Workspace, bool, error) {
	tx, q, err := s.beginScheduling(ctx)
	if err != nil {
		return db.Workspace{}, false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	prev, err := q.GetWorkspaceForUpdate(ctx, workspaceID)
	if err != nil {
		return db.Workspace{}, false, err
	}
	updated, err := write(q)
	if err != nil {
		return db.Workspace{}, false, err
	}
	changed := prev.SchedulingPausedAt.Valid != pause
	if changed && audit != nil {
		if err := audit(ctx, q, prev, updated); err != nil {
			return db.Workspace{}, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return db.Workspace{}, false, fmt.Errorf("commit scheduling change: %w", err)
	}
	return updated, changed, nil
}

// NotifyRuntimeMayHaveWork is ResumeAgentScheduling's wake-up for a caller that
// resumed the agent inside its own transaction (an approved decision action).
func (s *TaskService) NotifyRuntimeMayHaveWork(runtimeID pgtype.UUID, taskID string) {
	s.notifyRuntimeMayHaveWork(runtimeID, taskID)
}
