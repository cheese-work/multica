package handler

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Sub-workflow step (CHE-2031): the edge watch of an issue on a step with a
// sub_workflow spawns the step's child before it reads the join, and the join
// counts only the children of the step's stage.

// errSubWorkflowMissing: the step names a workflow this workspace does not have.
var errSubWorkflowMissing = errors.New("sub-workflow not found in this workspace")

// errSubWorkflowRefused: the child cannot be made as the workflow stands, and
// a retry would not change that (its start-step handler fails the assignee
// check). The watch logs it and stops asking.
var errSubWorkflowRefused = errors.New("sub-workflow child refused")

// subWorkflowHandoffResolver resolves the child's start-step handler with the
// admission a new issue gets (createHandoffResolver): the same handler rules,
// then the same assignee gate. No request exists on the edge path, so the invoke
// gate judges the human the parent acts for: its requester, else its creator
// when that is a member. A handler that person may not use refuses the spawn.
func (h *Handler) subWorkflowHandoffResolver(parent db.Issue) service.CreateHandoffResolver {
	originator := ""
	switch {
	case parent.RequestedByID.Valid:
		originator = uuidToString(parent.RequestedByID)
	case parent.CreatorType == "member":
		originator = uuidToString(parent.CreatorID)
	}
	workspaceID := uuidToString(parent.WorkspaceID)
	return func(ctx context.Context, step issueworkflow.Step, project db.Project) (pgtype.Text, pgtype.UUID, error) {
		humans, err := h.workflowHumans(ctx, step, project, project.WorkspaceID, parent.RequestedByID)
		if err != nil {
			return pgtype.Text{}, pgtype.UUID{}, err
		}
		assigneeType, assigneeID, ok := issueworkflow.ResolveHandler(step, project, parent.CreatorType, parent.CreatorID, humans)
		if !ok {
			return pgtype.Text{}, pgtype.UUID{}, nil
		}
		assignee := pgtype.Text{String: assigneeType, Valid: true}
		status, msg := h.validateAssignee(ctx, workspaceID, assignee, assigneeID, func(agent db.Agent) bool {
			return h.canInvokeAgent(ctx, agent, "system", "", originator, workspaceID)
		})
		if status != 0 {
			return pgtype.Text{}, pgtype.UUID{}, fmt.Errorf("%w: %s", errSubWorkflowRefused, msg)
		}
		return assignee, assigneeID, nil
	}
}

// subWorkflowTargetsMessage checks every sub-workflow step of a definition
// being saved: its workflow must exist in the workspace. It returns the 400
// message, or "" when the steps are fine.
func (h *Handler) subWorkflowTargetsMessage(ctx context.Context, qtx *db.Queries, workspaceID pgtype.UUID, steps []issueworkflow.Step) (string, error) {
	for _, s := range steps {
		if s.SubWorkflow == nil {
			continue
		}
		id, err := parseUUIDString(s.SubWorkflow.WorkflowID)
		if err != nil {
			return fmt.Sprintf("step %s: sub-workflow id is not a valid id", s.StatusKey), nil
		}
		if _, err := qtx.GetIssueWorkflow(ctx, db.GetIssueWorkflowParams{ID: id, WorkspaceID: workspaceID}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Sprintf("step %s: sub-workflow not found in this workspace", s.StatusKey), nil
			}
			return "", err
		}
	}
	return "", nil
}

// spawnSubWorkflowChild gives the issue the child its sub-workflow step starts:
// one child of the issue on the target workflow in the step's stage. A child
// already there (an earlier pass, a re-entry, one made by hand) is the child,
// so a second pass makes none. The child starts at the workflow's start status
// with that step's handler, admitted like any new issue.
func (h *Handler) spawnSubWorkflowChild(ctx context.Context, parent db.Issue, sub issueworkflow.SubWorkflow) error {
	targetID, err := parseUUIDString(sub.WorkflowID)
	if err != nil {
		return errSubWorkflowMissing
	}
	row, err := h.Queries.GetIssueWorkflow(ctx, db.GetIssueWorkflowParams{ID: targetID, WorkspaceID: parent.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return errSubWorkflowMissing
	}
	if err != nil {
		return err
	}
	children, err := h.Queries.ListChildIssues(ctx, parent.ID)
	if err != nil {
		return err
	}
	for _, c := range children {
		if c.WorkflowID == targetID && c.Stage.Valid && c.Stage.Int32 == int32(sub.Stage) {
			return nil
		}
	}
	def, err := issueworkflow.Decode(row)
	if err != nil {
		return err
	}
	status := issueworkflow.StartStatus
	if !def.Has(status) && len(def.Steps) > 0 {
		status = def.Steps[0].StatusKey
	}
	_, err = h.IssueService.Create(ctx, service.IssueCreateParams{
		WorkspaceID:    parent.WorkspaceID,
		Title:          parent.Title + " — " + def.Name,
		Status:         status,
		Priority:       parent.Priority,
		CreatorType:    parent.CreatorType,
		CreatorID:      parent.CreatorID,
		ParentIssueID:  parent.ID,
		ProjectID:      parent.ProjectID,
		WorkflowID:     targetID,
		Stage:          pgtype.Int4{Int32: int32(sub.Stage), Valid: true},
		AllowDuplicate: true,
		HandoffResolver: h.subWorkflowHandoffResolver(parent),
	}, service.IssueCreateOpts{
		ActorID: uuidToString(parent.CreatorID),
		BroadcastPayload: func(issue db.Issue, _ []db.Attachment, _ []db.IssueLabel) map[string]any {
			return map[string]any{"issue": h.issueResponseWithCategory(ctx, issue)}
		},
	})
	return err
}
