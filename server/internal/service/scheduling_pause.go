package service

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Scheduling pause stops claim, not enqueue: queued tasks wait, running tasks
// finish. The fence itself lives in the claim queries (agent.sql); these
// methods only flip the columns and, on resume, invalidate the empty-claim
// verdict so a daemon that cached "nothing to claim" while paused polls again.

func (s *TaskService) PauseAgentScheduling(ctx context.Context, workspaceID, agentID, actorID pgtype.UUID) (db.Agent, error) {
	return s.Queries.PauseAgentScheduling(ctx, db.PauseAgentSchedulingParams{
		ID: agentID, WorkspaceID: workspaceID, ActorID: actorID,
	})
}

func (s *TaskService) ResumeAgentScheduling(ctx context.Context, workspaceID, agentID pgtype.UUID) (db.Agent, error) {
	agent, err := s.Queries.ResumeAgentScheduling(ctx, db.ResumeAgentSchedulingParams{
		ID: agentID, WorkspaceID: workspaceID,
	})
	if err != nil {
		return db.Agent{}, err
	}
	s.notifyRuntimeMayHaveWork(agent.RuntimeID, "")
	return agent, nil
}

func (s *TaskService) PauseWorkspaceScheduling(ctx context.Context, workspaceID, actorID pgtype.UUID) (db.Workspace, error) {
	return s.Queries.PauseWorkspaceScheduling(ctx, db.PauseWorkspaceSchedulingParams{
		ID: workspaceID, ActorID: actorID,
	})
}

func (s *TaskService) ResumeWorkspaceScheduling(ctx context.Context, workspaceID pgtype.UUID) (db.Workspace, error) {
	ws, err := s.Queries.ResumeWorkspaceScheduling(ctx, workspaceID)
	if err != nil {
		return db.Workspace{}, err
	}
	runtimeIDs, err := s.Queries.ListAgentRuntimeIDsByWorkspace(ctx, workspaceID)
	if err != nil {
		return db.Workspace{}, err
	}
	for _, id := range runtimeIDs {
		s.notifyRuntimeMayHaveWork(id, "")
	}
	return ws, nil
}
