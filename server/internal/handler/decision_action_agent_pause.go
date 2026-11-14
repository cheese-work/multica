package handler

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const (
	agentPauseAction  = "agent.pause"
	agentResumeAction = "agent.resume"
)

func isAgentSchedulingAction(actionType string) bool {
	return actionType == agentPauseAction || actionType == agentResumeAction
}

// executeAgentPauseAction is the same write as the scheduling-pause endpoint,
// run in the approval transaction. A repeat keeps the first actor and time.
func executeAgentPauseAction(ctx context.Context, execution decisionActionExecution) (json.RawMessage, error) {
	if execution.Agent.ArchivedAt.Valid {
		return nil, actionFailure{Code: "agent_archived", Message: "the target agent is archived"}
	}
	updated, err := execution.Queries.PauseAgentScheduling(ctx, db.PauseAgentSchedulingParams{
		ID: execution.Agent.ID, WorkspaceID: execution.Agent.WorkspaceID, ActorID: execution.Approver.UserID,
	})
	if err != nil {
		return nil, err
	}
	// changed and paused_at let a revoke prove the pause is still this approval's
	// own: only a pause that moved the agent from running to paused is undone.
	return json.Marshal(map[string]any{
		"agent_id": uuidToString(updated.ID), "scheduling_paused": true,
		"changed": !execution.Agent.SchedulingPausedAt.Valid, "paused_at": updated.SchedulingPausedAt.Time.UTC().Format(time.RFC3339Nano),
	})
}

func executeAgentResumeAction(ctx context.Context, execution decisionActionExecution) (json.RawMessage, error) {
	if execution.Agent.ArchivedAt.Valid {
		return nil, actionFailure{Code: "agent_archived", Message: "the target agent is archived"}
	}
	updated, err := execution.Queries.ResumeAgentScheduling(ctx, db.ResumeAgentSchedulingParams{
		ID: execution.Agent.ID, WorkspaceID: execution.Agent.WorkspaceID,
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"agent_id": uuidToString(updated.ID), "scheduling_paused": false})
}

// publishAgentSchedulingChanged tells open clients the agent's paused flag
// moved, and on resume wakes the runtime so a cached empty claim is retried.
func (handler *Handler) publishAgentSchedulingChanged(ctx context.Context, action db.DecisionAction) error {
	agentID, ok := decisionActionTarget(action)
	if !ok {
		return nil
	}
	agent, err := handler.Queries.GetAgent(ctx, agentID)
	if err != nil {
		return err
	}
	resp := handler.agentToResponse(agent)
	if err := handler.attachAgentSkills(ctx, &resp, agent.ID); err != nil {
		return err
	}
	eventType := protocol.EventAgentSchedulingPaused
	if action.ActionType == agentResumeAction {
		eventType = protocol.EventAgentSchedulingResumed
		handler.TaskService.NotifyRuntimeMayHaveWork(agent.RuntimeID, "")
	}
	handler.publish(eventType, uuidToString(agent.WorkspaceID), "member", uuidToString(action.ApprovedByUserID), map[string]any{"agent": broadcastAgentResponse(resp)})
	return nil
}

// agentBlastRadius is what a pause leaves waiting: work the agent holds now.
type agentBlastRadius struct{ running, queued, openIssues, wakeups int }

func (handler *Handler) agentBlastRadius(ctx context.Context, agentID pgtype.UUID) (agentBlastRadius, error) {
	var radius agentBlastRadius
	err := handler.DB.QueryRow(ctx, `
SELECT
  (SELECT count(*) FROM agent_task_queue WHERE agent_id = $1 AND status IN ('dispatched', 'running', 'waiting_local_directory')),
  (SELECT count(*) FROM agent_task_queue WHERE agent_id = $1 AND status IN ('queued', 'deferred')),
  (SELECT count(*) FROM issue WHERE assignee_type = 'agent' AND assignee_id = $1 AND status NOT IN ('done', 'cancelled', 'agent_accepted', 'merged')),
  (SELECT count(*) FROM issue_wakeup WHERE agent_id = $1 AND enabled AND disabled_at IS NULL)`, agentID).
		Scan(&radius.running, &radius.queued, &radius.openIssues, &radius.wakeups)
	return radius, err
}

// agentSchedulingPreview shows the approver the agent, its current state and
// the work a pause would leave waiting. Running runs finish; queued runs wait.
func (handler *Handler) agentSchedulingPreview(ctx context.Context, action db.DecisionAction, viewer pgtype.UUID) ([]ActionPreviewField, error) {
	var params decisionaction.AgentSchedulingParams
	if err := json.Unmarshal(action.Params, &params); err != nil {
		return nil, err
	}
	agent, err := handler.agentForAction(ctx, action.WorkspaceID, params.AgentID, viewer)
	if err != nil {
		return nil, err
	}
	paused := action.ActionType == agentPauseAction
	state := func(isPaused bool) string {
		if isPaused {
			return "Paused"
		}
		return "Running"
	}
	fields := []ActionPreviewField{
		{"Agent", agent.Name, agent.Name},
		{"Scheduling", state(agent.SchedulingPausedAt.Valid), state(paused)},
	}
	if !paused {
		return fields, nil
	}
	radius, err := handler.agentBlastRadius(ctx, agent.ID)
	if err != nil {
		return nil, err
	}
	return append(fields,
		ActionPreviewField{"Running runs (finish)", strconv.Itoa(radius.running), strconv.Itoa(radius.running)},
		ActionPreviewField{"Queued runs (wait)", strconv.Itoa(radius.queued), strconv.Itoa(radius.queued)},
		ActionPreviewField{"Open assigned issues", strconv.Itoa(radius.openIssues), strconv.Itoa(radius.openIssues)},
		ActionPreviewField{"Enabled wakeups", strconv.Itoa(radius.wakeups), strconv.Itoa(radius.wakeups)},
	), nil
}
