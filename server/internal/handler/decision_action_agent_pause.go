package handler

import (
	"context"
	"encoding/json"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const (
	agentPauseAction  = "agent.pause"
	agentResumeAction = "agent.resume"
)

// executeAgentPauseAction is the same write as the pause endpoint, run in the
// approval transaction. Queued tasks wait and running tasks finish. The
// approver is recorded in scheduling_paused_by; the approved action row is the
// reason and audit.
func executeAgentPauseAction(ctx context.Context, execution decisionActionExecution) (json.RawMessage, error) {
	if execution.Agent.ArchivedAt.Valid {
		return nil, actionFailure{Code: "agent_archived", Message: "the target agent is archived"}
	}
	if _, err := execution.Queries.PauseAgentScheduling(ctx, db.PauseAgentSchedulingParams{
		ID: execution.Agent.ID, WorkspaceID: execution.Agent.WorkspaceID, ActorID: execution.Approver.UserID,
	}); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"agent_id": uuidToString(execution.Agent.ID), "scheduling_paused": true})
}

func executeAgentResumeAction(ctx context.Context, execution decisionActionExecution) (json.RawMessage, error) {
	if _, err := execution.Queries.ResumeAgentScheduling(ctx, db.ResumeAgentSchedulingParams{
		ID: execution.Agent.ID, WorkspaceID: execution.Agent.WorkspaceID,
	}); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"agent_id": uuidToString(execution.Agent.ID), "scheduling_paused": false})
}

// publishAgentSchedulingChanged runs after commit: it sends the same event as
// the pause endpoint and, on resume, bumps the empty-claim cache so a daemon
// that cached "nothing to claim" while paused polls again. It is idempotent.
func (handler *Handler) publishAgentSchedulingChanged(ctx context.Context, action db.DecisionAction) error {
	agentID, ok := decisionActionTarget(action)
	if !ok {
		return nil
	}
	agent, err := handler.Queries.GetAgent(ctx, agentID)
	if err != nil {
		return err
	}
	eventType := protocol.EventAgentSchedulingResumed
	if action.ActionType == agentPauseAction {
		eventType = protocol.EventAgentSchedulingPaused
	} else {
		handler.TaskService.NotifyRuntimeMayHaveWork(agent.RuntimeID)
	}
	resp := handler.agentToResponse(agent)
	if err := handler.attachAgentSkills(ctx, &resp, agent.ID); err != nil {
		return err
	}
	handler.publish(eventType, uuidToString(action.WorkspaceID), "member", uuidToString(action.ApprovedByUserID), map[string]any{"agent": broadcastAgentResponse(resp)})
	return nil
}
