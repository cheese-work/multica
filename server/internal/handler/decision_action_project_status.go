package handler

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// projectStatusSetType is the action an agent proposes instead of writing a
// project status (CHE-9125).
const projectStatusSetType = "project.status.set"

// executeProjectStatusSetAction sets the project's status under the project
// lock the approval holds, with the audit row and run cancellation the member
// write has, and reads the new status back into the result.
func executeProjectStatusSetAction(ctx context.Context, execution decisionActionExecution) (json.RawMessage, error) {
	var params decisionaction.ProjectStatusSetParams
	if err := json.Unmarshal(execution.Action.Params, &params); err != nil {
		return nil, err
	}
	projectID, err := parseUUIDString(params.ProjectID)
	if err != nil {
		return nil, err
	}
	queries := execution.Queries
	previous, err := queries.GetProjectInWorkspace(ctx, db.GetProjectInWorkspaceParams{ID: projectID, WorkspaceID: execution.Action.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, actionFailure{Code: "project_missing", Message: "the project no longer exists in this workspace"}
	}
	if err != nil {
		return nil, err
	}
	updated, cancelled, err := service.ApplyProjectStatus(ctx, execution.Tx, queries, previous, params.Status)
	if err != nil {
		return nil, err
	}
	actor := auditActor{Type: "member", ID: execution.Approver.UserID, Source: "decision_action"}
	if err := insertAuditRow(ctx, queries, actor, auditInput{
		Kind: "project.update", WorkspaceID: updated.WorkspaceID, TargetID: updated.ID,
		Before: projectAuditFields(previous), After: projectAuditFields(updated),
	}); err != nil {
		return nil, err
	}
	taskIDs := make([]string, 0, len(cancelled))
	for _, task := range cancelled {
		taskIDs = append(taskIDs, uuidToString(task.ID))
	}
	return json.Marshal(map[string]any{
		"project_id": params.ProjectID, "from_status": previous.Status, "status": updated.Status, "cancelled_task_ids": taskIDs,
	})
}

// publishProjectStatusSet tells open clients the project changed and which
// runs the change ended.
func (handler *Handler) publishProjectStatusSet(ctx context.Context, action db.DecisionAction) error {
	var result struct {
		ProjectID        string   `json:"project_id"`
		CancelledTaskIDs []string `json:"cancelled_task_ids"`
	}
	if err := json.Unmarshal(action.Result, &result); err != nil {
		return err
	}
	projectID, err := parseUUIDString(result.ProjectID)
	if err != nil {
		return err
	}
	project, err := handler.Queries.GetProjectInWorkspace(ctx, db.GetProjectInWorkspaceParams{ID: projectID, WorkspaceID: action.WorkspaceID})
	if err != nil {
		return err
	}
	var cancelled []db.AgentTaskQueue
	for _, raw := range result.CancelledTaskIDs {
		id, err := parseUUIDString(raw)
		if err != nil {
			return err
		}
		task, err := handler.Queries.GetAgentTask(ctx, id)
		if err != nil {
			return err
		}
		cancelled = append(cancelled, task)
	}
	handler.broadcastCancelledWakeups(ctx, action.WorkspaceID, cancelled)
	resp := projectToResponse(project)
	resp.IssueCount, resp.DoneCount = handler.loadProjectIssueStats(ctx, project.WorkspaceID, project.ID)
	resp.ResourceCount = handler.loadProjectResourceCount(ctx, project.ID)
	handler.publish(protocol.EventProjectUpdated, uuidToString(action.WorkspaceID), "member", uuidToString(action.ApprovedByUserID), map[string]any{"project": resp})
	return nil
}

// projectStatusSetPreview shows the project and its status before and after.
func (handler *Handler) projectStatusSetPreview(ctx context.Context, action db.DecisionAction) ([]ActionPreviewField, error) {
	var params decisionaction.ProjectStatusSetParams
	if err := json.Unmarshal(action.Params, &params); err != nil {
		return nil, err
	}
	projectID, err := parseUUIDString(params.ProjectID)
	if err != nil {
		return nil, err
	}
	project, err := handler.Queries.GetProjectInWorkspace(ctx, db.GetProjectInWorkspaceParams{ID: projectID, WorkspaceID: action.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil // the executor ends the action with a failure
	}
	if err != nil {
		return nil, err
	}
	return []ActionPreviewField{
		{"Project", project.Title, project.Title},
		{"Status", project.Status, params.Status},
	}, nil
}
